// Phase D: Portal.Metadata schema for matrimail.
//
// The framework gives connectors a free-form `Metadata any` slot on each
// Portal row that is JSON-serialized into the SQL `portal.metadata` column.
// We use it to round-trip enough thread state that synthetic compose threads
// (which only live in the in-memory ThreadManager cache, TTL 24h) survive a
// bridge restart or a long idle period.
//
// All fields are JSON-tagged with `omitempty` so an empty PortalMetadata
// serializes to `{}` rather than padding the column with NULL strings, which
// matters for forward-compatibility: future fields can be added without
// migrating older rows.
package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/Leicas/matrimail/pkg/email"
)

// PortalMetadata mirrors the parts of email.EmailThread that are useful to
// reconstruct a thread from cold storage. Kept narrow on purpose: the
// participant-delta fields (Added/Removed) are runtime-only churn from inbound
// threading and have no meaning at restore time.
type PortalMetadata struct {
	// ThreadID is the EmailThread.ThreadID. Stored explicitly so a defensive
	// reader can sanity-check that the metadata it loaded actually belongs to
	// the thread the portal claims to host (mismatches mean the row was
	// hand-edited or the schema drifted).
	ThreadID string `json:"thread_id,omitempty"`

	// Subject is the canonical subject line, with no Re:/Fwd: stripping. Used
	// when restoring a thread that has never received an inbound message
	// (i.e. a draft) so HandleMatrixMessage can build the outgoing Subject
	// without falling back to "(no subject)".
	Subject string `json:"subject,omitempty"`

	// Participants is the active participant set (To + Cc folded together,
	// without From). For compose threads this seeds the recipient list.
	Participants []string `json:"participants,omitempty"`

	// References is the threading chain (oldest first). After the first send
	// in a compose thread this gets populated with the new Message-ID and
	// is what subsequent replies thread against.
	References []string `json:"references,omitempty"`

	// LastMessageID is the most recent Message-ID we know of for this thread.
	// On send, we update this to the dedup key (server-assigned ID for Gmail
	// API, our generated ID for SMTP) so the next outbound reply has a tail
	// to thread against even when the in-memory ThreadManager has evicted.
	LastMessageID string `json:"last_message_id,omitempty"`

	// IsDraft marks a synthetic compose thread that has not yet produced an
	// outbound email. Cleared on first successful send.
	IsDraft bool `json:"is_draft,omitempty"`

	// GmailThreadID is the Gmail API's server-assigned thread ID. Persisted
	// so that restart-time thread reconstruction can re-prime the
	// gmailThreadIDIndex fallback lookup key.
	GmailThreadID string `json:"gmail_thread_id,omitempty"`

	// LastFrom is the sender of the most recent inbound (DM-mode target).
	LastFrom string `json:"last_from,omitempty"`
	// LastTo / LastCc are the To/Cc of the most recent inbound; used to
	// split reply-all To/Cc on the next outbound.
	LastTo []string `json:"last_to,omitempty"`
	LastCc []string `json:"last_cc,omitempty"`
	// LastInboundMessageID identifies which inbound the Last* fields describe,
	// so a restored thread can still refuse a reply aimed at an older message.
	LastInboundMessageID string `json:"last_inbound_message_id,omitempty"`
	// LastOutboundMessageID is the most recent message we sent in this thread.
	LastOutboundMessageID string `json:"last_outbound_message_id,omitempty"`
	// LastDeliveredTo is the alias the most recent inbound was addressed to;
	// replies use this as From.
	LastDeliveredTo string `json:"last_delivered_to,omitempty"`

	// LastDate is the Date header of the most recent inbound, used by the
	// Gmail-style quote builder for the attribution line.
	LastDate time.Time `json:"last_date,omitempty"`
	// LastTextBody is the plain-text body of the most recent inbound, capped
	// at email.MaxQuoteBodyBytes. Persisted so post-restart outbound replies
	// can still produce a quote block; without it the reply ships unquoted.
	LastTextBody string `json:"last_text_body,omitempty"`
	// LastHTMLBody is the html body of the most recent inbound, capped at
	// email.MaxQuoteBodyBytes.
	LastHTMLBody string `json:"last_html_body,omitempty"`
}

// PortalMetadataFromThread snapshots a thread's reply context for persistence.
//
// This must be written on every event that changes the context, not only on
// send. Writing it on send alone leaves the stored copy describing the world as
// of the user's last reply: after a restart, a room whose newest event is
// inbound rehydrates from that stale copy, and an ordinary typed reply is
// addressed from recipients the sender has since dropped -- the same
// over-share the unconditional Last* assignment in addToExistingThread exists
// to prevent, reached by a different route.
func PortalMetadataFromThread(thread *email.EmailThread) *PortalMetadata {
	if thread == nil {
		return nil
	}
	return &PortalMetadata{
		ThreadID:              thread.ThreadID,
		Subject:               thread.Subject,
		Participants:          append([]string(nil), thread.Participants...),
		References:            append([]string(nil), thread.References...),
		LastMessageID:         thread.MessageID,
		IsDraft:               thread.IsDraft,
		GmailThreadID:         thread.GmailThreadID,
		LastFrom:              thread.LastFrom,
		LastTo:                append([]string(nil), thread.LastTo...),
		LastCc:                append([]string(nil), thread.LastCc...),
		LastInboundMessageID:  thread.LastInboundMessageID,
		LastOutboundMessageID: thread.LastOutboundMessageID,
		LastDeliveredTo:       thread.LastDeliveredTo,
		LastDate:              thread.LastDate,
		LastTextBody:          thread.LastTextBody,
		LastHTMLBody:          thread.LastHTMLBody,
	}
}

// PersistThreadState writes the thread's reply context onto the portal. Called
// from both inbound paths; best-effort, because losing the snapshot degrades to
// the in-memory cache rather than breaking delivery.
func PersistThreadState(ctx context.Context, portal *bridgev2.Portal, thread *email.EmailThread) error {
	if portal == nil || thread == nil {
		return nil
	}
	portal.Metadata = PortalMetadataFromThread(thread)
	return portal.Save(ctx)
}
