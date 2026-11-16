package notify

import "github.com/aether-rtc/aether/internal/platform/database"

// PersonalSections lists this module's rows that are one person's, for their
// own export: the browsers they let be notified. The keys are not listed;
// they are no use to their owner and are what a notice is encrypted to.
func PersonalSections(userID string) []database.Section {
	return []database.Section{{Name: "push_subscriptions",
		Query: `SELECT endpoint, created_at FROM notify_subscriptions WHERE user_id = ?`, Args: []any{userID}}}
}
