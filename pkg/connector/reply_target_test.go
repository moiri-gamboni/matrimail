package connector

import (
	"strings"
	"testing"

	"github.com/Leicas/matrimail/pkg/email"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func msgRef(id string) *database.Message {
	return &database.Message{ID: networkid.MessageID("email:" + id)}
}

// Recipients are resolved from the thread's Last* fields, which describe the
// most recent inbound. A reply aimed anywhere else cannot be addressed safely.
func TestCheckReplyTargetResolvable(t *testing.T) {
	t.Parallel()
	thread := &email.EmailThread{
		LastInboundMessageID:  "newest-inbound@example.com",
		LastOutboundMessageID: "our-last-send@example.com",
		// Deliberately neither of the two IDs the guard accepts, so the case
		// below checks that the guard never falls back to MessageID.
		MessageID: "some-other-message@example.com",
	}

	for _, tc := range []struct {
		name    string
		replyTo *database.Message
		wantErr bool
	}{
		{"no explicit reply", nil, false},
		{"reply to the newest inbound", msgRef("newest-inbound@example.com"), false},
		{"reply to our own last send", msgRef("our-last-send@example.com"), false},
		{"reply to an older message", msgRef("three-messages-ago@example.com"), true},
		{"reply to the message MessageID names", msgRef("some-other-message@example.com"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReplyTargetResolvable(thread, tc.replyTo)
			if tc.wantErr && err == nil {
				t.Fatal("want refusal, got nil — the reply would have been addressed from the wrong message's recipients")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "older message") {
				t.Errorf("refusal should explain itself to the user; got %q", err)
			}
		})
	}
}

// A thread restored from metadata written before LastInboundMessageID existed
// has nothing to compare against. Refusing every reply in those rooms would be
// worse than the bug; fall through instead.
func TestCheckReplyTargetResolvable_LegacyThreadFallsThrough(t *testing.T) {
	t.Parallel()
	thread := &email.EmailThread{MessageID: "tail@example.com"}
	if err := checkReplyTargetResolvable(thread, msgRef("anything@example.com")); err != nil {
		t.Fatalf("legacy thread should not refuse: %v", err)
	}
}
