package identity

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The directory query, at fifty thousand accounts.
//
// A keystroke has to come back in microseconds, and a scan of the table
// cannot. Three indexed shapes, tried in order, each bounded by LIMIT so a
// common query touches only as many rows as it returns:
//
//  1. Name prefix: a range on the generated, lower-cased name_key index,
//     already in name order, so the first `limit` rows are the answer and
//     nothing is sorted.
//  2. Address prefix: the same on email_key.
//  3. Substring, for three or more characters: the trigram FTS5 table, which
//     finds "harm" in Sharma, bounded to ftsCandidateCap candidates before
//     the join and the sort. A three-letter run common to thousands of names
//     is the one case that could cost tens of milliseconds; the cap keeps it
//     to a few, at the price of "some Sharmas" rather than "the first twenty
//     alphabetically" -- and the next keystroke narrows it anyway.
//
// INDEXED BY is spelled out because, at a single-domain organisation, the
// planner otherwise picks the domain index (every row matches it) and
// evaluates the name predicate row by row: measured 59 ms for a one-letter
// prefix that way, microseconds this way.
//
// The scope is a fragment over the alias `u` (a domain list, an id list, or
// nothing), so the same function serves the organisation, the contacts, and
// the published-address-book modes.

const ftsCandidateCap = 500

// cleanQuery drops control characters (a NUL inside a bound string is an
// "unterminated string" to SQLite) and bounds the length.
func cleanQuery(q string) string {
	q = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, q)
	q = strings.TrimSpace(q)
	if len(q) > 120 {
		q = q[:120]
	}
	return q
}

// ftsPhrase quotes a query as one FTS5 phrase. Quotes are the only special
// characters inside a phrase and they are doubled, so nothing a person types
// can become an operator.
func ftsPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func (r *SQLRepository) directorySearch(ctx context.Context, scope string, scopeArgs []any, q string, limit int) ([]DirectoryEntry, error) {
	q = cleanQuery(q)
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	active := string(StatusActive)

	if q == "" {
		return r.directoryRows(ctx,
			`SELECT u.id, u.display_name, u.email FROM identity_users AS u
			  WHERE u.status = ?`+scope+` ORDER BY u.name_key, u.id LIMIT ?`,
			append(append([]any{active}, scopeArgs...), limit)...)
	}

	lo := strings.ToLower(q)
	hi := lo + "￿"
	out := make([]DirectoryEntry, 0, limit)
	seen := make(map[string]bool, limit)
	take := func(rows []DirectoryEntry) {
		for _, e := range rows {
			if len(out) >= limit {
				return
			}
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
	}

	rows, err := r.directoryRows(ctx,
		`SELECT u.id, u.display_name, u.email FROM identity_users AS u INDEXED BY identity_users_name_key
		  WHERE u.name_key >= ? AND u.name_key < ? AND u.status = ?`+scope+`
		  ORDER BY u.name_key, u.id LIMIT ?`,
		append(append([]any{lo, hi, active}, scopeArgs...), limit)...)
	if err != nil {
		return nil, err
	}
	take(rows)
	if len(out) >= limit {
		return out, nil
	}

	rows, err = r.directoryRows(ctx,
		`SELECT u.id, u.display_name, u.email FROM identity_users AS u INDEXED BY identity_users_email_key
		  WHERE u.email_key >= ? AND u.email_key < ? AND u.status = ?`+scope+`
		  ORDER BY u.email_key, u.id LIMIT ?`,
		append(append([]any{lo, hi, active}, scopeArgs...), limit)...)
	if err != nil {
		return nil, err
	}
	take(rows)
	if len(out) >= limit || utf8.RuneCountInString(q) < 3 {
		return out, nil
	}

	rows, err = r.directoryRows(ctx,
		// CROSS JOIN is not decoration: it pins the FTS candidates as the
		// outer loop. Left to itself the planner scanned identity_users by
		// the domain index (every row, at a one-domain company) and probed
		// the candidates per row -- 13 ms for a query with no match at all.
		`SELECT u.id, u.display_name, u.email
		   FROM (SELECT id FROM identity_users_fts WHERE identity_users_fts MATCH ? LIMIT ?) AS f
		   CROSS JOIN identity_users AS u ON u.id = f.id
		  WHERE u.status = ?`+scope+`
		  ORDER BY u.name_key, u.id LIMIT ?`,
		append(append([]any{ftsPhrase(q), ftsCandidateCap, active}, scopeArgs...), limit)...)
	if err != nil {
		return nil, err
	}
	take(rows)
	return out, nil
}

func (r *SQLRepository) directoryRows(ctx context.Context, sqlText string, args ...any) ([]DirectoryEntry, error) {
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	defer rows.Close()
	var out []DirectoryEntry
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.Email); err != nil {
			return nil, err
		}
		if e.Name == "" {
			e.Name = e.Email
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// inList is "?, ?, ?" and the arguments, for a bounded list.
func inList(vals []string) (string, []any) {
	holes := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		holes[i] = "?"
		args[i] = v
	}
	return strings.Join(holes, ","), args
}
