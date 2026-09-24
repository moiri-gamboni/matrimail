package email

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func newOutboundTestProcessor(t *testing.T) (*Processor, *bridgev2.UserLogin) {
	t.Helper()
	log := zerolog.Nop()
	p := NewProcessor(&log, NewThreadManager(nil), false, "")
	p.SetAliasResolver(func(receiver string) []string {
		return []string{"me@example.com", "alias@example.com"}
	})
	login := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login-1"}}
	return p, login
}

// Which messages count as the user's own. A Sent mailbox or label says so, and
// so does a From address that is one of the account's own: a copy of the user's
// message can arrive through INBOX (mail to yourself, a list reflecting your
// post, Cc to self), and an IMAP Sent folder is not always named "Sent".
func TestProcessParsedEmail_ClassifiesOutbound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mailbox string
		from    string
		want    bool
	}{
		{"inbox from another party", "INBOX", "Alice <alice@example.com>", false},
		{"sent label", "SENT", "Me <me@example.com>", true},
		{"imap sent folder", "[Gmail]/Sent Mail", "me@example.com", true},
		{"sent label from an address not known as ours", "SENT", "old-address@example.com", true},
		{"inbox from our primary address", "INBOX", "Me <ME@Example.com>", true},
		{"inbox from a send-as alias", "INBOX", "alias@example.com", true},
		{"localised sent folder from our address", "Gesendet", "me@example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, login := newOutboundTestProcessor(t)
			msg, err := p.ProcessParsedEmail(context.Background(), &ParsedEmail{
				MessageID: "m-1@example.com",
				From:      tc.from,
				To:        []string{"bob@example.com"},
			}, login, tc.mailbox)
			if err != nil {
				t.Fatalf("ProcessParsedEmail: %v", err)
			}
			if msg.IsOutbound != tc.want {
				t.Errorf("IsOutbound = %v, want %v", msg.IsOutbound, tc.want)
			}
		})
	}
}

// End to end through the processor: the user answers from Gmail's web client,
// and the copy that arrives -- here through INBOX, with nothing but its From
// address to mark it -- must leave the thread answering the inbound.
func TestProcessParsedEmail_OwnMessageKeepsInboundReplyContext(t *testing.T) {
	t.Parallel()
	p, login := newOutboundTestProcessor(t)
	ctx := context.Background()

	in, err := p.ProcessParsedEmail(ctx, &ParsedEmail{
		MessageID: "in-1@example.com",
		From:      "Alice <alice@example.com>",
		To:        []string{"alias@example.com"},
	}, login, "INBOX")
	if err != nil {
		t.Fatalf("inbound: %v", err)
	}
	out, err := p.ProcessParsedEmail(ctx, &ParsedEmail{
		MessageID: "out-1@example.com",
		InReplyTo: "in-1@example.com",
		From:      "alias@example.com",
		To:        []string{"alice@example.com"},
	}, login, "INBOX")
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	if out.Thread != in.Thread {
		t.Fatalf("the reply landed in a different thread")
	}
	th := out.Thread
	if th.LastFrom != "Alice <alice@example.com>" || th.LastInboundMessageID != "in-1@example.com" {
		t.Errorf("reply context = from %q (%q); want Alice's inbound", th.LastFrom, th.LastInboundMessageID)
	}
	if th.LastDeliveredTo != "alias@example.com" {
		t.Errorf("LastDeliveredTo = %q; want the alias Alice wrote to", th.LastDeliveredTo)
	}
}
