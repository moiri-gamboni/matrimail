package email

import (
	"context"
	"errors"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"testing"
)

type gmailResolver struct{ err error }

func (gmailResolver) ResolveThreadID(receiver, messageID string) (string, bool) {
	return "wrong-room", true
}
func (r gmailResolver) ResolveGmailThreadID(receiver, gmailID string) (string, bool, error) {
	if r.err != nil {
		return "", false, r.err
	}
	if gmailID == "existing" {
		return "persisted-room", true, nil
	}
	return "", false, nil
}

func TestGmailThreadOverridesForwardAndReplyHeaders(t *testing.T) {
	for _, tc := range []struct{ id, want string }{{"existing", "persisted-room"}, {"new", "new-message@example.com"}} {
		tm := NewThreadManager(gmailResolver{})
		got, err := tm.DetermineThread("me", &ParsedEmail{MessageID: "new-message@example.com", GmailThreadID: tc.id, InReplyTo: "forged@example.com", TextContent: "Begin forwarded message"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ThreadID != tc.want {
			t.Errorf("%s: room=%s, want %s", tc.id, got.ThreadID, tc.want)
		}
	}
}

func TestGmailThreadCacheOverridesQuotedForward(t *testing.T) {
	tm := NewThreadManager(nil)
	original, err := tm.DetermineThread("me", &ParsedEmail{MessageID: "root@example.com", GmailThreadID: "gmail"})
	if err != nil {
		t.Fatal(err)
	}
	tm.CacheForReceiver("me", original)
	reply, err := tm.DetermineThread("me", &ParsedEmail{MessageID: "reply@example.com", GmailThreadID: "gmail", TextContent: "Begin forwarded message"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != original.ThreadID {
		t.Fatalf("reply opened %s", reply.ThreadID)
	}
}

func TestGmailLookupErrorDoesNotCreateThread(t *testing.T) {
	failure := errors.New("lookup timed out")
	tm := NewThreadManager(gmailResolver{err: failure})
	got, err := tm.DetermineThread("me", &ParsedEmail{MessageID: "new@example.com", GmailThreadID: "existing"})
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if got != nil {
		t.Fatalf("lookup failure created thread %s", got.ThreadID)
	}
}

func TestProcessParsedEmailRetriesGmailLookupFailure(t *testing.T) {
	failure := errors.New("database unavailable")
	tm := NewThreadManager(gmailResolver{err: failure})
	log := zerolog.Nop()
	p := NewProcessor(&log, tm, false, "")
	login := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "me"}}
	parsed := &ParsedEmail{MessageID: "new@example.com", GmailThreadID: "existing", From: "alice@example.com"}
	msg, err := p.ProcessParsedEmail(context.Background(), parsed, login, "INBOX")
	if msg != nil || !errors.Is(err, failure) {
		t.Fatalf("message=%v error=%v", msg, err)
	}
	tm.resolver = gmailResolver{}
	msg, err = p.ProcessParsedEmail(context.Background(), parsed, login, "INBOX")
	if err != nil || msg == nil || msg.Thread.ThreadID != "persisted-room" {
		t.Fatalf("retry: message=%v error=%v", msg, err)
	}
}
