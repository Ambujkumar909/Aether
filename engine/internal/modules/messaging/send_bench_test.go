package messaging

import (
	"context"
	"fmt"
	"testing"
)

// One message into a big conversation: the server-side cost of the send
// path, which the 200-person stress run showed is where a burst queues
// (ARCHITECTURE 6br).
func BenchmarkSendInto200(b *testing.B)  { benchSend(b, 200) }
func BenchmarkSendInto5000(b *testing.B) { benchSend(b, 5000) }

func benchSend(b *testing.B, members int) {
	s := serviceForTest(&testing.T{})
	s.dir = fourDir{}
	ctx := context.Background()
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	c, err := s.CreateGroup(ctx, me, "big", []string{"other@example.com"})
	if err != nil {
		b.Fatal(err)
	}
	repo := s.repo.(*SQLRepository)
	var ms []Member
	for i := range members - 2 {
		ms = append(ms, Member{UserID: fmt.Sprint("u", i), Email: fmt.Sprint("u", i, "@example.com"), Role: RoleMember})
	}
	if err := repo.AddMembers(ctx, c.ID, ms); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := range b.N {
		if _, err := s.Send(ctx, me, c.ID, fmt.Sprint("message ", i)); err != nil {
			b.Fatal(err)
		}
	}
}
