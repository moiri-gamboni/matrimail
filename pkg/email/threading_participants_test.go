package email

import (
	"sort"
	"strings"
	"testing"
)

// Locks in the union-of-participants behavior: a degenerate inbound email
// (Sent-folder echo of a self-sent reply, BCC-only, unparseable headers)
// must NOT erase the thread's existing participants. Without this guarantee
// the outbound reply path errors with "no recipients (thread participants
// empty after self-exclusion)" because the only remaining participant is
// the user themselves.
func TestAddToExistingThread_UnionsParticipants(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	// Initial thread state: Alice and Antoine, established by an earlier
	// inbound email (e.g. Alice → Antoine).
	thread := &EmailThread{
		ThreadID:     "msg-1@example.com",
		Subject:      "Re: this is a test email",
		Participants: []string{"alice@example.com", "antoine@haply.co"},
		MessageID:    "msg-1@example.com",
		References:   []string{"msg-1@example.com"},
	}

	// New inbound: looks like a self-sent reply that lost its recipient
	// somewhere in the parser (degenerate From, missing To). Without the
	// fix, this would set thread.Participants = {antoine@haply.co}.
	degenerate := &ParsedEmail{
		MessageID: "msg-2@example.com",
		Subject:   "Re: this is a test email",
		From:      "antoine@haply.co",
		To:        nil, // empty / unparseable
		Cc:        nil,
		InReplyTo: "msg-1@example.com",
	}

	tm.addToExistingThread(thread, degenerate)

	got := append([]string(nil), thread.Participants...)
	sort.Strings(got)
	want := []string{"alice@example.com", "antoine@haply.co"}
	if !equalSorted(got, want) {
		t.Errorf("after degenerate email, got %v; want %v (alice should have been preserved)", got, want)
	}
}

// Sanity-check the happy path: a normal inbound from Alice to Antoine adds
// Alice if she wasn't already there, and keeps everyone existing.
func TestAddToExistingThread_NormalInbound(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)
	thread := &EmailThread{
		ThreadID:     "msg-1@example.com",
		Participants: []string{"antoine@haply.co"},
		MessageID:    "msg-1@example.com",
	}
	in := &ParsedEmail{
		MessageID: "msg-2@example.com",
		From:      "alice@example.com",
		To:        []string{"antoine@haply.co"},
		Cc:        []string{"bob@example.com"},
		InReplyTo: "msg-1@example.com",
	}
	tm.addToExistingThread(thread, in)
	got := append([]string(nil), thread.Participants...)
	sort.Strings(got)
	want := []string{"alice@example.com", "antoine@haply.co", "bob@example.com"}
	if !equalSorted(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

// Locks in the Gmail threadId fallback: an inbound whose RFC 5322 headers
// (Message-ID, In-Reply-To, References) miss the existing thread cache but
// whose GmailThreadID matches a previously-cached thread MUST attach to that
// thread rather than creating a new one.
func TestDetermineThread_GmailThreadIDFallback(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	// Seed an existing thread with a known Gmail thread id.
	existing := &EmailThread{
		ThreadID:      "m1",
		Subject:       "Project status",
		Participants:  []string{"alice@example.com", "antoine@haply.co"},
		MessageID:     "m1",
		References:    []string{"m1"},
		GmailThreadID: "g1",
	}
	tm.CacheForReceiver("email:antoine@haply.co", existing)

	// Brand-new message: fresh Message-ID, empty threading headers — only the
	// GmailThreadID ties it back to the previous conversation.
	fresh := &ParsedEmail{
		MessageID:     "m2-fresh@gmail.com",
		Subject:       "Re: Project status",
		From:          "alice@example.com",
		To:            []string{"antoine@haply.co"},
		InReplyTo:     "",
		References:    nil,
		GmailThreadID: "g1",
	}
	got, err := tm.DetermineThread("email:antoine@haply.co", fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("DetermineThread returned nil; expected existing thread")
	}
	if got.ThreadID != "m1" {
		t.Errorf("expected ThreadID=m1 (existing thread via GmailThreadID fallback); got %q", got.ThreadID)
	}
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
