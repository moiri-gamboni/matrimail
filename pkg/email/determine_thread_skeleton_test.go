package email

import "testing"

// fakeResolver names a thread ID for any message ID it was given, standing in
// for the database-backed resolver the bridge wires in.
type fakeResolver struct{ tid string }

func (f fakeResolver) ResolveThreadID(receiver, messageID string) (string, bool) {
	if f.tid == "" {
		return "", false
	}
	return f.tid, true
}

// The skeleton path, end to end.
//
// When an external resolver names a thread that is not in the in-memory cache,
// DetermineThread builds an EmailThread holding only a ThreadID and Subject and
// runs the inbound through it. Everything the resolver knew about the thread's
// history -- its References chain, its Gmail thread id, the alias it was
// delivered to, the last message we sent -- is absent from that struct, because
// the resolver returns an ID and nothing else.
//
// That object is then handed to the caller, which persists it. The persisted
// row is the only durable copy. This test pins what the skeleton path actually
// produces, so the gap between "the thread" and "what this code path holds"
// stays visible: the existing unit tests all construct a fully-populated thread
// by hand and so cannot see it. The merge in the connector package is what
// makes the gap harmless; this is the evidence that the gap is real.
func TestDetermineThread_ResolverMissCarriesOnlyTheNewMessage(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(fakeResolver{tid: "original-thread@example.com"})

	// A reply arriving cold: the resolver knows its thread, the cache does not.
	thread := tm.DetermineThread("me@example.com", &ParsedEmail{
		MessageID:   "reply-7@example.com",
		InReplyTo:   "reply-6@example.com",
		From:        "alice@example.com",
		To:          []string{"me@example.com"},
		Subject:     "Re: Grant reporting",
		TextContent: "Numbers attached.",
	})

	if thread.ThreadID != "original-thread@example.com" {
		t.Fatalf("ThreadID = %q; the resolver's answer should win", thread.ThreadID)
	}

	// What the path does carry: the newest message's own context. These are the
	// fields a reply is addressed from, and they are correct.
	if thread.LastInboundMessageID != "reply-7@example.com" {
		t.Errorf("LastInboundMessageID = %q, want reply-7@example.com", thread.LastInboundMessageID)
	}
	if len(thread.LastTo) != 1 || thread.LastTo[0] != "me@example.com" {
		t.Errorf("LastTo = %v, want the newest message's recipients", thread.LastTo)
	}

	// What it does not carry, and the reason the connector merges rather than
	// assigns. If this ever starts arriving populated the merge is still
	// correct, but the reasoning about how reachable the skeleton path is would
	// need revisiting -- so assert the absence rather than leaving it implicit.
	if thread.LastOutboundMessageID != "" {
		t.Errorf("LastOutboundMessageID = %q; the skeleton path is not expected to recover our own sends -- if it now does, revisit the merge rationale",
			thread.LastOutboundMessageID)
	}
	if thread.GmailThreadID != "" {
		t.Errorf("GmailThreadID = %q; not expected from a resolver hit", thread.GmailThreadID)
	}
	// The chain holds only this message, not the six before it: persisting this
	// over a stored eight-link chain would truncate it to one.
	if len(thread.References) > 1 {
		t.Errorf("References = %v; the skeleton path is not expected to recover the chain", thread.References)
	}
}
