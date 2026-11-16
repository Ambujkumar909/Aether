package database

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A person's own data, handed to them.
//
// Each module knows which of its rows are one person's and says so as a list
// of Sections: a name and a query that selects only that person's rows. The
// export is those sections written as one JSON object of arrays, a row at a
// time, so its memory is one row however much the person has written. The
// modules stay the owners of their tables; nothing here knows a table's
// name.

// Section is one named list in a person's export.
type Section struct {
	Name  string
	Query string // selects that person's rows only; its column names are the keys
	Args  []any
}

// isTime: columns that hold a time in milliseconds, by the names this schema
// uses for them. A reader of their own export should not have to know that.
func isTime(col string) bool {
	switch col {
	case "at", "until", "last_seen", "last_read", "starts", "ends":
		return true
	}
	return strings.HasSuffix(col, "_at")
}

// WriteSections writes {"name":[{"column":value,...},...],...} without the
// outer braces, so the caller can put fields of its own in front: each
// section is preceded by a comma.
func (db *DB) WriteSections(ctx context.Context, w io.Writer, sections []Section) error {
	for _, s := range sections {
		name, _ := json.Marshal(s.Name)
		if _, err := fmt.Fprintf(w, ",\n%s:[", name); err != nil {
			return err
		}
		if err := db.writeRows(ctx, w, s); err != nil {
			return fmt.Errorf("export %s: %w", s.Name, err)
		}
		if _, err := io.WriteString(w, "]"); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) writeRows(ctx context.Context, w io.Writer, s Section) error {
	rows, err := db.QueryContext(ctx, s.Query, s.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	keys := make([][]byte, len(cols))
	for i, c := range cols {
		keys[i], _ = json.Marshal(c)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	first := true
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		var b strings.Builder
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("\n{")
		for i, v := range vals {
			if i > 0 {
				b.WriteString(",")
			}
			b.Write(keys[i])
			b.WriteString(":")
			switch x := v.(type) {
			case []byte:
				v = string(x)
			case int64:
				if isTime(cols[i]) {
					if x > 0 {
						v = time.UnixMilli(x).UTC().Format(time.RFC3339)
					} else {
						v = nil
					}
				}
			}
			j, err := json.Marshal(v)
			if err != nil {
				return err
			}
			b.Write(j)
		}
		b.WriteString("}")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
	}
	return rows.Err()
}
