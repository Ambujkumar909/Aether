package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func teamChannel(t *testing.T, tm *Team, title string) TeamChannel {
	t.Helper()
	for _, c := range tm.Channels {
		if c.Title == title {
			return c
		}
	}
	t.Fatalf("no channel %q in %+v", title, tm.Channels)
	return TeamChannel{}
}

func myTeam(t *testing.T, s *Service, a Actor, id string) *Team {
	t.Helper()
	ts, err := s.MyTeams(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.ID == id {
			return x
		}
	}
	return nil
}

// A team, its General channel, public joining, the organisation rule, and
// standard channels following the team.
func TestTeamCreateJoinAndChannels(t *testing.T) {
	s, me, you, third, out := channelService(t)
	ctx := context.Background()

	if _, err := s.CreateTeam(ctx, me, "   ", "", TeamPublic); !errors.Is(err, ErrTeamName) {
		t.Fatalf("a team with no name: %v", err)
	}
	tm, err := s.CreateTeam(ctx, me, "Design", "Everything we draw", TeamPublic)
	if err != nil {
		t.Fatal(err)
	}
	if tm.MyRole != RoleOwner || len(tm.Channels) != 1 || !tm.Channels[0].General {
		t.Fatalf("created: %+v", tm)
	}
	general := teamChannel(t, tm, GeneralChannel)

	// Browsing: the organisation sees it; an outsider does not.
	if ts, _ := s.BrowseTeams(ctx, you, "des", 10); len(ts) != 1 || ts[0].ID != tm.ID {
		t.Fatalf("browse by prefix: %+v", ts)
	}
	if ts, _ := s.BrowseTeams(ctx, out, "", 10); len(ts) != 0 {
		t.Fatalf("an outsider browsed the team: %+v", ts)
	}
	if _, err := s.JoinTeam(ctx, out, tm.ID); !errors.Is(err, ErrTeamPrivate) {
		t.Fatalf("an outsider joined: %v", err)
	}
	// The team's channels are not in the organisation-wide channel browser.
	if chs, _ := s.BrowseChannels(ctx, you, "", 50); len(chs) != 0 {
		t.Fatalf("team channels in the channel browser: %+v", chs)
	}

	// A standard channel, then someone joins: they are in both.
	std, err := s.CreateTeamChannel(ctx, me, tm.ID, "Reviews", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeamChannel(ctx, me, tm.ID, "reviews", "", false); !errors.Is(err, ErrChannelExists) {
		t.Fatalf("a duplicate name within the team: %v", err)
	}
	if _, err := s.JoinTeam(ctx, you, tm.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{general.ID, std.ID} {
		if ok, _ := s.repo.IsMember(ctx, id, you.ID); !ok {
			t.Fatalf("joining the team did not put you in %s", id)
		}
	}
	// A standard channel's membership follows the team.
	if _, err := s.JoinChannel(ctx, third, std.ID); !errors.Is(err, ErrTeamChannel) {
		t.Fatalf("joined a team's channel without the team: %v", err)
	}
	if err := s.Leave(ctx, you, std.ID); !errors.Is(err, ErrTeamChannel) {
		t.Fatalf("left a standard channel without leaving the team: %v", err)
	}
	if _, _, err := s.AddMembers(ctx, me, std.ID, []string{"third@example.com"}); !errors.Is(err, ErrTeamChannel) {
		t.Fatalf("added someone to a standard channel directly: %v", err)
	}

	// A private channel: the creator, then only people on the team.
	priv, err := s.CreateTeamChannel(ctx, me, tm.ID, "Leads", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.repo.IsMember(ctx, priv.ID, you.ID); ok {
		t.Fatal("a private channel took everyone on the team")
	}
	if got := myTeam(t, s, you, tm.ID); len(got.Channels) != 2 {
		t.Fatalf("you see the private channel you are not in: %+v", got.Channels)
	}
	_, skipped, err := s.AddMembers(ctx, me, priv.ID, []string{"third@example.com", "other@example.com"})
	if err != nil || len(skipped) != 1 || skipped[0] != "third@example.com" {
		t.Fatalf("adding to a private team channel: skipped %v err %v", skipped, err)
	}
	if ok, _ := s.repo.IsMember(ctx, priv.ID, you.ID); !ok {
		t.Fatal("a team member was not added to the private channel")
	}

	// General keeps its name and cannot be deleted.
	if err := s.DeleteChannel(ctx, me, general.ID); !errors.Is(err, ErrGeneralChannel) {
		t.Fatalf("deleted General: %v", err)
	}
	if _, err := s.Rename(ctx, me, general.ID, "Lobby", ""); !errors.Is(err, ErrGeneralChannel) {
		t.Fatalf("renamed General: %v", err)
	}

	// Leaving the team takes you out of every channel, the private one too.
	if err := s.LeaveTeam(ctx, you, tm.ID, you.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{general.ID, std.ID, priv.ID} {
		if ok, _ := s.repo.IsMember(ctx, id, you.ID); ok {
			t.Fatalf("still in %s after leaving the team", id)
		}
	}
	if myTeam(t, s, you, tm.ID) != nil {
		t.Fatal("the team is still in your list")
	}

	// Joining puts you in General and the standard channels -- never in a
	// private one.
	if _, err := s.JoinTeam(ctx, third, tm.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.repo.IsMember(ctx, std.ID, third.ID); !ok {
		t.Fatal("joining did not put third in the standard channel")
	}
	if ok, _ := s.repo.IsMember(ctx, priv.ID, third.ID); ok {
		t.Fatal("joining the team put third in a private channel")
	}
}

// Private teams: owners add people, codes let them in, and the rules of
// ownership hold.
func TestTeamPrivateCodesAndOwners(t *testing.T) {
	s, me, you, third, _ := channelService(t)
	ctx := context.Background()
	tm, err := s.CreateTeam(ctx, me, "Leadership", "", "anything not public is private")
	if err != nil || tm.Visibility != TeamPrivate {
		t.Fatalf("visibility defaults to private: %+v %v", tm, err)
	}
	if ts, _ := s.BrowseTeams(ctx, you, "", 10); len(ts) != 0 {
		t.Fatalf("a private team in the browser: %+v", ts)
	}
	if _, err := s.JoinTeam(ctx, you, tm.ID); !errors.Is(err, ErrTeamPrivate) {
		t.Fatalf("joined a private team: %v", err)
	}

	// Codes: owners only; the code lets anyone in; removed, it does not.
	if _, err := s.TeamCode(ctx, you, tm.ID, false); !errors.Is(err, ErrTeamNotOwner) {
		t.Fatalf("a non-owner made a code: %v", err)
	}
	code, err := s.TeamCode(ctx, me, tm.ID, false)
	if err != nil || len(code) != TeamCodeLen {
		t.Fatalf("code %q %v", code, err)
	}
	if _, err := s.JoinTeamByCode(ctx, you, "zzzzzzz"); !errors.Is(err, ErrTeamCode) {
		t.Fatalf("a wrong code: %v", err)
	}
	if _, err := s.JoinTeamByCode(ctx, you, "  "+strings.ToUpper(code)+" "); err != nil {
		t.Fatalf("the code, with spaces and in capitals: %v", err)
	}
	if myTeam(t, s, you, tm.ID) == nil {
		t.Fatal("not on the team after the code")
	}
	if got := myTeam(t, s, you, tm.ID); got.JoinCode != "" {
		t.Fatal("a member was shown the code")
	}
	if _, err := s.TeamCode(ctx, me, tm.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinTeamByCode(ctx, third, code); !errors.Is(err, ErrTeamCode) {
		t.Fatalf("a removed code still worked: %v", err)
	}

	// Owners add people; members do not.
	if _, err := s.AddTeamMembers(ctx, you, tm.ID, []string{"third@example.com"}); !errors.Is(err, ErrTeamNotOwner) {
		t.Fatalf("a member added someone: %v", err)
	}
	if skipped, err := s.AddTeamMembers(ctx, me, tm.ID, []string{"third@example.com", "out@elsewhere.example"}); err != nil || len(skipped) != 1 {
		t.Fatalf("owner adds: skipped %v err %v", skipped, err)
	}

	// The last owner cannot step down or leave while others remain.
	if err := s.SetTeamRole(ctx, me, tm.ID, me.ID, RoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the last owner stepped down: %v", err)
	}
	if err := s.LeaveTeam(ctx, me, tm.ID, me.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the last owner left: %v", err)
	}
	if err := s.SetTeamRole(ctx, you, tm.ID, third.ID, RoleOwner); !errors.Is(err, ErrTeamNotOwner) {
		t.Fatalf("a member made an owner: %v", err)
	}
	if err := s.SetTeamRole(ctx, me, tm.ID, you.ID, "admiral"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("an unknown role: %v", err)
	}
	if err := s.SetTeamRole(ctx, me, tm.ID, you.ID, RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveTeam(ctx, me, tm.ID, me.ID); err != nil {
		t.Fatalf("with another owner, the first may leave: %v", err)
	}
	// A member may not remove another; an owner may.
	if err := s.LeaveTeam(ctx, third, tm.ID, you.ID); !errors.Is(err, ErrTeamNotOwner) {
		t.Fatalf("a member removed an owner: %v", err)
	}
	if err := s.LeaveTeam(ctx, you, tm.ID, third.ID); err != nil {
		t.Fatal(err)
	}

	// Strangers see nothing of it.
	if _, err := s.TeamMembersOf(ctx, third, tm.ID); !errors.Is(err, ErrTeamNotMember) {
		t.Fatalf("a stranger listed the members: %v", err)
	}
	if _, err := s.CreateTeamChannel(ctx, third, tm.ID, "x", "", false); !errors.Is(err, ErrTeamNotMember) {
		t.Fatalf("a stranger made a channel: %v", err)
	}
	if _, err := s.TeamMembersOf(ctx, Actor{}, tm.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nobody listed the members: %v", err)
	}

	// Deleting: owners only, and every channel goes with it.
	ch, _ := s.CreateTeamChannel(ctx, you, tm.ID, "Plans", "", false)
	if err := s.DeleteTeam(ctx, third, tm.ID); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrTeamNotOwner) {
		t.Fatalf("a non-member deleted the team: %v", err)
	}
	if err := s.DeleteTeam(ctx, you, tm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.ConversationByID(ctx, ch.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a channel outlived its team: %v", err)
	}
}

// Bounds one over, and every list index-backed.
func TestTeamBoundsAndIndexes(t *testing.T) {
	s, me, you, _, _ := channelService(t)
	ctx := context.Background()
	repo := s.repo.(*SQLRepository)
	tm, _ := s.CreateTeam(ctx, me, strings.Repeat("n", MaxTeamName+20), strings.Repeat("d", MaxTeamDesc+20), TeamPublic)
	if len([]rune(tm.Name)) != MaxTeamName || len([]rune(tm.Description)) != MaxTeamDesc {
		t.Fatalf("name %d, description %d", len(tm.Name), len(tm.Description))
	}

	// Channels: General plus up to the bound, then one over.
	for i := 1; i < MaxTeamChannels; i++ {
		if _, err := s.CreateTeamChannel(ctx, me, tm.ID, fmt.Sprint("c", i), "", true); err != nil {
			t.Fatalf("channel %d: %v", i, err)
		}
	}
	if _, err := s.CreateTeamChannel(ctx, me, tm.ID, "one more", "", true); !errors.Is(err, ErrTeamChannels) {
		t.Fatalf("channel %d: %v", MaxTeamChannels+1, err)
	}

	// Members: fill to one under the bound, then the last fits and the next does not.
	now := time.Now().UnixMilli()
	if _, err := repo.db.ExecContext(ctx, `
		WITH RECURSIVE k(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM k WHERE n < ?)
		INSERT INTO messaging_team_members (team_id, user_id, email, name, role, joined_at)
		SELECT ?, 'fill-'||n, 'fill'||n||'@example.com', '', 'member', ? FROM k`, MaxTeamMembers-2, tm.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE messaging_teams SET member_count = ? WHERE id = ?`, MaxTeamMembers-1, tm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinTeam(ctx, you, tm.ID); err != nil {
		t.Fatalf("member %d: %v", MaxTeamMembers, err)
	}
	third := Actor{ID: "u-third", Email: "third@example.com", Name: "Third"}
	if _, err := s.JoinTeam(ctx, third, tm.ID); !errors.Is(err, ErrTeamFull) {
		t.Fatalf("member %d: %v", MaxTeamMembers+1, err)
	}

	for _, q := range []string{
		`SELECT t.id FROM messaging_team_members m JOIN messaging_teams t ON t.id = m.team_id WHERE m.user_id = 'u' ORDER BY t.name_key, t.id`,
		`SELECT id FROM messaging_teams t WHERE t.visibility = 'public' AND t.name_key >= 'a' AND t.name_key < 'b' ORDER BY t.name_key`,
		`SELECT c.id FROM messaging_conversations c WHERE c.team_id IN ('t1','t2') AND c.kind = 'channel'`,
		`SELECT 1 FROM messaging_teams WHERE join_code = 'abcdefg'`,
		`SELECT role FROM messaging_team_members WHERE team_id = 't' AND user_id = 'u'`,
	} {
		rows, err := repo.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var a, b, c int
			var d string
			_ = rows.Scan(&a, &b, &c, &d)
			plan += d + "; "
		}
		rows.Close()
		if strings.Contains(plan, "SCAN t ") || strings.Contains(plan, "SCAN m ") || strings.Contains(plan, "SCAN c ") || strings.Contains(plan, "SCAN messaging") {
			t.Fatalf("not index-backed: %s -> %s", q, plan)
		}
	}
}
