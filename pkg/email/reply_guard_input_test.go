package email

import "testing"

// The reply-target guard in pkg/connector refuses an outbound aimed at any
// message other than the newest one, by comparing the Matrix reply target
// against thread.LastInboundMessageID. It treats an empty LastInboundMessageID
// as "restored from metadata that predates this field" and waves the reply
// through.
//
// That fallback makes the guard's own tests unable to detect its failure. Drop
// either assignment below and nothing breaks, nothing logs, and every existing
// test still passes -- the guard just silently stops guarding, and the
// over-share it exists to prevent comes back. So the producer is pinned here,
// on the two paths that build a thread from an inbound message.
//
// Not a redundant restatement of the recipient-stickiness tests: those assert
// the Last* recipients track the newest message. This asserts the thread also
// records *which* message that was. Without it the recipients are correct and
// unusable, because nothing can tell whether a given reply is aimed at them.

func TestCreateNewThread_RecordsTheInboundTheRecipientsDescribe(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	thread := tm.createNewThread(&ParsedEmail{
		MessageID: "first@example.com",
		From:      "alice@example.com",
		To:        []string{"me@example.com"},
		Subject:   "Grant reporting",
	})

	if thread.LastInboundMessageID != "first@example.com" {
		t.Errorf("LastInboundMessageID = %q, want first@example.com; empty makes the reply-target guard wave every reply through",
			thread.LastInboundMessageID)
	}
}

func TestAddToExistingThread_AdvancesTheInboundTheRecipientsDescribe(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	thread := &EmailThread{ThreadID: "t", MessageID: "t"}
	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID: "msg-1@example.com",
		From:      "alice@example.com",
		To:        []string{"me@example.com"},
	})
	if thread.LastInboundMessageID != "msg-1@example.com" {
		t.Fatalf("LastInboundMessageID = %q after the first inbound, want msg-1@example.com", thread.LastInboundMessageID)
	}

	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID: "msg-2@example.com",
		From:      "alice@example.com",
		To:        []string{"me@example.com"},
		InReplyTo: "msg-1@example.com",
	})
	// Must advance, not just be non-empty: a stale value here refuses the
	// user's reply to the message actually in front of them, while accepting
	// one aimed at a message whose recipients are no longer loaded.
	if thread.LastInboundMessageID != "msg-2@example.com" {
		t.Errorf("LastInboundMessageID = %q after a second inbound, want msg-2@example.com", thread.LastInboundMessageID)
	}
}
