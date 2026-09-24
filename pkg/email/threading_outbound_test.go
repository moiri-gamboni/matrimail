package email

import (
	"testing"
	"time"
)

// A message the user sent from another client (Gmail web, a phone) reaches the
// bridge through the Sent label or folder. It belongs in the thread's room, but
// the thread's Last* fields describe the inbound a reply must answer. If the
// user's own message overwrote them, the next reply typed in Matrix would be
// addressed to the recipients of the user's message -- in DM mode, to the user.
func TestAddToExistingThread_OutboundKeepsInboundReplyContext(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)
	inboundDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	thread := tm.createNewThread(&ParsedEmail{
		MessageID:   "in-1@example.com",
		From:        "Alice <alice@example.com>",
		To:          []string{"me@example.com"},
		Cc:          []string{"bob@example.com"},
		Date:        inboundDate,
		TextContent: "Can you send the numbers?",
		HTMLContent: "<p>Can you send the numbers?</p>",
		DeliveredTo: "me@example.com",
	})

	tm.addToExistingThread(thread, &ParsedEmail{
		MessageID:     "out-1@example.com",
		InReplyTo:     "in-1@example.com",
		From:          "Me <me@example.com>",
		To:            []string{"alice@example.com"},
		Date:          inboundDate.Add(time.Hour),
		TextContent:   "Attached.",
		GmailThreadID: "gtid-1",
		Outbound:      true,
	})

	if thread.LastFrom != "Alice <alice@example.com>" {
		t.Errorf("LastFrom = %q; want the inbound sender, or a DM-mode reply goes to ourselves", thread.LastFrom)
	}
	if len(thread.LastTo) != 1 || thread.LastTo[0] != "me@example.com" {
		t.Errorf("LastTo = %v; want the inbound's To", thread.LastTo)
	}
	if len(thread.LastCc) != 1 || thread.LastCc[0] != "bob@example.com" {
		t.Errorf("LastCc = %v; want the inbound's Cc", thread.LastCc)
	}
	if thread.LastInboundMessageID != "in-1@example.com" {
		t.Errorf("LastInboundMessageID = %q; want the inbound", thread.LastInboundMessageID)
	}
	if thread.LastDeliveredTo != "me@example.com" {
		t.Errorf("LastDeliveredTo = %q; want the alias the inbound reached", thread.LastDeliveredTo)
	}
	if !thread.LastDate.Equal(inboundDate) {
		t.Errorf("LastDate = %v; want the inbound's date for the quote attribution", thread.LastDate)
	}
	if thread.LastTextBody != "Can you send the numbers?" || thread.LastHTMLBody != "<p>Can you send the numbers?</p>" {
		t.Errorf("quoted bodies = %q / %q; want the inbound's", thread.LastTextBody, thread.LastHTMLBody)
	}

	// What an outbound does change: it is the thread's newest message and our
	// own latest send, and it joins the chain.
	if thread.MessageID != "out-1@example.com" {
		t.Errorf("MessageID = %q; want the outbound, the newest message", thread.MessageID)
	}
	if thread.LastOutboundMessageID != "out-1@example.com" {
		t.Errorf("LastOutboundMessageID = %q; want the outbound", thread.LastOutboundMessageID)
	}
	if len(thread.References) == 0 || thread.References[len(thread.References)-1] != "out-1@example.com" {
		t.Errorf("References = %v; want the outbound appended", thread.References)
	}
	if thread.GmailThreadID != "gtid-1" {
		t.Errorf("GmailThreadID = %q; want it learned from the outbound", thread.GmailThreadID)
	}
}

// A thread the user started from another client has no inbound yet. Its reply
// context must stay empty so a reply falls back to the thread's participants
// minus the user's own addresses, rather than treating the user as the sender
// to answer.
func TestCreateNewThread_OutboundHasNoInboundReplyContext(t *testing.T) {
	t.Parallel()
	tm := NewThreadManager(nil)

	thread := tm.createNewThread(&ParsedEmail{
		MessageID:   "out-1@example.com",
		From:        "me@example.com",
		To:          []string{"alice@example.com"},
		Cc:          []string{"bob@example.com"},
		Date:        time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		TextContent: "Kicking this off.",
		Outbound:    true,
	})

	if thread.LastFrom != "" || len(thread.LastTo) != 0 || len(thread.LastCc) != 0 || thread.LastInboundMessageID != "" {
		t.Errorf("reply context = from %q to %v cc %v id %q; want empty, there is no inbound",
			thread.LastFrom, thread.LastTo, thread.LastCc, thread.LastInboundMessageID)
	}
	if thread.LastTextBody != "" || !thread.LastDate.IsZero() {
		t.Errorf("quote context = %q at %v; want empty", thread.LastTextBody, thread.LastDate)
	}
	if thread.LastOutboundMessageID != "out-1@example.com" {
		t.Errorf("LastOutboundMessageID = %q; want the outbound", thread.LastOutboundMessageID)
	}
	if len(thread.Participants) != 3 {
		t.Errorf("Participants = %v; want sender and both recipients", thread.Participants)
	}
}
