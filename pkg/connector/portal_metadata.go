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
	"sync"
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

// persistMu serialises the read-modify-write below.
//
// Until persisting on receive was added, the send path was the only writer to
// Portal.Metadata and it ran on the portal's own event goroutine. There are now
// two more writers — the Gmail poller and the IMAP idle loop — and bridgev2's
// Portal has no lock covering Metadata. Without this, an inbound arriving
// alongside a send can save a snapshot taken before the send, dropping that
// send from References and clearing LastOutboundMessageID, after which the
// reply guard refuses a reply to the user's own last message. The symptom
// appears in no log.
var persistMu sync.Mutex

// PersistThreadState writes the thread's reply context onto the portal. Called
// from the send path and from both inbound paths; best-effort, because losing
// the snapshot degrades to the in-memory cache rather than breaking delivery.
//
// Fields are merged rather than assigned, per the rules on
// mergePortalMetadata. The caller does not always hold the whole thread:
// resolution can hand back a skeleton carrying only a ThreadID and Subject
// (threading.go builds one when an external resolver names a thread that is not
// in cache), and assigning that over the stored row would wipe the References
// chain, the Gmail thread id and the alias to reply from — permanently, because
// the row is the last copy.
func PersistThreadState(ctx context.Context, portal *bridgev2.Portal, thread *email.EmailThread) error {
	if portal == nil || thread == nil {
		return nil
	}
	persistMu.Lock()
	defer persistMu.Unlock()

	prev, _ := portal.Metadata.(*PortalMetadata)
	portal.Metadata = mergePortalMetadata(prev, PortalMetadataFromThread(thread))
	return portal.Save(ctx)
}

// mergePortalMetadata folds a fresh snapshot onto the stored one.
//
// Split out from PersistThreadState only so it can be tested: exercising the
// real function needs a live bridgev2.Portal and a database behind Save, which
// is how the wholesale-overwrite bug this fixes reached the tree untested.
//
// The rule differs per field on purpose, and the division is the whole point:
//
//   - Thread identity (References, GmailThreadID, LastDeliveredTo, Subject,
//     Participants, LastMessageID, LastOutboundMessageID) accumulates. An empty
//     incoming value means the caller did not know it, not that it is gone, so
//     the stored value wins. References compares length rather than emptiness
//     because the inbound path rebuilds the chain from one message's headers
//     and can hand back a shorter-but-non-empty one.
//   - The inbound reply context (LastFrom, LastTo, LastCc,
//     LastInboundMessageID, LastDate and the quoted bodies) is replaced
//     wholesale, empties included, whenever the snapshot describes an inbound.
//     Clearing them on absence is the recipient-stickiness fix itself: an
//     inbound with no Cc: must erase the previous Cc, or a reply reaches
//     someone the sender deliberately dropped. Merging them field by field
//     would reintroduce that bug through the storage layer.
//   - A snapshot describing no inbound at all keeps the stored context as a
//     unit. That is a skeleton thread holding only the user's own message,
//     which leaves the context alone by design; its empties mean "unknown".
func mergePortalMetadata(prev, next *PortalMetadata) *PortalMetadata {
	if next == nil {
		return prev
	}
	if prev == nil {
		return next
	}
	if !next.hasInboundContext() {
		next.LastFrom = prev.LastFrom
		next.LastTo = append([]string(nil), prev.LastTo...)
		next.LastCc = append([]string(nil), prev.LastCc...)
		next.LastInboundMessageID = prev.LastInboundMessageID
		next.LastDate = prev.LastDate
		next.LastTextBody = prev.LastTextBody
		next.LastHTMLBody = prev.LastHTMLBody
	}
	if len(next.References) < len(prev.References) {
		next.References = append([]string(nil), prev.References...)
	}
	if next.GmailThreadID == "" {
		next.GmailThreadID = prev.GmailThreadID
	}
	if next.LastDeliveredTo == "" {
		next.LastDeliveredTo = prev.LastDeliveredTo
	}
	if next.LastOutboundMessageID == "" {
		next.LastOutboundMessageID = prev.LastOutboundMessageID
	}
	if next.LastMessageID == "" {
		next.LastMessageID = prev.LastMessageID
	}
	if next.Subject == "" {
		next.Subject = prev.Subject
	}
	if len(next.Participants) == 0 {
		next.Participants = append([]string(nil), prev.Participants...)
	}
	return next
}

// hasInboundContext reports whether the snapshot carries the reply context of
// some inbound. LastFrom is checked as well as the ID because rows written
// before LastInboundMessageID existed carry only the former.
func (pm *PortalMetadata) hasInboundContext() bool {
	return pm.LastInboundMessageID != "" || pm.LastFrom != ""
}

// fillInboundContext completes a cached thread that holds no inbound reply
// context from the stored row. That thread is a skeleton the user's own
// message was threaded onto after the cache lost it; without this, a reply
// typed in Matrix would be addressed from the user's own message's recipients
// until the next inbound arrives.
func fillInboundContext(thread *email.EmailThread, pm *PortalMetadata) {
	if thread.LastInboundMessageID != "" || thread.LastFrom != "" || !pm.hasInboundContext() {
		return
	}
	thread.LastFrom = pm.LastFrom
	thread.LastTo = append([]string(nil), pm.LastTo...)
	thread.LastCc = append([]string(nil), pm.LastCc...)
	thread.LastInboundMessageID = pm.LastInboundMessageID
	thread.LastDate = pm.LastDate
	thread.LastTextBody = pm.LastTextBody
	thread.LastHTMLBody = pm.LastHTMLBody
	if thread.LastDeliveredTo == "" {
		thread.LastDeliveredTo = pm.LastDeliveredTo
	}
}

// ThreadFromPortalMetadata is the inverse of PortalMetadataFromThread.
//
// Kept beside it deliberately: the two restore sites previously held this
// literal twice, and they had already drifted -- one gained LastDate and the
// quoted bodies while the other went without, so a thread restored through the
// second path replied with no quote. A field added to one direction and not the
// other is silent, and the reflection test over the forward direction cannot
// see the inverse.
func ThreadFromPortalMetadata(pm *PortalMetadata) *email.EmailThread {
	if pm == nil {
		return nil
	}
	return &email.EmailThread{
		ThreadID:              pm.ThreadID,
		Subject:               pm.Subject,
		Participants:          append([]string(nil), pm.Participants...),
		References:            append([]string(nil), pm.References...),
		MessageID:             pm.LastMessageID,
		IsDraft:               pm.IsDraft,
		GmailThreadID:         pm.GmailThreadID,
		LastFrom:              pm.LastFrom,
		LastTo:                append([]string(nil), pm.LastTo...),
		LastCc:                append([]string(nil), pm.LastCc...),
		LastInboundMessageID:  pm.LastInboundMessageID,
		LastOutboundMessageID: pm.LastOutboundMessageID,
		LastDeliveredTo:       pm.LastDeliveredTo,
		LastDate:              pm.LastDate,
		LastTextBody:          pm.LastTextBody,
		LastHTMLBody:          pm.LastHTMLBody,
	}
}
