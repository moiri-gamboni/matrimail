package email

import "testing"

// A reply is addressed from the thread's Last* state. Those fields must track
// the most recent inbound message exactly, because an email with no Cc: header
// parses to a nil slice and the previous behaviour ("only assign when non-nil")
// left the *earlier* message's Cc in place.
//
// The concrete failure: Alice mails you and Cc's a funder, then sends a
// follow-up to you alone in the same thread. LastCc still held the funder, while
// LastTextBody had already been overwritten with the private follow-up — so a
// reply went to the funder quoting the message Alice had taken off-thread.
func TestAddToExistingThread_RecipientsAreNotSticky(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	thread := &EmailThread{
		ThreadID:  "msg-1@example.com",
		Subject:   "Grant reporting",
		MessageID: "msg-1@example.com",
	}

	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID:   "msg-1@example.com",
		From:        "alice@example.com",
		To:          []string{"me@example.com"},
		Cc:          []string{"funder@example.org"},
		TextContent: "Looping in our funder.",
	})
	if len(thread.LastCc) != 1 || thread.LastCc[0] != "funder@example.org" {
		t.Fatalf("setup: LastCc = %v; want the funder", thread.LastCc)
	}

	// Same thread, no Cc header at all: Alice has taken this off-thread.
	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID:   "msg-2@example.com",
		From:        "alice@example.com",
		To:          []string{"me@example.com"},
		Cc:          nil,
		InReplyTo:   "msg-1@example.com",
		TextContent: "Between us, the numbers are not going to work.",
	})

	if len(thread.LastCc) != 0 {
		t.Errorf("LastCc = %v; want empty — a reply would have gone to %v while quoting a message they were not on",
			thread.LastCc, thread.LastCc)
	}
	if thread.LastTextBody != "Between us, the numbers are not going to work." {
		t.Errorf("LastTextBody = %q; the body tracks the newest message, which is why a stale Cc is dangerous", thread.LastTextBody)
	}
}

// The same stickiness applied to To.
func TestAddToExistingThread_ToIsNotSticky(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	thread := &EmailThread{ThreadID: "t", MessageID: "t"}
	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID: "a@example.com",
		From:      "alice@example.com",
		To:        []string{"me@example.com", "bob@example.com"},
	})
	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID: "b@example.com",
		From:      "alice@example.com",
		To:        nil,
	})
	if len(thread.LastTo) != 0 {
		t.Errorf("LastTo = %v; want empty", thread.LastTo)
	}
}
