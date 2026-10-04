package email

import "testing"

type gmailResolver struct{}

func (gmailResolver) ResolveThreadID(receiver, messageID string) (string, bool) {
	return "wrong-room", true
}
func (gmailResolver) ResolveGmailThreadID(receiver, gmailID string) (string, bool) {
	if gmailID == "existing" {
		return "persisted-room", true
	}
	return "", false
}

func TestGmailThreadOverridesForwardAndReplyHeaders(t *testing.T) {
	for _, tc := range []struct{ id, want string }{{"existing", "persisted-room"}, {"new", "new-message@example.com"}} {
		tm := NewThreadManager(gmailResolver{})
		got := tm.DetermineThread("me", &ParsedEmail{MessageID: "new-message@example.com", GmailThreadID: tc.id, InReplyTo: "forged@example.com", TextContent: "Begin forwarded message"})
		if got.ThreadID != tc.want {
			t.Errorf("%s: room=%s, want %s", tc.id, got.ThreadID, tc.want)
		}
	}
}

func TestGmailThreadCacheOverridesQuotedForward(t *testing.T) {
	tm := NewThreadManager(nil)
	original := tm.DetermineThread("me", &ParsedEmail{MessageID: "root@example.com", GmailThreadID: "gmail"})
	tm.CacheForReceiver("me", original)
	reply := tm.DetermineThread("me", &ParsedEmail{MessageID: "reply@example.com", GmailThreadID: "gmail", TextContent: "Begin forwarded message"})
	if reply.ThreadID != original.ThreadID {
		t.Fatalf("reply opened %s", reply.ThreadID)
	}
}
