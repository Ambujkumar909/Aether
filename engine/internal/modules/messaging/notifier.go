package messaging

// UseNotifier sets where notices for people with no socket open go
// (modules/notify, over Web Push); see notifyAbsent in service.go.
func (s *Service) UseNotifier(n Notifier) { s.notifier = n }
