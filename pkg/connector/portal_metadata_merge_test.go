package connector

import "testing"

// Persisting on inbound made three goroutines write the portal row that used to
// have one writer, and the row is the only durable copy of the thread. These
// tests pin the two halves of the merge rule, which pull in opposite
// directions: identity accumulates, recipients get replaced. Getting either
// backwards is silent — the bridge keeps delivering, and the damage shows up as
// a reply that will not send or one that reaches the wrong people.

// The skeleton case. Thread resolution can produce an EmailThread holding only
// an ID and subject, and before the merge existed that skeleton was assigned
// straight over the stored row: References, GmailThreadID and LastDeliveredTo
// gone, with no other copy anywhere. The visible symptom is a thread that stops
// threading and starts replying from the wrong alias, several messages later.
func TestMergePortalMetadata_SkeletonDoesNotWipeStoredThread(t *testing.T) {
	t.Parallel()

	stored := &PortalMetadata{
		ThreadID:              "t1",
		Subject:               "Grant reporting",
		References:            []string{"a@example.com", "b@example.com", "c@example.com"},
		GmailThreadID:         "gt-99",
		LastDeliveredTo:       "grants@apartresearch.com",
		LastOutboundMessageID: "mine-1@example.com",
		LastMessageID:         "c@example.com",
		Participants:          []string{"alice@example.com"},
	}
	skeleton := &PortalMetadata{ThreadID: "t1", Subject: "Grant reporting"}

	got := mergePortalMetadata(stored, skeleton)

	if len(got.References) != 3 {
		t.Errorf("References = %v; want the stored chain of 3 — a thread that loses it stops threading for good", got.References)
	}
	if got.GmailThreadID != "gt-99" {
		t.Errorf("GmailThreadID = %q; want gt-99", got.GmailThreadID)
	}
	if got.LastDeliveredTo != "grants@apartresearch.com" {
		t.Errorf("LastDeliveredTo = %q; want the stored alias — replies would otherwise go out as the wrong From", got.LastDeliveredTo)
	}
	if got.LastOutboundMessageID != "mine-1@example.com" {
		t.Errorf("LastOutboundMessageID = %q; want it kept — the reply guard refuses a reply to our own last send without it", got.LastOutboundMessageID)
	}
	if got.LastMessageID != "c@example.com" {
		t.Errorf("LastMessageID = %q; want it kept", got.LastMessageID)
	}
	if len(got.Participants) != 1 {
		t.Errorf("Participants = %v; want the stored set", got.Participants)
	}
}

// The other half, and the reason this is a merge and not a straight "keep
// everything non-empty": the recipient fields must be allowed to go empty.
// An inbound with no Cc: has to erase the stored Cc, or a reply reaches the
// person the sender just dropped from the thread -- the exact bug the
// stickiness fix removed, reintroduced through the storage layer.
func TestMergePortalMetadata_DroppedRecipientsAreCleared(t *testing.T) {
	t.Parallel()

	stored := &PortalMetadata{
		ThreadID:             "t1",
		LastFrom:             "alice@example.com",
		LastTo:               []string{"moiri@apartresearch.com", "bob@example.com"},
		LastCc:               []string{"funder@example.org"},
		LastInboundMessageID: "b@example.com",
	}
	// The newest inbound: Alice has taken the funder and Bob off-thread.
	fresh := &PortalMetadata{
		ThreadID:             "t1",
		LastFrom:             "alice@example.com",
		LastTo:               []string{"moiri@apartresearch.com"},
		LastCc:               nil,
		LastInboundMessageID: "c@example.com",
	}

	got := mergePortalMetadata(stored, fresh)

	if len(got.LastCc) != 0 {
		t.Errorf("LastCc = %v; want empty — merging it back is how a reply reaches someone taken off-thread", got.LastCc)
	}
	if len(got.LastTo) != 1 {
		t.Errorf("LastTo = %v; want only the remaining recipient", got.LastTo)
	}
	if got.LastInboundMessageID != "c@example.com" {
		t.Errorf("LastInboundMessageID = %q; want the newest inbound, or the guard compares against a stale message", got.LastInboundMessageID)
	}
}

// A first write has nothing to merge against.
func TestMergePortalMetadata_NoStoredRow(t *testing.T) {
	t.Parallel()

	fresh := &PortalMetadata{ThreadID: "t1", References: []string{"a@example.com"}}
	if got := mergePortalMetadata(nil, fresh); got != fresh {
		t.Errorf("merge onto a nil stored row = %+v; want the fresh snapshot unchanged", got)
	}
	stored := &PortalMetadata{ThreadID: "t1"}
	if got := mergePortalMetadata(stored, nil); got != stored {
		t.Errorf("merge of a nil snapshot = %+v; want the stored row untouched", got)
	}
}
