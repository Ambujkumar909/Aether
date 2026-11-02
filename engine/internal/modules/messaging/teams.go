package messaging

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Teams: the containers above channels (Teams' words: "Create team", "Give
// your team a name", "Public - Anyone in your organization can join",
// "Private - Only team owners can add members", "Join a team with a code",
// "Generate team code", "Manage team", "Leave team", "Delete team",
// "Standard - Everyone on the team has access").
//
// A team has owners and members. Every team has a General channel. Joining a
// team puts you in General and every standard channel; leaving takes you out
// of all of its channels, private ones included. A private channel's members
// are chosen, and must be on the team. The channels are ordinary channels
// underneath -- the same messages, pushes, files and unread counts.
//
// Who sees what. A public team can be browsed and joined by anybody who could
// message its creator (the organisation rule, as for channels). A private
// team is invisible to non-members and entered by an owner adding you, or
// with its code. Codes are seven characters and can be reset or removed.

const (
	TeamPublic  = "public"
	TeamPrivate = "private"

	MaxTeamName     = 80
	MaxTeamDesc     = 1024
	MaxTeamMembers  = 5000
	MaxTeamChannels = 200
	TeamCodeLen     = 7
	GeneralChannel  = "General"
)

var (
	ErrTeamName       = errors.New("messaging: a team needs a name")
	ErrTeamNotOwner   = errors.New("messaging: only team owners can do that")
	ErrTeamNotMember  = errors.New("messaging: you are not on this team")
	ErrTeamPrivate    = errors.New("messaging: that team is private")
	ErrTeamCode       = errors.New("messaging: no team has that code")
	ErrTeamFull       = errors.New("messaging: this team has reached its limit of members")
	ErrTeamChannels   = errors.New("messaging: this team has reached its limit of channels")
	ErrTeamChannel    = errors.New("messaging: membership of this channel follows the team")
	ErrGeneralChannel = errors.New("messaging: the General channel cannot be deleted")
	ErrNotOnTeam      = errors.New("messaging: only people on the team can be added to its channels")
	ErrLastOwner      = errors.New("messaging: a team needs an owner; make someone else an owner first")
)

type Team struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Visibility  string        `json:"visibility"`
	MemberCount int           `json:"memberCount"`
	CreatedAt   time.Time     `json:"createdAt"`
	MyRole      string        `json:"myRole,omitempty"`
	JoinCode    string        `json:"joinCode,omitempty"` // owners only
	Channels    []TeamChannel `json:"channels,omitempty"`

	createdBy string
	joinCode  string
}

// TeamChannel is a channel as the team list shows it.
type TeamChannel struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Visibility string `json:"visibility"` // public = standard, private
	General    bool   `json:"general,omitempty"`
}

type TeamMember struct {
	UserID   string    `json:"userId"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// --- storage ----------------------------------------------------------------

const teamCols = `id, name, description, visibility, member_count, created_at, created_by, COALESCE(join_code, '')`

// teamColsT: the same, for queries that join and name the table t.
const teamColsT = `t.id, t.name, t.description, t.visibility, t.member_count, t.created_at, t.created_by, COALESCE(t.join_code, '')`

func scanTeam(s rowScanner) (*Team, error) {
	var t Team
	var created int64
	if err := s.Scan(&t.ID, &t.Name, &t.Description, &t.Visibility, &t.MemberCount, &created, &t.createdBy, &t.joinCode); err != nil {
		return nil, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	return &t, nil
}

func (r *SQLRepository) CreateTeam(ctx context.Context, t *Team, owner TeamMember) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO messaging_teams (id, name, description, visibility, join_code, created_by, created_at, member_count) VALUES (?,?,?,?,NULL,?,?,1)`,
		t.ID, t.Name, t.Description, t.Visibility, t.createdBy, t.CreatedAt.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messaging_team_members (team_id, user_id, email, name, role, joined_at) VALUES (?,?,?,?,?,?)`,
		t.ID, owner.UserID, owner.Email, owner.Name, RoleOwner, owner.JoinedAt.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLRepository) TeamByID(ctx context.Context, id string) (*Team, error) {
	t, err := scanTeam(r.db.QueryRowContext(ctx, `SELECT `+teamCols+` FROM messaging_teams WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (r *SQLRepository) TeamByCode(ctx context.Context, code string) (*Team, error) {
	t, err := scanTeam(r.db.QueryRowContext(ctx, `SELECT `+teamCols+` FROM messaging_teams WHERE join_code = ?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTeamCode
	}
	return t, err
}

// TeamRole is the user's role on the team, "" if not on it.
func (r *SQLRepository) TeamRole(ctx context.Context, teamID, userID string) (string, error) {
	var role string
	err := r.db.QueryRowContext(ctx, `SELECT role FROM messaging_team_members WHERE team_id = ? AND user_id = ?`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// MyTeams lists the user's teams by name, with their role: one query.
func (r *SQLRepository) MyTeams(ctx context.Context, userID string) ([]*Team, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+teamColsT+`, m.role FROM messaging_team_members m
		  JOIN messaging_teams t ON t.id = m.team_id
		 WHERE m.user_id = ? ORDER BY t.name_key, t.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Team
	for rows.Next() {
		var t Team
		var created int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Visibility, &t.MemberCount, &created, &t.createdBy, &t.joinCode, &t.MyRole); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, &t)
	}
	return out, rows.Err()
}

// TeamChannelsFor lists, for several teams at once, the channels this user
// can see: every standard channel, and the private ones they are in. One
// query however many teams.
func (r *SQLRepository) TeamChannelsFor(ctx context.Context, teamIDs []string, userID string) (map[string][]TeamChannel, error) {
	out := map[string][]TeamChannel{}
	if len(teamIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(teamIDs)), ",")
	args := []any{}
	for _, id := range teamIDs {
		args = append(args, id)
	}
	args = append(args, string(KindChannel), userID)
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.team_id, c.id, c.title, c.visibility FROM messaging_conversations c
		 WHERE c.team_id IN (`+ph+`) AND c.kind = ?
		   AND (c.visibility = 'public' OR EXISTS (SELECT 1 FROM messaging_members m WHERE m.conversation_id = c.id AND m.user_id = ?))
		 ORDER BY c.team_id, c.title_key <> 'general', c.title_key`, args...) // General first, as in Teams
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var team string
		var ch TeamChannel
		if err := rows.Scan(&team, &ch.ID, &ch.Title, &ch.Visibility); err != nil {
			return nil, err
		}
		ch.General = ch.Title == GeneralChannel
		out[team] = append(out[team], ch)
	}
	return out, rows.Err()
}

// TeamConversations: every channel of the team (for membership changes).
func (r *SQLRepository) TeamConversations(ctx context.Context, teamID string) ([]*Conversation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+convCols+` FROM messaging_conversations WHERE team_id = ? AND kind = ?`, teamID, string(KindChannel))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *SQLRepository) PublicTeams(ctx context.Context, userID, q string, limit int) ([]*Team, error) {
	args := []any{userID, TeamPublic}
	where := ""
	if q != "" {
		lo := strings.ToLower(q)
		where = ` AND t.name_key >= ? AND t.name_key < ?`
		args = append(args, lo, lo+"￿")
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+teamColsT+`, COALESCE((SELECT role FROM messaging_team_members m WHERE m.team_id = t.id AND m.user_id = ?), '')
		  FROM messaging_teams t WHERE t.visibility = ?`+where+` ORDER BY t.name_key LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Team
	for rows.Next() {
		var t Team
		var created int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.Visibility, &t.MemberCount, &created, &t.createdBy, &t.joinCode, &t.MyRole); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (r *SQLRepository) TeamMembers(ctx context.Context, teamID string, limit int) ([]TeamMember, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT user_id, email, name, role, joined_at FROM messaging_team_members WHERE team_id = ? ORDER BY joined_at, user_id LIMIT ?`, teamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		var at int64
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role, &at); err != nil {
			return nil, err
		}
		m.JoinedAt = time.UnixMilli(at).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddTeamMember adds one person (if not already there) within the bound,
// in one statement, and counts them.
func (r *SQLRepository) AddTeamMember(ctx context.Context, teamID string, m TeamMember) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_team_members (team_id, user_id, email, name, role, joined_at)
		SELECT ?,?,?,?,?,? WHERE (SELECT member_count FROM messaging_teams WHERE id = ?) < ?
		ON CONFLICT DO NOTHING`, teamID, m.UserID, m.Email, m.Name, m.Role, m.JoinedAt.UnixMilli(), teamID, MaxTeamMembers)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		_, err = r.db.ExecContext(ctx, `UPDATE messaging_teams SET member_count = member_count + 1 WHERE id = ?`, teamID)
	}
	return n == 1, err
}

func (r *SQLRepository) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM messaging_team_members WHERE team_id = ? AND user_id = ?`, teamID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		_, err = r.db.ExecContext(ctx, `UPDATE messaging_teams SET member_count = member_count - 1 WHERE id = ?`, teamID)
	}
	return err
}

func (r *SQLRepository) SetTeamRole(ctx context.Context, teamID, userID, role string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE messaging_team_members SET role = ? WHERE team_id = ? AND user_id = ?`, role, teamID, userID)
	return err
}

func (r *SQLRepository) TeamOwnerCount(ctx context.Context, teamID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_team_members WHERE team_id = ? AND role = ?`, teamID, RoleOwner).Scan(&n)
	return n, err
}

func (r *SQLRepository) UpdateTeam(ctx context.Context, t *Team) error {
	_, err := r.db.ExecContext(ctx, `UPDATE messaging_teams SET name = ?, description = ?, visibility = ? WHERE id = ?`, t.Name, t.Description, t.Visibility, t.ID)
	return err
}

func (r *SQLRepository) SetTeamCode(ctx context.Context, teamID, code string) error {
	var v any
	if code != "" {
		v = code
	}
	_, err := r.db.ExecContext(ctx, `UPDATE messaging_teams SET join_code = ? WHERE id = ?`, v, teamID)
	return err
}

func (r *SQLRepository) DeleteTeam(ctx context.Context, teamID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM messaging_teams WHERE id = ?`, teamID)
	return err
}

func (r *SQLRepository) CountTeamChannels(ctx context.Context, teamID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_conversations WHERE team_id = ? AND kind = ?`, teamID, string(KindChannel)).Scan(&n)
	return n, err
}

// --- service ----------------------------------------------------------------

func cleanVisibility(v string) string {
	if v == TeamPublic {
		return TeamPublic
	}
	return TeamPrivate
}

// newCode: seven characters from an alphabet without look-alikes.
func newCode() string {
	const abc = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, TeamCodeLen)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = abc[int(b[i])%len(abc)]
	}
	return string(b)
}

func (s *Service) teamRole(ctx context.Context, teamID, userID string) (string, error) {
	if userID == "" {
		return "", ErrForbidden
	}
	return s.repo.TeamRole(ctx, teamID, userID)
}

// CreateTeam makes a team with its creator as owner and a General channel.
func (s *Service) CreateTeam(ctx context.Context, a Actor, name, description, visibility string) (*Team, error) {
	name = cleanTitle(name, MaxTeamName)
	if name == "" {
		return nil, ErrTeamName
	}
	now := time.Now().UTC()
	t := &Team{ID: newID(), Name: name, Description: cleanTitle(description, MaxTeamDesc), Visibility: cleanVisibility(visibility), CreatedAt: now, MemberCount: 1, MyRole: RoleOwner, createdBy: a.ID}
	if err := s.repo.CreateTeam(ctx, t, TeamMember{UserID: a.ID, Email: a.Email, Name: a.Name, Role: RoleOwner, JoinedAt: now}); err != nil {
		return nil, err
	}
	general := &Conversation{
		ID: newID(), Kind: KindChannel, Title: GeneralChannel, Visibility: VisibilityPublic, TeamID: t.ID,
		CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members: []Member{{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now, Role: RoleOwner}},
	}
	if err := s.repo.CreateConversation(ctx, general); err != nil {
		return nil, err
	}
	s.system(ctx, a, general.ID, fmt.Sprintf("created the team %s", name))
	t.Channels = []TeamChannel{{ID: general.ID, Title: GeneralChannel, Visibility: VisibilityPublic, General: true}}
	return t, nil
}

// MyTeams: the teams list, each with the channels this person can see.
func (s *Service) MyTeams(ctx context.Context, a Actor) ([]*Team, error) {
	teams, err := s.repo.MyTeams(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	chans, err := s.repo.TeamChannelsFor(ctx, ids, a.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		t.Channels = chans[t.ID]
		if t.MyRole == RoleOwner {
			t.JoinCode = t.joinCode
		}
	}
	if teams == nil {
		teams = []*Team{}
	}
	return teams, nil
}

// BrowseTeams: public teams of the organisation, by name.
func (s *Service) BrowseTeams(ctx context.Context, a Actor, q string, limit int) ([]*Team, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.repo.PublicTeams(ctx, a.ID, strings.TrimSpace(q), limit*2)
	if err != nil {
		return nil, err
	}
	out := []*Team{}
	for _, t := range rows {
		if t.MyRole == "" && !s.mayReachUser(ctx, a, t.createdBy) {
			continue
		}
		out = append(out, t)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// join puts somebody on the team and in its General and standard channels.
func (s *Service) join(ctx context.Context, a Actor, t *Team, who TeamMember, announce string) error {
	added, err := s.repo.AddTeamMember(ctx, t.ID, who)
	if err != nil {
		return err
	}
	if !added {
		if role, _ := s.repo.TeamRole(ctx, t.ID, who.UserID); role != "" {
			return nil // already on it
		}
		return ErrTeamFull
	}
	convs, err := s.repo.TeamConversations(ctx, t.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, c := range convs {
		if c.Visibility != VisibilityPublic {
			continue
		}
		if ok, _ := s.repo.IsMember(ctx, c.ID, who.UserID); ok {
			continue
		}
		if err := s.repo.AddMembers(ctx, c.ID, []Member{{UserID: who.UserID, Email: who.Email, Name: who.Name, JoinedAt: now, LastRead: now, Role: RoleMember}}); err != nil {
			return err
		}
		if c.Title == GeneralChannel && announce != "" {
			s.system(ctx, a, c.ID, announce)
		}
	}
	return nil
}

// JoinTeam: a public team, by anybody in the organisation.
func (s *Service) JoinTeam(ctx context.Context, a Actor, teamID string) (*Team, error) {
	t, err := s.repo.TeamByID(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if t.Visibility != TeamPublic || !s.mayReachUser(ctx, a, t.createdBy) {
		return nil, ErrTeamPrivate
	}
	if err := s.join(ctx, a, t, TeamMember{UserID: a.ID, Email: a.Email, Name: a.Name, Role: RoleMember, JoinedAt: time.Now().UTC()}, "joined the team"); err != nil {
		return nil, err
	}
	return s.teamFor(ctx, a, teamID)
}

// JoinTeamByCode: any team, public or private, with its code.
func (s *Service) JoinTeamByCode(ctx context.Context, a Actor, code string) (*Team, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	if len(code) != TeamCodeLen {
		return nil, ErrTeamCode
	}
	t, err := s.repo.TeamByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if err := s.join(ctx, a, t, TeamMember{UserID: a.ID, Email: a.Email, Name: a.Name, Role: RoleMember, JoinedAt: time.Now().UTC()}, "joined the team with a code"); err != nil {
		return nil, err
	}
	return s.teamFor(ctx, a, t.ID)
}

func (s *Service) teamFor(ctx context.Context, a Actor, teamID string) (*Team, error) {
	teams, err := s.MyTeams(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		if t.ID == teamID {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

// AddTeamMembers: owners add people (who must be reachable), up to the bound.
func (s *Service) AddTeamMembers(ctx context.Context, a Actor, teamID string, emails []string) ([]string, error) {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return nil, err
	}
	if role != RoleOwner {
		return nil, ErrTeamNotOwner
	}
	t, err := s.repo.TeamByID(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if len(emails) > MaxAddAtOnce {
		emails = emails[:MaxAddAtOnce]
	}
	var skipped []string
	seen := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		if !s.reachable(ctx, a, e) {
			skipped = append(skipped, e)
			continue
		}
		uid, name, err := s.dir.LookupByEmail(ctx, e)
		if err != nil {
			skipped = append(skipped, e)
			continue
		}
		if r, _ := s.repo.TeamRole(ctx, t.ID, uid); r != "" {
			continue // already on it: nothing happened to them
		}
		if err := s.join(ctx, a, t, TeamMember{UserID: uid, Email: e, Name: name, Role: RoleMember, JoinedAt: time.Now().UTC()}, "added "+nameOr(name, e)+" to the team"); err != nil {
			if errors.Is(err, ErrTeamFull) {
				return skipped, err
			}
			return skipped, err
		}
		if uid != a.ID {
			s.recordActivity(ctx, []activityRow{{id: newID(), userID: uid, kind: ActivityTeam, actorID: a.ID, actorName: a.Name, teamID: t.ID, at: time.Now().UTC()}})
		}
	}
	return skipped, nil
}

func nameOr(name, email string) string {
	if name != "" {
		return name
	}
	return email
}

// LeaveTeam takes somebody off a team and out of all its channels. Owners
// may remove anyone; anybody may remove themselves. The last owner may not
// leave while others remain.
func (s *Service) LeaveTeam(ctx context.Context, a Actor, teamID, userID string) error {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return err
	}
	if role == "" {
		return ErrTeamNotMember
	}
	if userID != a.ID && role != RoleOwner {
		return ErrTeamNotOwner
	}
	theirs, err := s.repo.TeamRole(ctx, teamID, userID)
	if err != nil {
		return err
	}
	if theirs == "" {
		return ErrNotFound
	}
	t, err := s.repo.TeamByID(ctx, teamID)
	if err != nil {
		return err
	}
	if theirs == RoleOwner && t.MemberCount > 1 {
		if n, _ := s.repo.TeamOwnerCount(ctx, teamID); n <= 1 {
			return ErrLastOwner
		}
	}
	convs, err := s.repo.TeamConversations(ctx, teamID)
	if err != nil {
		return err
	}
	for _, c := range convs {
		if ok, _ := s.repo.IsMember(ctx, c.ID, userID); !ok {
			continue
		}
		if err := s.repo.RemoveMember(ctx, c.ID, userID); err != nil {
			return err
		}
		if c.Title == GeneralChannel {
			if userID == a.ID {
				s.system(ctx, a, c.ID, "left the team")
			} else {
				s.system(ctx, a, c.ID, "removed someone from the team")
			}
		}
	}
	if err := s.repo.RemoveTeamMember(ctx, teamID, userID); err != nil {
		return err
	}
	if t.MemberCount <= 1 {
		return s.deleteTeam(ctx, t, convs)
	}
	return nil
}

// SetTeamRole: owners make members owners and owners members.
func (s *Service) SetTeamRole(ctx context.Context, a Actor, teamID, userID, role string) error {
	mine, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return err
	}
	if mine != RoleOwner {
		return ErrTeamNotOwner
	}
	if role != RoleOwner && role != RoleMember {
		return ErrBadRequest
	}
	theirs, err := s.repo.TeamRole(ctx, teamID, userID)
	if err != nil {
		return err
	}
	if theirs == "" {
		return ErrNotFound
	}
	if theirs == RoleOwner && role == RoleMember {
		if n, _ := s.repo.TeamOwnerCount(ctx, teamID); n <= 1 {
			return ErrLastOwner
		}
	}
	return s.repo.SetTeamRole(ctx, teamID, userID, role)
}

// UpdateTeam: owners rename, describe, and set public or private.
func (s *Service) UpdateTeam(ctx context.Context, a Actor, teamID string, name, description, visibility *string) (*Team, error) {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return nil, err
	}
	if role != RoleOwner {
		return nil, ErrTeamNotOwner
	}
	t, err := s.repo.TeamByID(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if name != nil {
		n := cleanTitle(*name, MaxTeamName)
		if n == "" {
			return nil, ErrTeamName
		}
		t.Name = n
	}
	if description != nil {
		t.Description = cleanTitle(*description, MaxTeamDesc)
	}
	if visibility != nil {
		t.Visibility = cleanVisibility(*visibility)
	}
	if err := s.repo.UpdateTeam(ctx, t); err != nil {
		return nil, err
	}
	return s.teamFor(ctx, a, teamID)
}

// TeamCode: owners generate (or reset) the code, or remove it.
func (s *Service) TeamCode(ctx context.Context, a Actor, teamID string, remove bool) (string, error) {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return "", err
	}
	if role != RoleOwner {
		return "", ErrTeamNotOwner
	}
	if remove {
		return "", s.repo.SetTeamCode(ctx, teamID, "")
	}
	for i := 0; i < 5; i++ {
		code := newCode()
		if err := s.repo.SetTeamCode(ctx, teamID, code); err == nil {
			return code, nil
		} else if !isUniqueViolation(err) {
			return "", err
		}
	}
	return "", errors.New("messaging: could not make a unique code")
}

// TeamMembersOf: anyone on the team sees who else is.
func (s *Service) TeamMembersOf(ctx context.Context, a Actor, teamID string) ([]TeamMember, error) {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrTeamNotMember
	}
	ms, err := s.repo.TeamMembers(ctx, teamID, MaxTeamMembers)
	if ms == nil {
		ms = []TeamMember{}
	}
	return ms, err
}

// DeleteTeam: owners only; every channel goes with it.
func (s *Service) DeleteTeam(ctx context.Context, a Actor, teamID string) error {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return err
	}
	if role != RoleOwner {
		return ErrTeamNotOwner
	}
	t, err := s.repo.TeamByID(ctx, teamID)
	if err != nil {
		return err
	}
	convs, err := s.repo.TeamConversations(ctx, teamID)
	if err != nil {
		return err
	}
	return s.deleteTeam(ctx, t, convs)
}

func (s *Service) deleteTeam(ctx context.Context, t *Team, convs []*Conversation) error {
	for _, c := range convs {
		atts, err := s.repo.DeleteConversation(ctx, c.ID)
		if err != nil {
			return err
		}
		if s.files != nil {
			for _, at := range atts {
				_ = s.files.Remove(at.Blob)
			}
		}
	}
	return s.repo.DeleteTeam(ctx, t.ID)
}

// CreateTeamChannel: any member makes a standard channel (everybody on the
// team is in it) or a private one (the creator, as its owner).
func (s *Service) CreateTeamChannel(ctx context.Context, a Actor, teamID, name, topic string, private bool) (*Conversation, error) {
	role, err := s.teamRole(ctx, teamID, a.ID)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrTeamNotMember
	}
	name = strings.TrimPrefix(cleanTitle(name, MaxChannelName), "#")
	if name == "" {
		return nil, ErrBadName
	}
	if n, err := s.repo.CountTeamChannels(ctx, teamID); err != nil {
		return nil, err
	} else if n >= MaxTeamChannels {
		return nil, ErrTeamChannels
	}
	now := time.Now().UTC()
	c := &Conversation{
		ID: newID(), Kind: KindChannel, Title: name, Topic: cleanTitle(topic, MaxChannelTopic), TeamID: teamID,
		Visibility: VisibilityPublic, CreatedBy: a.ID, CreatedAt: now, LastMessageAt: now,
		Members: []Member{{UserID: a.ID, Email: a.Email, Name: a.Name, JoinedAt: now, LastRead: now, Role: RoleOwner}},
	}
	if private {
		c.Visibility = VisibilityPrivate
	} else {
		ms, err := s.repo.TeamMembers(ctx, teamID, MaxTeamMembers)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if m.UserID != a.ID {
				c.Members = append(c.Members, Member{UserID: m.UserID, Email: m.Email, Name: m.Name, JoinedAt: now, LastRead: now, Role: RoleMember})
			}
		}
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrChannelExists
		}
		return nil, err
	}
	s.system(ctx, a, c.ID, fmt.Sprintf("created the channel %s", name))
	return s.decorate(ctx, a, c)
}

// teamChannelRule decides what may be done to a team's channel's membership:
// a standard channel's follows the team; a private one's must stay on it.
func (s *Service) teamChannelRule(ctx context.Context, c *Conversation, addingUserID string) error {
	if c.TeamID == "" {
		return nil
	}
	if c.Visibility == VisibilityPublic {
		return ErrTeamChannel
	}
	if addingUserID != "" {
		role, err := s.repo.TeamRole(ctx, c.TeamID, addingUserID)
		if err != nil {
			return err
		}
		if role == "" {
			return ErrNotOnTeam
		}
	}
	return nil
}
