package control

import "github.com/aether-rtc/aether/internal/platform/database"

// PersonalSections lists this module's rows that are one person's, for their
// own export: the times they asked to control somebody's screen, and the
// times somebody asked to control theirs.
func PersonalSections(userID string) []database.Section {
	const cols = `meeting_id, controller_name, subject_name, scope, state, requested_at, decided_at, ended_at, reason`
	return []database.Section{
		{Name: "remote_control_you_asked_for", Query: `SELECT ` + cols + ` FROM control_sessions WHERE controller_id = ?`, Args: []any{userID}},
		{Name: "remote_control_of_your_screen", Query: `SELECT ` + cols + ` FROM control_sessions WHERE subject_id = ?`, Args: []any{userID}},
	}
}
