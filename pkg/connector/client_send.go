// Phase C addition: outbound (Matrix -> Email) flow.
//
// This file contains EmailClient.handleMatrixMessageOutbound — the full
// implementation invoked by HandleMatrixMessage in client.go. Lives in its
// own file for review hygiene; nothing else needs to import it.
//
// The flow:
//
//  1. Resolve the EmailThread for this portal via ThreadManager.
//  2. Compute In-Reply-To and References from the reply target / thread state.
//  3. Resolve recipients = thread participants minus self.
//  4. Build OutgoingMessage (Re: prefix, optional HTML body, attachments).
//  5. Download Matrix media (if media msgtype) into an EmailAttachment.
//  6. BuildMIME -> Sender.Send -> SentDedup.Record.
//  7. Best-effort APPEND to Sent (SMTP only — Gmail API auto-files).
//  8. Update thread cache so subsequent replies thread correctly.
//  9. Return MatrixMessageResponse with the network MessageID.
package connector

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrimail/pkg/common"
	"github.com/Leicas/matrimail/pkg/email"
)

// handleMatrixMessageOutbound is the full outbound implementation. Errors are
// returned to bridgev2 (which will surface them as message-status events to
// the user); successful sends record dedup state and return the network
// MessageID for the framework to persist.
func (ec *EmailClient) handleMatrixMessageOutbound(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if ec.Sender == nil {
		return nil, errors.New("matrimail: outbound disabled (no Sender configured for this account)")
	}
	if msg == nil || msg.Content == nil || msg.Portal == nil {
		return nil, errors.New("matrimail: invalid Matrix message (missing content or portal)")
	}

	// Phase D defensive guard: RoomFeatures already advertises Edit/Delete as
	// CapLevelRejected so the framework drops these in checkMessageContentCaps,
	// but a misbehaving client (or a future bridgev2 with looser validation)
	// could still hand us an m.replace. Email cannot be unsent — surface a
	// clear error rather than silently re-emitting the body as a fresh message.
	if msg.Content.RelatesTo != nil {
		if replaceID := msg.Content.RelatesTo.GetReplaceID(); replaceID != "" {
			ec.UserLogin.Log.Warn().
				Str("replace_id", string(replaceID)).
				Msg("dropping outbound: edits not supported (email cannot be unsent)")
			return nil, errors.New("matrimail: edits not supported (email cannot be unsent)")
		}
	}

	thread, err := ec.resolveThreadForPortalWithMetadata(msg.Portal)
	if err != nil {
		return nil, err
	}

	// IsDraft branch: a synthetic compose thread has never produced an email,
	// so threading headers must be empty (this is the FIRST message in the
	// chain). We also fall back to the message body's first line for Subject
	// if the user never set one via !matrimail compose subject:"...".
	wasDraft := thread.IsDraft
	var inReplyTo string
	var references []string
	if wasDraft {
		inReplyTo = ""
		references = nil
		if strings.TrimSpace(thread.Subject) == "" {
			thread.Subject = deriveSubjectFromBody(msg.Content.Body)
		}
	} else {
		if err := checkReplyTargetResolvable(thread, msg.ReplyTo); err != nil {
			_ = postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Send refused", err.Error())
			return nil, err
		}
		inReplyTo, references = computeReplyChain(thread, msg.ReplyTo)
	}

	// Build the alias set (lowercased) once: the user's primary address plus
	// any send-as aliases. We filter all of these from recipients and pick
	// the From by matching thread.LastDeliveredTo.
	selves := make([]string, 0, 1+len(ec.SendAsAliases))
	selves = append(selves, strings.ToLower(ec.Email))
	for _, sa := range ec.SendAsAliases {
		if sa.Email != "" {
			selves = append(selves, strings.ToLower(sa.Email))
		}
	}

	// DM-mode toggle: when the user ran !matrimail reply-only in this room,
	// the next send goes to thread.LastFrom only.
	dmMode := false
	if ec.Main != nil {
		dmMode = ec.Main.consumePortalNextReplyDM(msg.Portal.ID)
	}

	var to []netmail.Address
	var cc []netmail.Address
	var dropped []string
	if dmMode {
		to, err = resolveDMRecipients(thread, selves)
	} else {
		to, cc, dropped, err = resolveReplyAllRecipients(thread, selves)
	}
	if err != nil {
		return nil, err
	}
	if len(dropped) > 0 {
		ec.UserLogin.Log.Warn().Strs("dropped", dropped).
			Msg("recipients dropped: the thread lists addresses that will not parse; they are NOT on this reply")
	}

	// Pick From: the alias the most recent inbound was addressed to wins
	// (when it's known and is one of our addresses); otherwise the primary.
	fromAddr, fromName := pickFromAddressFromSlice(ec, thread, selves)
	om := buildOutgoingMessage(fromAddr, fromName, thread, msg, inReplyTo, references, to, cc)
	// Append the user's Gmail-side signature on NEW threads only, matching
	// the Gmail web UI's "include signature on first message in thread"
	// behavior. Signature is empty when the account isn't OAuth-Gmail or
	// when the user hasn't configured one — AppendSignature is a no-op then.
	if inReplyTo == "" && len(references) == 0 && ec.Signature != "" {
		om.TextBody = email.AppendSignature(om.TextBody, ec.Signature, false)
		if om.HTMLBody != "" {
			om.HTMLBody = email.AppendSignature(om.HTMLBody, ec.Signature, true)
		}
	}
	if wasDraft {
		// First send: don't add the "Re:" prefix even if the subject happens
		// to look like a reply already, and don't inherit any thread state
		// from the (empty) References list. buildOutgoingMessage already
		// honors msg.ReplyTo == nil here, so the only override needed is
		// stripping a stray Re: that the user typed manually.
		om.Subject = strings.TrimSpace(thread.Subject)
	}

	// Pull Matrix media (if any) into an EmailAttachment. Best-effort: a
	// failure here aborts the send because the user clearly intended to
	// attach something.
	if msg.Content.URL != "" || msg.Content.File != nil {
		att, attErr := ec.downloadMediaAsAttachment(ctx, msg.Content)
		if attErr != nil {
			_ = postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Send failed", "Failed to download attachment: "+attErr.Error())
			return nil, fmt.Errorf("download attachment: %w", attErr)
		}
		om.Attachments = append(om.Attachments, att)
	}

	mimeBytes, err := om.BuildMIME()
	if err != nil {
		_ = postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Send failed", "Failed to build MIME message: "+err.Error())
		return nil, fmt.Errorf("build mime: %w", err)
	}

	recipientStrs := make([]string, 0, len(to)+len(cc))
	for _, a := range to {
		recipientStrs = append(recipientStrs, a.Address)
	}
	for _, a := range cc {
		recipientStrs = append(recipientStrs, a.Address)
	}

	// Pass the Gmail-native thread ID (empty for non-Gmail / IMAP-learned
	// threads) so the Gmail API files the reply inside the original
	// conversation in the sender's mailbox instead of starting a new one.
	serverID, err := ec.Sender.Send(ctx, mimeBytes, fromAddr, recipientStrs, thread.GmailThreadID)
	if err != nil {
		_ = postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Send failed", err.Error())
		return nil, fmt.Errorf("send: %w", err)
	}

	// Gmail API may rewrite the Message-ID server-side; if so, prefer that
	// value as the dedup key (it's what the IMAP IDLE echo will carry).
	dedupKey := strings.Trim(om.MessageID, "<>")
	if serverID != "" {
		dedupKey = strings.Trim(serverID, "<>")
	}

	if err := postSendReceiptToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, fromAddr, to, cc, dropped); err != nil {
		// The receipt is the only channel telling the user a chat-shaped reply
		// went out as a reply-all. Its silence must not also be silent.
		ec.UserLogin.Log.Warn().Err(err).Strs("to", recipientStrs).
			Msg("send receipt not posted; the user has no record of who this went to")
	}

	matrixEvtID := ""
	if msg.Event != nil {
		matrixEvtID = string(msg.Event.ID)
	}

	if ec.Main.SentDedup != nil {
		if err := ec.Main.SentDedup.Record(ctx, string(ec.UserLogin.ID), dedupKey, matrixEvtID); err != nil {
			ec.UserLogin.Log.Warn().Err(err).Msg("dedup record failed; outbound may double-post on Sent IDLE echo")
		}
	}

	// Best-effort APPEND to Sent so users see their outbound in Gmail web /
	// iOS Mail without waiting for SMTP->IMAP propagation. Skip for the
	// Gmail API path because the API already filed the message in Sent.
	if ec.Sender.Provider() == "smtp" && ec.IMAPClient != nil {
		if err := ec.IMAPClient.AppendToSentFolder(ctx, mimeBytes); err != nil {
			ec.UserLogin.Log.Warn().Err(err).Msg("append to Sent failed; message will appear after SMTP->IMAP propagation")
		}
	}

	// Update thread cache so subsequent replies in the same room thread
	// against this newly-sent message rather than against the previous tail.
	thread.References = append(thread.References, dedupKey)
	thread.MessageID = dedupKey
	thread.LastOutboundMessageID = dedupKey
	if wasDraft {
		// First-send conversion: thread is now a real thread, not a draft.
		// Subsequent messages should produce In-Reply-To/References headers
		// pointing at this message.
		thread.IsDraft = false
	}
	if ec.Main.ThreadManager != nil {
		ec.Main.ThreadManager.CacheForReceiver(string(ec.UserLogin.ID), thread)
	}

	// Phase D: persist the updated thread state to Portal.Metadata so a 24h
	// ThreadManager TTL eviction (or a bridge restart) doesn't lose the
	// References chain. Best-effort; failure is logged but not fatal — the
	// in-memory cache is still good for at least 24h.
	if err := PersistThreadState(ctx, msg.Portal, thread); err != nil {
		ec.UserLogin.Log.Warn().Err(err).Msg("save portal metadata failed; thread state will only persist via ThreadManager cache (24h TTL)")
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        common.EmailToMessageID(dedupKey),
			SenderID:  MakeUserID(ec.Email),
			Timestamp: time.Now(),
		},
	}, nil
}

// deriveSubjectFromBody extracts a subject from the first line of a Matrix
// message body. Used as a fallback for compose threads where the user never
// supplied an explicit subject:"..." argument. We cap the length at 78 chars
// (the soft RFC 5322 line-length recommendation, minus headroom for a "Re:"
// prefix downstream) and split on either a blank line or a single newline so
// multi-paragraph messages don't dump the whole body into Subject.
func deriveSubjectFromBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "(no subject)"
	}
	// Take everything up to the first newline.
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[:nl]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "(no subject)"
	}
	const maxLen = 78
	if len(body) > maxLen {
		body = body[:maxLen]
	}
	return body
}

// resolveThreadForPortal looks up the EmailThread that backs this Matrix
// portal. Stripped portal IDs are of the form "thread:<message-id>". If the
// thread isn't in the in-memory cache (bridge restart, never received in this
// room, etc.), fall back to reconstructing from Portal.Metadata so cold-cache
// rooms remain usable for outbound replies.
func (ec *EmailClient) resolveThreadForPortal(portalID networkid.PortalID) (*email.EmailThread, error) {
	threadID := strings.TrimPrefix(string(portalID), "thread:")
	if threadID == "" {
		return nil, errors.New("matrimail: portal has no thread ID")
	}
	if ec.Main.ThreadManager == nil {
		return nil, errors.New("matrimail: ThreadManager not initialized")
	}
	thread := ec.Main.ThreadManager.GetThreadByID(string(ec.UserLogin.ID), threadID)
	if thread != nil {
		return thread, nil
	}
	return nil, fmt.Errorf("matrimail: thread %s not found in cache (re-receive a message in this room to re-cache, or this is a stale room)", threadID)
}

// resolveThreadForPortalWithMetadata is resolveThreadForPortal extended to
// rehydrate the thread from Portal.Metadata when the ThreadManager misses, and
// to complete a cached thread that lacks inbound reply context from it.
// Reseeds the cache so subsequent operations on the same room avoid the
// round trip.
func (ec *EmailClient) resolveThreadForPortalWithMetadata(portal *bridgev2.Portal) (*email.EmailThread, error) {
	if portal == nil {
		return nil, errors.New("matrimail: nil portal")
	}
	threadID := strings.TrimPrefix(string(portal.ID), "thread:")
	pm := storedPortalMetadata(portal)
	hasMetadata := pm != nil && pm.ThreadID != "" && pm.ThreadID == threadID
	if thread, err := ec.resolveThreadForPortal(portal.ID); err == nil {
		if hasMetadata {
			fillInboundContext(thread, pm)
		}
		return thread, nil
	}
	if !hasMetadata {
		return nil, fmt.Errorf("matrimail: thread %s not found in cache and no portal metadata to restore from", threadID)
	}
	thread := ThreadFromPortalMetadata(pm)
	ec.Main.ThreadManager.CacheForReceiver(string(ec.UserLogin.ID), thread)
	return thread, nil
}

// computeReplyChain returns (in-reply-to, references) headers for an outbound
// reply. Honors an explicit Matrix reply when present; otherwise falls back
// to the thread tail.
func computeReplyChain(thread *email.EmailThread, replyTo *database.Message) (string, []string) {
	references := append([]string(nil), thread.References...)
	if replyTo != nil {
		parentID := common.MessageIDFromNetworkID(replyTo.ID)
		references = append(references, parentID)
		return parentID, references
	}
	if thread.MessageID != "" {
		references = append(references, thread.MessageID)
		return thread.MessageID, references
	}
	return "", references
}

// resolveReplyAllRecipients computes (To, Cc) for a reply-all outbound:
//   - To = unique(thread.LastFrom + thread.LastTo) minus selves
//   - Cc = unique(thread.LastCc) minus selves and minus addresses already in To
//
// When thread.LastFrom is empty (first send on a compose thread, or restored
// thread with no inbound), fall back to the old behavior: use
// thread.Participants minus selves as To, no Cc.
//
// selves must be a slice of lowercased addresses (primary + aliases).
func resolveReplyAllRecipients(thread *email.EmailThread, selves []string) ([]netmail.Address, []netmail.Address, []string, error) {
	if thread == nil {
		return nil, nil, nil, errors.New("matrimail: nil thread")
	}
	selfSet := selvesSet(selves)
	var dropped []string
	take := func(raw string, dst *[]netmail.Address, seen map[string]bool) {
		a, ok, unparseable := parseAddrIfAllowed(raw, selfSet, seen)
		switch {
		case ok:
			*dst = append(*dst, a)
		case unparseable:
			dropped = append(dropped, raw)
		}
	}

	// Compose-thread / restored-thread fallback path.
	if strings.TrimSpace(thread.LastFrom) == "" {
		var to []netmail.Address
		seen := map[string]bool{}
		for _, p := range thread.Participants {
			take(p, &to, seen)
		}
		if len(to) == 0 {
			return nil, nil, dropped, noRecipientsError("thread participants empty after self-exclusion", dropped)
		}
		return to, nil, dropped, nil
	}

	var to []netmail.Address
	seen := map[string]bool{}
	take(thread.LastFrom, &to, seen)
	for _, p := range thread.LastTo {
		take(p, &to, seen)
	}
	var cc []netmail.Address
	for _, p := range thread.LastCc {
		take(p, &cc, seen)
	}
	if len(to) == 0 && len(cc) == 0 {
		return nil, nil, dropped, noRecipientsError("reply-all set empty after self-exclusion", dropped)
	}
	return to, cc, dropped, nil
}

// resolveDMRecipients computes the recipient set for a DM-mode reply: only
// thread.LastFrom (the sender of the most recent inbound). Returns an error
// when LastFrom is empty (no inbound to reply to).
func resolveDMRecipients(thread *email.EmailThread, selves []string) ([]netmail.Address, error) {
	if thread == nil {
		return nil, errors.New("matrimail: nil thread")
	}
	if strings.TrimSpace(thread.LastFrom) == "" {
		return nil, errors.New("matrimail: DM mode requested but thread has no LastFrom (no inbound to reply to)")
	}
	selfSet := selvesSet(selves)
	seen := map[string]bool{}
	if a, ok, _ := parseAddrIfAllowed(thread.LastFrom, selfSet, seen); ok {
		return []netmail.Address{a}, nil
	}
	return nil, errors.New("matrimail: DM mode target was either unparseable or one of the user's own addresses")
}

// selvesSet converts a slice of lowercased addresses into a lookup set.
func selvesSet(selves []string) map[string]bool {
	out := make(map[string]bool, len(selves))
	for _, s := range selves {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

// noRecipientsError explains an empty recipient set. When every candidate
// failed to parse, "empty after self-exclusion" is simply false and hides the
// evidence, so the unparseable entries are named instead.
func noRecipientsError(reason string, dropped []string) error {
	if len(dropped) > 0 {
		return fmt.Errorf("matrimail: no recipients — %d address(es) on this thread could not be parsed: %s",
			len(dropped), strings.Join(dropped, ", "))
	}
	return fmt.Errorf("matrimail: no recipients (%s)", reason)
}

// parseAddrIfAllowed parses a "Name <addr>" / "addr" string, filters self
// addresses, and dedupes against `seen` (lowercased addr-key). Returns
// (Address, true) when the entry should be included.
//
// A parse failure means a recipient the thread listed is being dropped, which
// is materially different from filtering out ourselves or a duplicate. It is
// returned separately so the caller can tell the user rather than lose them
// silently -- the failure this whole file exists to stop.
func parseAddrIfAllowed(raw string, selves, seen map[string]bool) (addr netmail.Address, ok bool, unparseable bool) {
	a, err := netmail.ParseAddress(raw)
	if err != nil {
		return netmail.Address{}, false, true
	}
	key := strings.ToLower(a.Address)
	if selves[key] {
		return netmail.Address{}, false, false
	}
	if seen[key] {
		return netmail.Address{}, false, false
	}
	seen[key] = true
	return *a, true, false
}

// pickFromAddress returns (address, displayName) for the outbound From
// header. Prefers thread.LastDeliveredTo when it matches one of the user's
// aliases; otherwise falls back to the primary address. Display name is
// pulled from the matching SendAsAliases entry when available.
func pickFromAddress(ec *EmailClient, thread *email.EmailThread, selves map[string]bool) (string, string) {
	primary := ec.Email
	if thread == nil || strings.TrimSpace(thread.LastDeliveredTo) == "" {
		return primary, ""
	}
	want := strings.ToLower(strings.TrimSpace(thread.LastDeliveredTo))
	if !selves[want] {
		return primary, ""
	}
	for _, sa := range ec.SendAsAliases {
		if strings.EqualFold(sa.Email, want) {
			return sa.Email, sa.Name
		}
	}
	// Alias matched the primary or wasn't in the SendAsAliases list; still
	// honor the address but with no display name override.
	return thread.LastDeliveredTo, ""
}

// pickFromAddress overloads via slice-of-strings selves to match the call
// site that builds a slice rather than a map.
func pickFromAddressFromSlice(ec *EmailClient, thread *email.EmailThread, selves []string) (string, string) {
	return pickFromAddress(ec, thread, selvesSet(selves))
}

// buildOutgoingMessage assembles the OutgoingMessage from the Matrix event
// content and resolved threading metadata. Re: prefixing matches RFC 5322
// convention (case-insensitive check so we don't double-prefix).
func buildOutgoingMessage(fromAddr, fromName string, thread *email.EmailThread, msg *bridgev2.MatrixMessage, inReplyTo string, references []string, to, cc []netmail.Address) *email.OutgoingMessage {
	domain := ""
	if at := strings.LastIndex(fromAddr, "@"); at >= 0 {
		domain = fromAddr[at+1:]
	}
	newMsgID := strings.Trim(email.GenerateMessageID(domain), "<>")

	// Subject: normalize to a single "Re: " prefix on replies (no "Re: Re: …"
	// chains), and leave a first-send subject untouched. NormalizeReplySubject
	// strips every leading Re:/RE:/re:/Re[N]: and prepends exactly one "Re: ".
	// "Reply" here means anything threading into an existing message — either
	// an explicit Matrix reply (msg.ReplyTo set) or a plain send into a thread
	// that already has a tail (inReplyTo populated by computeReplyChain).
	isReply := inReplyTo != ""
	subject := thread.Subject
	if isReply {
		subject = email.NormalizeReplySubject(subject)
	}

	textBody := msg.Content.Body
	htmlBody := ""
	if msg.Content.Format == event.FormatHTML {
		htmlBody = msg.Content.FormattedBody
	}

	// Gmail-style quote: when this is a reply and we know the parent's body,
	// append "On <date>, <sender> wrote:" followed by a `>`-quoted parent body
	// (text), and a <div class="gmail_quote"> block (HTML). The parent body is
	// captured on the inbound path in EmailThread.LastTextBody / LastHTMLBody.
	if isReply && (thread.LastTextBody != "" || thread.LastHTMLBody != "") {
		quoteText := email.FormatGmailQuoteText(thread.LastDate, thread.LastFrom, thread.LastTextBody)
		if quoteText != "" {
			if textBody != "" {
				textBody = textBody + "\n\n" + quoteText
			} else {
				textBody = quoteText
			}
		}
		if thread.LastHTMLBody != "" {
			quoteHTML := email.FormatGmailQuoteHTML(thread.LastDate, thread.LastFrom, thread.LastHTMLBody)
			if quoteHTML != "" {
				if htmlBody != "" {
					htmlBody = `<div dir="ltr">` + htmlBody + `</div><br>` + quoteHTML
				} else {
					// No HTML alternative from Matrix side, but the parent had
					// HTML — promote the plain reply body to HTML so the
					// recipient's client renders the gmail_quote properly.
					htmlBody = `<div dir="ltr">` + htmlEscapeOutbound(msg.Content.Body) + `</div><br>` + quoteHTML
				}
			}
		}
	}

	return &email.OutgoingMessage{
		MessageID:  newMsgID,
		From:       netmail.Address{Name: fromName, Address: fromAddr},
		To:         to,
		Cc:         cc,
		Subject:    subject,
		Date:       time.Now(),
		InReplyTo:  inReplyTo,
		References: references,
		TextBody:   textBody,
		HTMLBody:   htmlBody,
	}
}

// htmlEscapeOutbound escapes the user's plain-text reply body before
// embedding it in the HTML half of a Gmail-quote wrapper. We only escape the
// four HTML metacharacters and convert newlines to <br>; anything richer
// would have arrived via Matrix's FormatHTML path already.
func htmlEscapeOutbound(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"\n", "<br>",
	)
	return r.Replace(s)
}

// downloadMediaAsAttachment pulls a Matrix media payload (encrypted or not)
// into a single EmailAttachment ready to be appended to an OutgoingMessage.
func (ec *EmailClient) downloadMediaAsAttachment(ctx context.Context, content *event.MessageEventContent) (*email.EmailAttachment, error) {
	if ec.UserLogin == nil || ec.UserLogin.Bridge == nil || ec.UserLogin.Bridge.Bot == nil {
		return nil, errors.New("matrimail: bridge bot intent not available")
	}
	data, err := ec.UserLogin.Bridge.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, err
	}
	mime := ""
	if content.Info != nil {
		mime = content.Info.MimeType
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return &email.EmailAttachment{
		Filename:    content.GetFileName(),
		ContentType: mime,
		Size:        int64(len(data)),
		Data:        data,
		Disposition: "attachment",
	}, nil
}

// checkReplyTargetResolvable refuses an outbound whose Matrix reply points at
// a message other than the one the thread's Last* fields describe.
//
// Recipients for a reply are resolved from thread.LastFrom/LastTo/LastCc,
// which track the most recent inbound. An explicit reply to any older message
// was previously still addressed from that newest state -- so replying to a
// funder's question in a long thread went out to whoever happened to be on the
// latest message, quoting an unrelated one. Per-message recipients are not
// stored anywhere, so they cannot be recovered; refusing is the only honest
// option. Replying to our own most recent send is fine: the Last* fields still
// describe the correct inbound to answer. The check uses LastInboundMessageID
// rather than MessageID because MessageID is the newest message in either
// direction, while the Last* fields describe the newest inbound specifically.
func checkReplyTargetResolvable(thread *email.EmailThread, replyTo *database.Message) error {
	if thread == nil || replyTo == nil {
		return nil
	}
	target := common.MessageIDFromNetworkID(replyTo.ID)
	if target == "" {
		return nil
	}
	if target == thread.LastInboundMessageID || (thread.LastOutboundMessageID != "" && target == thread.LastOutboundMessageID) {
		return nil
	}
	if thread.LastInboundMessageID == "" {
		// Nothing to compare against (restored thread predating this field).
		// Fall through rather than refuse every reply in an old room.
		return nil
	}
	return errors.New("matrimail: replying to an older message in this thread is not supported — " +
		"its recipients are not stored, and answering from the newest message's recipients could " +
		"reach people who were not on the message you replied to. Reply to the most recent message instead")
}
