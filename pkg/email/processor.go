package email

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	netmail "net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Leicas/matrimail/pkg/common"
	logging "github.com/Leicas/matrimail/pkg/logging"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Matrix content size limits and thresholds
const (
	// MaxMatrixContentSize is the conservative limit for Matrix events (48 KiB)
	// Matrix rejects events > 64 KiB after encryption, so we use a conservative cap
	MaxMatrixContentSize = 48 * 1024

	// HTMLMinificationTarget is the target size for HTML minification (24 KiB)
	// Conservative target to account for encryption overhead
	HTMLMinificationTarget = 24 * 1024

	// PerEventTarget is the conservative per-event target for chunked content (16 KiB)
	// Use conservative target to account for encryption overhead
	PerEventTarget = 16 * 1024
)

// Pre-compiled regex patterns for performance
var (
	reImgCidSrc = regexp.MustCompile(`(?i)(src\s*=\s*)(['"])\s*cid:([^'"\s)]+)`)
	reCSSCidURL = regexp.MustCompile(`(?i)url\(\s*cid:([^) \t\r\n]+)\s*\)`)
)

// DedupChecker reports whether a given (receiver, messageID) pair was sent by
// us — i.e. we issued an outbound send for it and the IMAP IDLE just echoed
// it back from the Sent folder. The connector wires a concrete implementation
// at startup; the field is nil-tolerant so tests and the legacy single-direction
// path keep working.
type DedupChecker interface {
	IsOurMessage(ctx context.Context, receiver, messageID string) (bool, error)
}

// AliasResolver returns the bridge user's known addresses (primary + send-as
// aliases, all lowercased) for the given userLogin receiver. An empty slice
// indicates no alias info is available — processor falls back to treating
// every recipient as "other".
type AliasResolver func(receiver string) []string

// ErrorNotifier is called when processing surfaces a user-visible problem
// (body parse failure, attachment failure) that should be reported to the
// affected room. The connector wires a concrete implementation; nil-tolerant.
type ErrorNotifier interface {
	NotifyProcessingError(ctx context.Context, receiver, messageID, subject, kind string, err error)
}

// Processor handles the complete email processing pipeline
type Processor struct {
	log           *zerolog.Logger
	threadManager *ThreadManager

	sanitized bool
	secret    string

	// MaxUploadBytes limits individual media uploads to Matrix. Items larger than this
	// will either be gzipped (for text/html and text/plain bodies) or skipped with a notice.
	MaxUploadBytes int
	// When true, attempt gzip for oversized original email bodies before giving up.
	GzipLargeBodies bool

	// dedupChecker is consulted on Sent-mailbox messages to suppress our own
	// outbound echoes. nil means dedup is disabled (legacy / inbound-only mode).
	dedupChecker DedupChecker

	// aliasResolver returns the bridge user's primary + alias addresses.
	// Used to detect DeliveredTo on inbound mail so replies preserve the
	// alias. nil means aliases are unknown (legacy / non-Gmail).
	aliasResolver AliasResolver

	// errorNotifier surfaces user-visible processing errors back into the
	// affected Matrix room. nil-tolerant.
	errorNotifier ErrorNotifier
}

// SetDedupChecker wires a DedupChecker into the processor. Called once at
// startup from EmailConnector.Init.
func (p *Processor) SetDedupChecker(d DedupChecker) { p.dedupChecker = d }

// SetAliasResolver wires an AliasResolver into the processor. Called once at
// startup from EmailConnector.Init.
func (p *Processor) SetAliasResolver(r AliasResolver) { p.aliasResolver = r }

// SetErrorNotifier wires an ErrorNotifier into the processor.
func (p *Processor) SetErrorNotifier(n ErrorNotifier) { p.errorNotifier = n }

// pickDeliveredTo returns the first address in to/cc that matches one of the
// user's aliases (case-insensitive), preserving the original casing/formatting
// from the inbound headers. Empty string when nothing matches.
func pickDeliveredTo(to, cc, aliases []string) string {
	if len(aliases) == 0 {
		return ""
	}
	lookup := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			lookup[a] = true
		}
	}
	check := func(list []string) string {
		for _, raw := range list {
			addr := extractEmailAddress(raw)
			if addr == "" {
				continue
			}
			if lookup[strings.ToLower(addr)] {
				return addr
			}
		}
		return ""
	}
	if hit := check(to); hit != "" {
		return hit
	}
	if hit := check(cc); hit != "" {
		return hit
	}
	return ""
}

// NewProcessor creates a new email processor
func NewProcessor(log *zerolog.Logger, threadManager *ThreadManager, sanitized bool, secret string) *Processor {
	logger := log.With().Str("component", "email_processor").Logger()
	return &Processor{
		log:             &logger,
		threadManager:   threadManager,
		sanitized:       sanitized,
		secret:          secret,
		MaxUploadBytes:  0, // set by connector; 0 means unlimited unless overridden
		GzipLargeBodies: true,
	}
}

// EmailMessage represents a complete parsed email ready for Matrix bridging
type EmailMessage struct {
	*ParsedEmail
	Thread      *EmailThread
	PortalKey   networkid.PortalKey
	MessageID   networkid.MessageID
	Timestamp   time.Time
	IsOutbound  bool // True if this email was sent by the bridge user
	Attachments []*EmailAttachment
}

// ProcessIMAPMessage processes an IMAP FetchMessageData and converts it to Matrix events
func (p *Processor) ProcessIMAPMessage(ctx context.Context, fetchData *imapclient.FetchMessageData, userLogin *bridgev2.UserLogin, mailbox string) (*EmailMessage, error) {
	p.log.Info().
		Uint32("seq_num", fetchData.SeqNum).
		Msg("Processing IMAP message")

	// Parse the IMAP fetch data into a ParsedEmail
	parsedEmail, err := p.parseIMAPFetchData(fetchData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse IMAP fetch data: %w", err)
	}
	// Drafts are skipped: they have unstable Message-IDs (re-generated on every
	// save) and a self-From address that causes them to surface in Matrix as
	// messages from a third-party ghost — i.e. "a reply by someone" — rather
	// than as drafts. The user composes drafts in their email client; they
	// only appear in Matrix once actually sent (at which point the \Draft flag
	// is gone and the message takes a normal Message-ID).
	if parsedEmail.IsDraft {
		p.log.Debug().Str("message_id", parsedEmail.MessageID).Str("mailbox", mailbox).Msg("Skipping draft message (\\Draft flag)")
		return nil, nil
	}
	return p.ProcessParsedEmail(ctx, parsedEmail, userLogin, mailbox)
}

// ProcessParsedEmail is the post-parse pipeline shared by the IMAP and Gmail
// API ingestion paths: validation → sent-folder dedup → threading → portal-key
// → attribution → EmailMessage assembly. Callers are responsible for
// producing the ParsedEmail; this method handles everything downstream.
//
// Returns (nil, nil) — no error, no message to forward — when the message is
// our own outbound echo and matches the sent-folder dedup table.
func (p *Processor) ProcessParsedEmail(ctx context.Context, parsedEmail *ParsedEmail, userLogin *bridgev2.UserLogin, mailbox string) (*EmailMessage, error) {
	if parsedEmail == nil {
		return nil, fmt.Errorf("ProcessParsedEmail: nil parsedEmail")
	}
	// Guard against degraded parse that lacks basic identity/threading info
	if strings.TrimSpace(parsedEmail.From) == "" {
		return nil, fmt.Errorf("degraded parse: missing sender information")
	}
	if strings.TrimSpace(parsedEmail.MessageID) == "" {
		return nil, fmt.Errorf("degraded parse: missing message-id header")
	}
	// Validate MessageID format to prevent injection attacks
	if len(parsedEmail.MessageID) > 998 { // RFC 5322 line length limit
		return nil, fmt.Errorf("degraded parse: message-id exceeds RFC limits")
	}

	if p.sanitized {
		p.log.Debug().
			Str("message_id_hash", logging.HashHMAC(parsedEmail.MessageID, p.secret, 10)).
			Str("subject_hash", logging.HashHMAC(parsedEmail.Subject, p.secret, 10)).
			Str("from_masked", logging.MaskEmail(parsedEmail.From)).
			Msg("Successfully parsed email message")
	} else {
		p.log.Debug().
			Str("message_id", parsedEmail.MessageID).
			Str("subject", parsedEmail.Subject).
			Str("from", parsedEmail.From).
			Msg("Successfully parsed email message")
	}

	// Step 0: Sent-folder dedup. If we just received an echo of a message we
	// ourselves sent (recorded by HandleMatrixMessage in the connector),
	// short-circuit before threading + portal work.
	if p.dedupChecker != nil && p.isOutboundMessage(mailbox) {
		if hit, derr := p.dedupChecker.IsOurMessage(ctx, string(userLogin.ID), parsedEmail.MessageID); derr != nil {
			p.log.Warn().Err(derr).Msg("Dedup check failed; falling through and processing message normally")
		} else if hit {
			if p.sanitized {
				p.log.Debug().
					Str("message_id_hash", logging.HashHMAC(parsedEmail.MessageID, p.secret, 10)).
					Str("mailbox", mailbox).
					Msg("Skipping our own outbound message (Sent-folder dedup)")
			} else {
				p.log.Debug().
					Str("message_id", parsedEmail.MessageID).
					Str("mailbox", mailbox).
					Msg("Skipping our own outbound message (Sent-folder dedup)")
			}
			return nil, nil
		}
	}

	// Resolve DeliveredTo (which alias this inbound was addressed to) before
	// threading so the resulting thread sticks the alias for outbound use.
	receiver := string(userLogin.ID)
	if p.aliasResolver != nil {
		if aliases := p.aliasResolver(receiver); len(aliases) > 0 {
			parsedEmail.DeliveredTo = pickDeliveredTo(parsedEmail.To, parsedEmail.Cc, aliases)
		}
	}

	// Step 1: Determine thread membership (scoped by receiver)
	thread := p.threadManager.DetermineThread(receiver, parsedEmail)
	p.threadManager.CacheForReceiver(receiver, thread)
	if thread == nil {
		return nil, fmt.Errorf("failed to determine thread for message %s", parsedEmail.MessageID)
	}

	// Step 2: Create portal key for this thread
	portalKey := networkid.PortalKey{
		ID:       networkid.PortalID(fmt.Sprintf("thread:%s", thread.ThreadID)),
		Receiver: userLogin.ID,
	}

	// Step 3: Create network message ID
	networkMessageID := networkid.MessageID(fmt.Sprintf("email:%s", parsedEmail.MessageID))

	// Step 4: Check if this is an outbound message
	isOutbound := p.isOutboundMessage(mailbox)

	if p.sanitized {
		p.log.Debug().
			Str("mailbox", mailbox).
			Bool("is_outbound", isOutbound).
			Str("from_masked", logging.MaskEmail(parsedEmail.From)).
			Str("ghost_masked", logging.MaskEmail(string(common.EmailToGhostID(extractEmailAddress(parsedEmail.From))))).
			Msg("Attribution decision")
	} else {
		p.log.Debug().
			Str("mailbox", mailbox).
			Bool("is_outbound", isOutbound).
			Str("from", parsedEmail.From).
			Str("ghost", string(common.EmailToGhostID(extractEmailAddress(parsedEmail.From)))).
			Msg("Attribution decision")
	}

	emailMessage := &EmailMessage{
		ParsedEmail: parsedEmail,
		Thread:      thread,
		PortalKey:   portalKey,
		MessageID:   networkMessageID,
		Timestamp:   parsedEmail.Date,
		IsOutbound:  isOutbound,
	}

	if p.sanitized {
		p.log.Info().
			Str("thread_id", logging.HashHMAC(thread.ThreadID, p.secret, 10)).
			Str("portal_key", logging.HashHMAC(string(portalKey.ID), p.secret, 10)).
			Bool("is_outbound", isOutbound).
			Msg("Successfully processed complete email message")
	} else {
		p.log.Info().
			Str("thread_id", thread.ThreadID).
			Str("portal_key", string(portalKey.ID)).
			Bool("is_outbound", isOutbound).
			Msg("Successfully processed complete email message")
	}

	emailMessage.Attachments = parsedEmail.Attachments
	return emailMessage, nil
}

// parseIMAPFetchData parses IMAP fetch data into a ParsedEmail struct
func (p *Processor) parseIMAPFetchData(fetchData *imapclient.FetchMessageData) (*ParsedEmail, error) {
	// Collect the fetch data into a buffer
	buf, err := fetchData.Collect()
	if err != nil {
		return nil, fmt.Errorf("failed to collect fetch data: %w", err)
	}

	// Initialize parsed email with basic information from IMAP fetch data
	parsedEmail := &ParsedEmail{
		MessageID: fmt.Sprintf("uid-%d", buf.UID), // Fallback if no Message-ID found
		Date:      time.Now(),                     // Fallback if no date found
	}

	// Surface the \Draft flag so ProcessIMAPMessage can drop drafts before
	// they leak into the Matrix room.
	for _, f := range buf.Flags {
		if f == imap.FlagDraft {
			parsedEmail.IsDraft = true
			break
		}
	}

	// Extract data from envelope if available
	if buf.Envelope != nil {
		env := buf.Envelope

		// Extract Message-ID
		if env.MessageID != "" {
			parsedEmail.MessageID = cleanMessageID(env.MessageID)
		}

		// Extract subject
		if env.Subject != "" {
			parsedEmail.Subject = env.Subject
		}

		// Extract In-Reply-To
		if len(env.InReplyTo) > 0 {
			parsedEmail.InReplyTo = cleanMessageID(env.InReplyTo[0])
		}

		// Extract date
		if !env.Date.IsZero() {
			parsedEmail.Date = env.Date
		}

		// Extract sender
		if len(env.From) > 0 {
			parsedEmail.From = formatIMAPAddress(&env.From[0])
		}

		// Extract recipients
		parsedEmail.To = formatIMAPAddressSlice(env.To)
		parsedEmail.Cc = formatIMAPAddressSlice(env.Cc)
		parsedEmail.Bcc = formatIMAPAddressSlice(env.Bcc)
	}

	// Parse body sections for text and HTML content
	// TODO: thread receiver/login through parseIMAPFetchData so we can call
	// p.errorNotifier.NotifyProcessingError on these failures and surface
	// them into the affected room instead of the management room.
	textContent, htmlContent, err := p.parseMessageBody(buf)
	if err != nil {
		p.log.Warn().Err(err).Msg("Failed to parse message body, using fallback")
		textContent = "[Failed to parse message content]"
	}

	parsedEmail.TextContent = textContent
	parsedEmail.HTMLContent = htmlContent

	// Extract attachments if present
	attachments, err := p.extractAttachments(buf)
	if err != nil {
		p.log.Warn().Err(err).Msg("Failed to extract attachments")
	} else if len(attachments) > 0 {
		p.log.Debug().Int("count", len(attachments)).Msg("Extracted attachments from email")
	}
	parsedEmail.Attachments = attachments

	// Extract References from headers if body sections are available
	references := p.extractReferencesFromHeaders(buf)
	if len(references) > 0 {
		parsedEmail.References = references
	}

	return parsedEmail, nil
}

// parseMessageBody extracts text and HTML content from IMAP body sections
func (p *Processor) parseMessageBody(buf *imapclient.FetchMessageBuffer) (textContent, htmlContent string, err error) {
	// High-level: log the number of body sections
	p.log.Debug().Int("body_section_count", len(buf.BodySection)).Msg("Parsing message body sections")
	// Prefer MIME parsing for any non-header sections to avoid dumping boundaries/headers
	for idx, section := range buf.BodySection {
		p.log.Trace().
			Int("section_index", idx).
			Str("specifier", string(section.Section.Specifier)).
			Int("bytes", len(section.Bytes)).
			Msg("Processing body section")
		if len(section.Bytes) == 0 {
			continue
		}
		if section.Section.Specifier == imap.PartSpecifierHeader {
			continue
		}
		text, html := p.parseMIMEContent(section.Bytes)
		if text != "" && textContent == "" {
			textContent = text
		}
		if html != "" && htmlContent == "" {
			htmlContent = html
		}
		if textContent != "" && htmlContent != "" {
			break
		}
	}

	// If we don't have text/plain but we do have HTML, derive a simple plaintext fallback
	if textContent == "" && htmlContent != "" {
		textContent = simpleHTMLToText(htmlContent)
	}
	// If still no text content found, provide a fallback
	if textContent == "" {
		textContent = "[No readable text content found]"
	}
	p.log.Debug().
		Int("text_len", len(textContent)).
		Int("html_len", len(htmlContent)).
		Msg("Finished parsing message body")

	return textContent, htmlContent, nil
}

// parseMIMEContent attempts to parse MIME content from raw body data
func (p *Processor) parseMIMEContent(data []byte) (textContent, htmlContent string) {
	// Try to parse as a complete email message
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		// If not a complete message, heuristically detect multipart boundary
		if boundary := detectBoundary(data); boundary != "" {
			return p.parseMultipartContent(bytes.NewReader(data), boundary)
		}
		// Fallback as raw text
		return string(data), ""
	}

	// Content-Transfer-Encoding may require decoding even for single-part messages
	cte := msg.Header.Get("Content-Transfer-Encoding")

	// Check Content-Type header
	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		// No content type: try boundary heuristic with standard email size limit
		limitedReader := io.LimitReader(decodeBody(msg.Body, cte), 25*1024*1024) // 25MB standard limit
		raw, _ := io.ReadAll(limitedReader)
		if boundary := detectBoundary(raw); boundary != "" {
			return p.parseMultipartContent(bytes.NewReader(raw), boundary)
		}
		// Fallback: plain text (decoded)
		return string(raw), ""
	}

	// Parse media type
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Fallback to plain text (decoded) with standard email size limit
		limitedReader := io.LimitReader(decodeBody(msg.Body, cte), 25*1024*1024) // 25MB standard limit
		body, _ := io.ReadAll(limitedReader)
		return string(body), ""
	}

	switch {
	case strings.HasPrefix(mediaType, "text/plain"):
		limitedReader := io.LimitReader(decodeBody(msg.Body, cte), 25*1024*1024) // 25MB standard limit
		body, _ := io.ReadAll(limitedReader)
		return string(body), ""
	case strings.HasPrefix(mediaType, "text/html"):
		limitedReader := io.LimitReader(decodeBody(msg.Body, cte), 25*1024*1024) // 25MB standard limit
		body, _ := io.ReadAll(limitedReader)
		return "", string(body)
	case strings.HasPrefix(mediaType, "multipart/"):
		// Handle multipart messages
		return p.parseMultipartContent(msg.Body, params["boundary"])
	default:
		// Unknown content type, try to read as text (decoded) with standard email size limit
		limitedReader := io.LimitReader(decodeBody(msg.Body, cte), 25*1024*1024) // 25MB standard limit
		body, _ := io.ReadAll(limitedReader)
		return string(body), ""
	}
}

// maxMultipartDepth bounds how far the two multipart parsers will recurse.
// Nesting costs an attacker about fifty bytes per level, and each level reads
// the remaining bytes, so an unbounded parser turns a small message into
// quadratic work and eventually a stack overflow -- which no recover() can
// catch. Real mail nests three or four deep; twenty is far past anything
// legitimate.
const maxMultipartDepth = 20

// maxPartsPerLevel bounds the breadth of a single multipart container for the
// same reason depth is bounded: the per-part size limit says nothing about how
// many parts there are.
const maxPartsPerLevel = 200

// parseMultipartContent parses multipart MIME content.
func (p *Processor) parseMultipartContent(body io.Reader, boundary string) (textContent, htmlContent string) {
	return p.parseMultipartContentDepth(body, boundary, 0)
}

func (p *Processor) parseMultipartContentDepth(body io.Reader, boundary string, depth int) (textContent, htmlContent string) {
	if boundary == "" {
		return "[Multipart message with no boundary]", ""
	}
	if depth >= maxMultipartDepth {
		p.log.Warn().Int("depth", depth).Msg("multipart nesting limit reached; not recursing further")
		return truncationNotice("This message is nested more than " + strconv.Itoa(maxMultipartDepth) + " levels deep and was not fully parsed. Open it in Gmail to read the original."), ""
	}

	mr := multipart.NewReader(body, boundary)
	parts := 0
	truncatedParts := false
	for {
		if parts >= maxPartsPerLevel {
			p.log.Warn().Int("parts", parts).Msg("multipart part limit reached; ignoring the rest of this container")
			truncatedParts = true
			break
		}
		parts++
		part, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		// Decode part body according to Content-Transfer-Encoding with standard email size limit
		cte := strings.ToLower(part.Header.Get("Content-Transfer-Encoding"))
		decoded := decodeBody(part, cte)
		limitedReader := io.LimitReader(decoded, 25*1024*1024) // 25MB standard limit
		partData, err := io.ReadAll(limitedReader)
		part.Close()
		if err != nil {
			continue
		}

		// Check Content-Type of this part
		contentType := part.Header.Get("Content-Type")
		mediaType, params, _ := mime.ParseMediaType(contentType)
		p.log.Trace().
			Str("mime_media_type", mediaType).
			Msg("Multipart content: encountered part")

		switch {
		case strings.HasPrefix(mediaType, "multipart/"):
			// Recurse into nested multiparts (e.g., multipart/alternative inside multipart/mixed)
			childText, childHTML := p.parseMultipartContentDepth(bytes.NewReader(partData), params["boundary"], depth+1)
			if textContent == "" && childText != "" {
				textContent = childText
			}
			if htmlContent == "" && childHTML != "" {
				htmlContent = childHTML
			}
		case strings.HasPrefix(mediaType, "text/plain"):
			if textContent == "" {
				textContent = string(partData)
			}
		case strings.HasPrefix(mediaType, "text/html"):
			if htmlContent == "" {
				htmlContent = string(partData)
			}
		}
	}

	if truncatedParts {
		note := truncationNotice("This message had more than " + strconv.Itoa(maxPartsPerLevel) + " parts and was not fully parsed. Open it in Gmail to read the original.")
		if textContent == "" {
			textContent = note
		} else {
			textContent += "\n\n" + note
		}
	}

	return textContent, htmlContent
}

// truncationNotice formats a bridge-side limit as something a reader can act
// on. A truncated message that says nothing is indistinguishable from a short
// one, which is the failure mode these limits exist to avoid trading for.
func truncationNotice(what string) string {
	return "⚠️ " + what
}

// extractAttachments extracts attachments from IMAP body sections
func (p *Processor) extractAttachments(buf *imapclient.FetchMessageBuffer) ([]*EmailAttachment, error) {
	if buf == nil {
		return nil, nil
	}

	var attachments []*EmailAttachment
	p.log.Debug().Int("body_section_count", len(buf.BodySection)).Msg("Starting attachment extraction")

	// Look through body sections for attachments
	for idx, section := range buf.BodySection {
		p.log.Trace().
			Int("section_index", idx).
			Str("specifier", string(section.Section.Specifier)).
			Int("bytes", len(section.Bytes)).
			Msg("Examining section for attachments")
		if len(section.Bytes) == 0 {
			continue
		}

		// Check if this section could be an attachment
		if section.Section.Specifier != imap.PartSpecifierText &&
			section.Section.Specifier != imap.PartSpecifierHeader {

			// Try to parse as multipart content for attachments
			attachments = append(attachments, p.extractMultipartAttachments(section.Bytes)...)
		}
	}

	return attachments, nil
}

// extractMultipartAttachments extracts attachments from multipart content
func (p *Processor) extractMultipartAttachments(data []byte) []*EmailAttachment {
	var attachments []*EmailAttachment

	// Try to parse as a complete email message
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return attachments
	}

	// Check Content-Type header
	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		return attachments
	}

	// Parse media type
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return attachments
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		// Handle multipart messages for attachments
		boundary := params["boundary"]
		if boundary != "" {
			attachments = p.parseMultipartAttachments(msg.Body, boundary)
		}
	}

	return attachments
}

// parseMultipartAttachments parses multipart content for attachments.
func (p *Processor) parseMultipartAttachments(body io.Reader, boundary string) []*EmailAttachment {
	return p.parseMultipartAttachmentsDepth(body, boundary, 0)
}

func (p *Processor) parseMultipartAttachmentsDepth(body io.Reader, boundary string, depth int) []*EmailAttachment {
	var attachments []*EmailAttachment
	if depth >= maxMultipartDepth {
		p.log.Warn().Int("depth", depth).Msg("multipart nesting limit reached; not extracting deeper attachments")
		return attachments
	}

	mr := multipart.NewReader(body, boundary)
	parts := 0
	for {
		if parts >= maxPartsPerLevel {
			p.log.Warn().Int("parts", parts).Msg("multipart part limit reached; ignoring the rest of this container")
			break
		}
		parts++
		part, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				break
			}
			continue
		}

		contentDisposition := part.Header.Get("Content-Disposition")
		dispLower := strings.ToLower(strings.TrimSpace(contentDisposition))

		// Read and decode part body with standard email size limit
		cte := strings.ToLower(part.Header.Get("Content-Transfer-Encoding"))
		decoded := decodeBody(part, cte)
		limitedReader := io.LimitReader(decoded, 25*1024*1024) // 25MB standard limit
		dataBytes, err := io.ReadAll(limitedReader)
		part.Close()
		if err != nil {
			continue
		}

		// Determine content type and parameters
		ct := part.Header.Get("Content-Type")
		mediaType, params, _ := mime.ParseMediaType(ct)
		p.log.Trace().
			Str("mime_media_type", mediaType).
			Str("content_disposition", strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Disposition")))).
			Str("cte", cte).
			Int("decoded_bytes", len(dataBytes)).
			Msg("Attachment parsing: encountered part")
		if ct == "" {
			ct = "application/octet-stream"
			mediaType = ct
		}

		// Skip multipart container parts: recurse to find real parts
		// BUT: Don't recurse into quoted/forwarded content to avoid extracting old thread attachments
		if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
			childBoundary := params["boundary"]
			// Only recurse if this looks like current message structure, not quoted content
			if !p.isQuotedContent(dataBytes) {
				attachments = append(attachments, p.parseMultipartAttachmentsDepth(bytes.NewReader(dataBytes), childBoundary, depth+1)...)
			}
			continue
		}

		// Extract potential filename
		filename := ""
		if _, cdParams, err := mime.ParseMediaType(contentDisposition); err == nil {
			if fn := cdParams["filename"]; fn != "" {
				filename = fn
			}
		}

		// Inline-related headers
		contentID := normalizeCIDHeader(part.Header.Get("Content-ID"))
		contentLocation := strings.TrimSpace(part.Header.Get("Content-Location"))
		isInline := strings.Contains(dispLower, "inline")

		// Decide if this is a real attachment to expose:
		// - Always include if disposition is attachment
		// - Include any part with a filename (even text/*)
		// - Include inline images/files if they have Content-ID or Content-Location (for HTML inlining)
		// - Otherwise, skip common body parts like text/plain and text/html without attachment indicators
		isText := strings.HasPrefix(strings.ToLower(mediaType), "text/")
		isAttachmentDisposition := strings.Contains(dispLower, "attachment")
		isInlineReference := contentID != "" || contentLocation != ""
		if !(isAttachmentDisposition || filename != "" || isInlineReference) {
			// Skip non-attachment, non-inline-reference parts (likely body text or generic parts)
			if isText {
				continue
			}
			if filename == "" {
				continue
			}
		}

		// Filter out tracking pixels / spacer images for inline images only.
		// Real tracking pixels are typically 1x1 or 2x2 transparent GIFs (~50–
		// 200 bytes). The previous 1 KB threshold was wide enough to also drop
		// legitimate small inline icons (favicons, signature glyphs, small logos
		// — typically 300 bytes to a few KB), which made signature-block images
		// disappear from rendered emails. 256 bytes catches essentially all
		// real tracking pixels while preserving real content.
		if isInlineReference && strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			if len(dataBytes) < 256 {
				p.log.Debug().
					Str("filename", filename).
					Str("content_type", ct).
					Int("size", len(dataBytes)).
					Str("cid", contentID).
					Msg("Filtered out tracking-pixel-sized image (< 256B)")
				continue
			}
		}

		// Default filename if still empty
		if filename == "" {
			filename = "attachment"
		}

		attachment := &EmailAttachment{
			Filename:        filename,
			ContentType:     ct,
			Size:            int64(len(dataBytes)),
			Data:            dataBytes,
			ContentID:       contentID,
			ContentLocation: normalizeContentLocation(contentLocation),
			Disposition:     strings.ToLower(strings.TrimSpace(strings.Split(contentDisposition, ";")[0])),
			IsInline:        isInline || isInlineReference,
		}
		attachments = append(attachments, attachment)
		p.log.Debug().
			Str("filename", filename).
			Str("content_type", ct).
			Int64("size", attachment.Size).
			Bool("inline", attachment.IsInline).
			Str("cid", attachment.ContentID).
			Str("cl", attachment.ContentLocation).
			Msg("Extracted email part")
	}

	return attachments
}

// extractReferencesFromHeaders extracts References header from body sections
func (p *Processor) extractReferencesFromHeaders(buf *imapclient.FetchMessageBuffer) []string {
	// Look for header sections
	for _, section := range buf.BodySection {
		if section.Section.Specifier == imap.PartSpecifierHeader {
			// Parse headers
			headers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(section.Bytes))).ReadMIMEHeader()
			if err != nil {
				continue
			}

			// Extract References header
			if references := headers.Get("References"); references != "" {
				return parseReferences(references)
			}
		}
	}
	return nil
}

// decodeBody wraps the reader according to Content-Transfer-Encoding
func decodeBody(r io.Reader, cte string) io.Reader {
	switch strings.ToLower(cte) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	default:
		return r
	}
}

// detectBoundary tries to find a MIME boundary token in a raw body without headers
func detectBoundary(data []byte) string {
	// Look for a line starting with --token and followed soon by a Content-Type header
	// This is a best-effort heuristic for bodies that are raw multipart payloads
	lines := bytes.Split(data, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		line := bytes.TrimRight(lines[i], "\r")
		if bytes.HasPrefix(line, []byte("--")) && len(line) > 2 {
			token := string(line[2:])
			// Skip closing boundary markers like --token--
			token = strings.TrimSuffix(token, "--")
			// Validate by checking next few lines for Content-Type
			for j := i + 1; j < len(lines) && j < i+10; j++ {
				if bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(lines[j])), []byte("content-type:")) {
					return token
				}
			}
		}
	}
	return ""
}

// isQuotedContent tries to detect if a multipart section contains quoted/forwarded content
// rather than current message content, to avoid extracting attachments from email thread history
func (p *Processor) isQuotedContent(dataBytes []byte) bool {
	// Check various indicators that this is quoted/forwarded content

	// 1. Look for common forwarded message headers within the content
	dataStr := string(dataBytes)
	lowerData := strings.ToLower(dataStr)

	// Common forwarded message markers
	forwardMarkers := []string{
		"-----original message-----",
		"begin forwarded message",
		"forwarded message",
		"---------- forwarded message ----------",
		"from:", // Often appears at start of quoted content
	}

	for _, marker := range forwardMarkers {
		if strings.Contains(lowerData, marker) {
			return true
		}
	}

	// 2. Check for reply indicators with Message-ID patterns
	// These often indicate we're looking at a nested/quoted email
	if strings.Contains(lowerData, "message-id:") &&
		(strings.Contains(lowerData, "date:") || strings.Contains(lowerData, "subject:")) {
		return true
	}

	// 3. Check content length - very large multipart sections in replies
	// are often the entire quoted thread history
	if len(dataBytes) > 100*1024 { // 100KB threshold
		// Large content with multiple boundaries is likely quoted thread
		boundaryCount := strings.Count(lowerData, "boundary=")
		if boundaryCount > 2 {
			return true
		}
	}

	return false
}

// formatIMAPAddress converts an IMAP address to string format
func formatIMAPAddress(addr *imap.Address) string {
	if addr == nil {
		return ""
	}

	if addr.Name != "" {
		// Must go through net/mail so a display name containing a comma --
		// "Doe, John", the Exchange/Outlook default -- is quoted. Unquoted, it
		// fails net/mail.ParseAddress on the send path, where the recipient is
		// then dropped from reply-all silently.
		a := netmail.Address{Name: addr.Name, Address: fmt.Sprintf("%s@%s", addr.Mailbox, addr.Host)}
		return a.String()
	}
	return fmt.Sprintf("%s@%s", addr.Mailbox, addr.Host)
}

// formatIMAPAddressSlice converts IMAP v2 address slices to string slice
func formatIMAPAddressSlice(addrs []imap.Address) []string {
	if len(addrs) == 0 {
		return nil
	}

	// Pre-allocate with exact size to avoid reallocations
	result := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		result = append(result, formatIMAPAddress(&addr))
	}
	return result
}

// isOutboundMessage determines if this email was sent by the bridge user.
// Currently, we only process the INBOX, so all processed emails are treated as inbound.
// When Sent-folder processing is added, this can be revisited with mailbox context.
func (p *Processor) isOutboundMessage(mailbox string) bool {
	mb := strings.ToLower(strings.TrimSpace(mailbox))
	// Treat messages from any “Sent” mailbox variant as outbound.
	if mb == "" {
		return false
	}
	// Common patterns: "sent", "sent items", "sent messages", "[gmail]/sent mail"
	if strings.Contains(mb, "sent") {
		return true
	}
	return false
}

// ToMatrixEvent converts an EmailMessage to a bridgev2 RemoteMessage event
func (p *Processor) ToMatrixEvent(ctx context.Context, emailMsg *EmailMessage, userLogin *bridgev2.UserLogin) bridgev2.RemoteMessage {
	return &EmailMatrixEvent{
		emailMessage: emailMsg,
		userLogin:    userLogin,
		processor:    p,
	}
}

// EmailMatrixEvent and helper functions

// InlineImageMeta holds metadata for an inline image we plan to post as a sidecar m.image
// Index preserves document order for nice numbering.
type InlineImageMeta struct {
	Index int
	Label string
	MXC   id.ContentURIString
	Mime  string
	Size  int
}

// EmailMatrixEvent implements bridgev2.RemoteMessage for email messages
type EmailMatrixEvent struct {
	emailMessage *EmailMessage
	userLogin    *bridgev2.UserLogin
	processor    *Processor
}

// Implement bridgev2.RemoteMessage interface
func (e *EmailMatrixEvent) GetID() networkid.MessageID {
	return e.emailMessage.MessageID
}

func (e *EmailMatrixEvent) GetTimestamp() time.Time {
	return e.emailMessage.Timestamp
}

func (e *EmailMatrixEvent) GetSender() bridgev2.EventSender {
	// Create ghost sender for ALL messages (both inbound and outbound)
	fromEmail := extractEmailAddress(e.emailMessage.From)
	if strings.TrimSpace(fromEmail) == "" {
		// Fallback to a deterministic placeholder rather than letting the bridge default to bot
		fromEmail = "unknown"
		if e.processor != nil {
			if e.processor.sanitized {
				e.processor.log.Debug().Msg("Sender email missing/unparsable; using fallback ghost email:unknown")
			} else {
				e.processor.log.Debug().Msg("Sender email missing/unparsable; using fallback ghost email:unknown")
			}
		}
	}
	ghostID := common.EmailToGhostID(fromEmail)

	if e.emailMessage.IsOutbound {
		// For outbound messages: set both IsFromMe=true (for Matrix attribution)
		// AND Sender=ghostID (for database storage and thread resolution)
		return bridgev2.EventSender{Sender: ghostID, IsFromMe: true}
	}

	// For inbound messages: only ghost sender
	return bridgev2.EventSender{Sender: ghostID}
}

func (e *EmailMatrixEvent) GetPortalKey() networkid.PortalKey {
	return e.emailMessage.PortalKey
}

func (e *EmailMatrixEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventMessage
}

func (e *EmailMatrixEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.
		Str("email_message_id", string(e.emailMessage.MessageID)).
		Str("email_subject", e.emailMessage.Subject).
		Str("email_from", e.emailMessage.From)
}

func (e *EmailMatrixEvent) ConvertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI) (cm *bridgev2.ConvertedMessage, err error) {
	var parts []*bridgev2.ConvertedMessagePart
	// Helper to append a part with deterministic ID for idempotency
	appendPart := func(id string, content *event.MessageEventContent) {
		parts = append(parts, &bridgev2.ConvertedMessagePart{ID: networkid.PartID(id), Type: event.EventMessage, Content: content})
	}
	// Global safety net: never drop the whole message on panic. Emit a placeholder notice instead.
	defer func() {
		if r := recover(); r != nil {
			e.processor.log.Error().Any("panic", r).Msg("Email conversion panicked — sending placeholder notice")
			n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "⚠️ Your email could not be fully processed by the bridge. The original message may have been dropped. Please report this to the bridge maintainers."}
			appendPart("error-placeholder", n)
			cm = &bridgev2.ConvertedMessage{Parts: parts}
			// Clear err to prevent confusing dual state - placeholder message is the recovery strategy
			err = nil
		}
	}()

	// Preprocess inline images for HTML
	// Pre-size maps based on typical attachment counts to reduce allocations
	attachmentCount := len(e.emailMessage.Attachments)
	usedInline := make(map[int]bool, attachmentCount)
	// For CSS url(cid:...) rewriting later
	cidToMXC := make(map[string]string, attachmentCount)
	locToMXC := make(map[string]string, attachmentCount)
	// Inline images we will send as sidecar m.image events, in document order
	// Pre-allocate with reasonable capacity to reduce reallocations
	inlineImages := make([]*InlineImageMeta, 0, attachmentCount/2)
	nextIndex := 1

	origHTML := e.emailMessage.HTMLContent
	e.processor.log.Debug().
		Int("attachments", len(e.emailMessage.Attachments)).
		Int("text_len", len(e.emailMessage.TextContent)).
		Int("html_len", len(origHTML)).
		Msg("Converting email to Matrix event")
	if origHTML != "" {
		// Early lightweight minification to save space without harming formatting
		if len(origHTML) > 30*1024 {
			origHTML = lightMinifyHTML(origHTML)
		}
		// Externalize data: URLs to MXC and rewrite references. Also get metas for those images.
		dataURIsReplaced, replacedCount, failedCount, dataMetas := e.externalizeDataURIs(ctx, intent, origHTML)
		if replacedCount > 0 || failedCount > 0 {
			if replacedCount > 0 {
				n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: fmt.Sprintf("Optimized inline resources: moved %d embedded data URLs to media.", replacedCount)}
				appendPart("html-inline-optimized", n)
			}
			if failedCount > 0 {
				n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: fmt.Sprintf("Some embedded data URLs were too large or invalid and were omitted (%d).", failedCount)}
				appendPart("html-inline-omitted", n)
			}
		}
		origHTML = dataURIsReplaced

		// Build quick lookups for attachments by CID and Content-Location
		// We'll process <img> tags in document order, upload needed parts, and replace with placeholders.
		// Prepare a regex to find <img ...> tags.
		reImgTag := regexp.MustCompile(`(?is)<\s*img\b[^>]*>`)
		// Attribute extractors
		extractAttr := func(tag, name string) string {
			re := regexp.MustCompile(`(?i)` + name + `\s*=\s*([\'\"][^\'\"]*[\'\"]|[^\s>]+)`)
			m := re.FindStringSubmatch(tag)
			if len(m) < 2 {
				return ""
			}
			val := strings.TrimSpace(m[1])
			if strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"") {
				val = strings.TrimSuffix(strings.TrimPrefix(val, "\""), "\"")
			}
			if strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'") {
				val = strings.TrimSuffix(strings.TrimPrefix(val, "'"), "'")
			}
			return val
		}

		// Process tags in order
		occurrence := 0
		origHTML = reImgTag.ReplaceAllStringFunc(origHTML, func(tag string) string {
			occurrence++
			src := strings.TrimSpace(extractAttr(tag, "src"))
			alt := strings.TrimSpace(extractAttr(tag, "alt"))
			if alt == "" {
				alt = strings.TrimSpace(extractAttr(tag, "title"))
			}
			low := strings.ToLower(src)
			// Helper: record the inline image meta (used by the plain-text
			// fallback and by the sidecar emission below) and emit an <img>
			// tag pointing at the uploaded mxc URI inline. Element renders
			// mxc img tags in formatted_body natively, so this gives proper
			// inline image display rather than a "[Image N: label]" text stub.
			add := func(mxc id.ContentURIString, mime string, sz int, defaultLabel string) string {
				label := defaultLabel
				if alt != "" {
					label = alt
				}
				meta := &InlineImageMeta{Index: nextIndex, Label: label, MXC: mxc, Mime: mime, Size: sz}
				inlineImages = append(inlineImages, meta)
				nextIndex++
				// HTML-escape the alt text so a hostile filename can't break out of the attribute.
				safeAlt := html.EscapeString(label)
				return fmt.Sprintf(`<img src="%s" alt="%s">`, string(mxc), safeAlt)
			}
			// Remote images: never fetch, remove placeholders entirely to reduce clutter
			if strings.HasPrefix(low, "http:") || strings.HasPrefix(low, "https:") {
				// Remove remote images entirely - they're usually tracking/marketing content
				return ""
			}
			// Data URIs should have been externalized to mxc already. If src is mxc, try to match a data meta.
			if strings.HasPrefix(low, "mxc://") {
				for _, dm := range dataMetas {
					if strings.EqualFold(string(dm.MXC), src) {
						return add(dm.MXC, dm.Mime, dm.Size, dm.Label)
					}
				}
				// Unknown mxc: show a generic placeholder without sidecar
				return "[Image]"
			}
			// CID-referenced inline
			if strings.HasPrefix(low, "cid:") {
				cid := normalizeCIDRef(src)
				idx := findAttachmentByCID(e.emailMessage.Attachments, cid)
				if idx >= 0 {
					att := e.emailMessage.Attachments[idx]
					if e.processor.MaxUploadBytes > 0 && att.Size > int64(e.processor.MaxUploadBytes) {
						return "[Image omitted: too large]"
					}
					mxc, _, err := intent.UploadMedia(ctx, "", att.Data, bestFilename(att, cid), att.ContentType)
					if err == nil {
						cidToMXC[cid] = string(mxc)
						usedInline[idx] = true
						return add(mxc, att.ContentType, int(att.Size), bestFilename(att, cid))
					}
				}
				return "[Image]"
			}
			// Content-Location relative reference (non-http, non-cid, non-data)
			if low != "" && !strings.HasPrefix(low, "data:") && !strings.HasPrefix(low, "http:") && !strings.HasPrefix(low, "https:") {
				key := normalizeContentLocation(src)
				idx := findAttachmentByContentLocation(e.emailMessage.Attachments, key)
				if idx >= 0 {
					att := e.emailMessage.Attachments[idx]
					if e.processor.MaxUploadBytes > 0 && att.Size > int64(e.processor.MaxUploadBytes) {
						return "[Image omitted: too large]"
					}
					mxc, _, err := intent.UploadMedia(ctx, "", att.Data, bestFilename(att, key), att.ContentType)
					if err == nil {
						locToMXC[key] = string(mxc)
						usedInline[idx] = true
						return add(mxc, att.ContentType, int(att.Size), bestFilename(att, key))
					}
				}
				return "[Image]"
			}
			// Fallback
			return "[Image]"
		})

		// After removing <img> tags, still rewrite CSS backgrounds referencing cid:
		rewritten := rewriteHTMLInline(origHTML, cidToMXC, locToMXC)

		// Materialize CSS / Outlook background images as inline <img> tags
		// so they survive Matrix's HTML sanitizer (which strips `style` and
		// the legacy `background` attribute). Uploads any cid that was only
		// referenced via a background and isn't already in cidToMXC.
		uploadBG := func(cid string) (string, string) {
			idx := findAttachmentByCID(e.emailMessage.Attachments, cid)
			if idx < 0 {
				return "", ""
			}
			att := e.emailMessage.Attachments[idx]
			if e.processor.MaxUploadBytes > 0 && att.Size > int64(e.processor.MaxUploadBytes) {
				return "", ""
			}
			mxc, _, err := intent.UploadMedia(ctx, "", att.Data, bestFilename(att, cid), att.ContentType)
			if err != nil {
				e.processor.log.Warn().Err(err).Str("cid", cid).Msg("Failed to upload cid background image")
				return "", ""
			}
			usedInline[idx] = true
			// Record so the inline-images sidecar path can see it; the meta
			// label is generic since the only reference was a background.
			inlineImages = append(inlineImages, &InlineImageMeta{
				Index: nextIndex,
				Label: bestFilename(att, cid),
				MXC:   mxc,
				Mime:  att.ContentType,
				Size:  int(att.Size),
			})
			nextIndex++
			return string(mxc), att.ContentType
		}
		rewritten = MaterializeBackgroundImages(rewritten, cidToMXC, uploadBG)

		e.processor.log.Debug().
			Int("inline_images", len(inlineImages)).
			Int("new_html_len", len(rewritten)).
			Msg("Processed inline <img> tags and rewrote CSS backgrounds")
		origHTML = rewritten
	}

	// Create the main text message content. For replies (parent message
	// referenced via In-Reply-To or References), strip the quoted history so
	// Matrix readers see only the new content the sender wrote — the quote
	// chain is preserved in the email itself for downstream mail clients.
	isReply := e.emailMessage.InReplyTo != "" || len(e.emailMessage.References) > 0
	bodyText := e.emailMessage.TextContent
	if isReply {
		if stripped := StripQuotedReply(bodyText); stripped != "" {
			bodyText = stripped
		}
	}
	// Matrix renders FormattedBody (HTML) when present, so the same strip has
	// to happen on the HTML half — otherwise the gmail_quote / blockquote
	// chain shows up in the room even though we cleaned up the plain text.
	if isReply && origHTML != "" {
		if stripped := StripQuotedReplyHTML(origHTML); stripped != "" {
			origHTML = stripped
		}
	}
	// Avoid duplicating large content when HTML is present: summarize body
	if origHTML != "" && len(bodyText) > 2048 {
		short, _ := truncateUTF8PreserveWords(bodyText, 1000)
		bodyText = short + "\n\n[HTML version included below]"
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    bodyText,
	}
	// If we collected inline images, append a short listing to the text body for clients ignoring HTML.
	if len(inlineImages) > 0 {
		var b strings.Builder
		b.WriteString(content.Body)
		if content.Body != "" {
			b.WriteString("\n\n")
		}
		b.WriteString("Images:\n")
		for _, im := range inlineImages {
			b.WriteString(fmt.Sprintf(" - Image %d: %s\n", im.Index, im.Label))
		}
		content.Body = strings.TrimRight(b.String(), "\n")
	}

	// Add HTML formatting if available
	if origHTML != "" && origHTML != e.emailMessage.TextContent {
		content.Format = event.FormatHTML
		// Decode HTML entities in the formatted body before sending to Matrix
		content.FormattedBody = html.UnescapeString(origHTML)
		// Filter out invisible Unicode characters from HTML content too
		content.FormattedBody = filterInvisibleUnicode(content.FormattedBody)
	}

	// Ensure body isn't empty - if we have HTML but no text, try to extract from HTML
	if content.Body == "" && origHTML != "" {
		content.Body = simpleHTMLToText(origHTML)
		// If HTML extraction still produces nothing meaningful, use fallback
		if strings.TrimSpace(content.Body) == "" {
			content.Body = "[Email content is HTML-only - check formatted version]"
		}
	} else if content.Body == "" {
		content.Body = "[No text content]"
	}

	// htmlAttachedAsFile tracks whether the original HTML ends up uploaded as
	// a sidecar file (because it was either too large to ship as
	// formatted_body or detected as marketing-heavy and intentionally
	// dropped). When true, inline images are already visible in that
	// attachment — emitting m.image sidecars too would produce duplicate
	// (and often broken) image events.
	htmlAttachedAsFile := false

	// Marketing-heavy emails (Miro/Mailchimp/etc. style: nested tables,
	// CSS background images, little real text) render in Matrix as walls
	// of empty bordered boxes because the sanitizer strips the inline
	// styles that hold the layout together. Drop the HTML alternative
	// entirely and force the attach-as-file branch below — the user gets
	// the readable plain text in Matrix and the full HTML as a clickable
	// attachment.
	forceHTMLAttach := origHTML != "" && content.FormattedBody != "" && IsLikelyMarketingHTML(origHTML, bodyText)

	// Step 1: If we have HTML, try to keep it by minifying when necessary
	if (forceHTMLAttach || !withinMatrixLimit(content, MaxMatrixContentSize)) && content.FormattedBody != "" {
		// Marketing-heavy emails: skip minification (it's not a size problem)
		// and go straight to attaching the original HTML as a file.
		if !forceHTMLAttach {
			// Try bounded minification (more conservative target to account for encryption overhead)
			if minified, ok := boundedMinifyHTML(content.FormattedBody, HTMLMinificationTarget); ok {
				content.FormattedBody = minified
			}
		}
		// Drop HTML and ship the original as a file when forced (marketing)
		// or when we couldn't get under the size limit even after minify.
		if forceHTMLAttach || !withinMatrixLimit(content, MaxMatrixContentSize) {
			content.FormattedBody = ""
			// Add a small notice in the body — wording matches the reason
			// we dropped the HTML so users know what to expect.
			notice := "[Full HTML too large to send inline — attached below]"
			if forceHTMLAttach {
				notice = "[Marketing-style HTML kept as attachment — open it to see the full design]"
			}
			if content.Body != "" {
				content.Body += "\n\n" + notice
			} else {
				content.Body = notice
			}
			htmlBytes := []byte(origHTML)
			// Prepare a user-facing notice about data handling
			var noticeText string
			filename := "original-email.html"
			mimeType := "text/html"
			// Enforce upload size limit with gzip fallback for bodies
			if e.processor.MaxUploadBytes > 0 && len(htmlBytes) > e.processor.MaxUploadBytes && e.processor.GzipLargeBodies {
				if gz, ok := gzipBytes(htmlBytes); ok && len(gz) <= e.processor.MaxUploadBytes {
					htmlBytes = gz
					filename = "original-email.html.gz"
					mimeType = "application/gzip"
					noticeText = "HTML body exceeded upload limit — compressed and attached as .gz for review."
				}
			}
			if e.processor.MaxUploadBytes > 0 && len(htmlBytes) > e.processor.MaxUploadBytes {
				// Still too big — send a clear notice and skip
				n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "HTML body too large to attach; content was omitted."}
				appendPart("html-oversize-omitted", n)
			} else {
				mxc, _, err := intent.UploadMedia(ctx, "", htmlBytes, filename, mimeType)
				if err != nil {
					e.processor.log.Warn().Err(err).Msg("Failed to upload full HTML, sending notice instead")
					n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Failed to upload full HTML content."}
					parts = append(parts, &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: n})
				} else {
					if noticeText != "" {
						n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: noticeText}
						appendPart("html-inline-notice", n)
					} else {
						n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Full HTML was too large to send inline — attached for review."}
						parts = append(parts, &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: n})
					}
					att := &event.MessageEventContent{MsgType: event.MsgFile, Body: filename, URL: mxc}
					att.Info = &event.FileInfo{MimeType: mimeType, Size: len(htmlBytes)}
					appendPart("html-attachment", att)
					htmlAttachedAsFile = true
				}
			}
		}
	}

	// Step 2: If still too large (plain text is huge), truncate body and attach full text.
	if !withinMatrixLimit(content, MaxMatrixContentSize) {
		fullText := content.Body
		// Aim to leave headroom for wrapper keys etc.
		maxBody := MaxMatrixContentSize - 2048
		if maxBody < 1024 {
			maxBody = 1024
		}
		trunc, did := truncateUTF8PreserveWords(fullText, maxBody)
		if did {
			content.Body = trunc + "\n\n[Message truncated — full text attached]"
		} else {
			// As a last resort, cut raw bytes safely
			if len(fullText) > maxBody {
				content.Body = fullText[:maxBody] + "\n\n[Message truncated]"
			}
		}
		// Attach full text (with gzip fallback if oversized)
		textBytes := []byte(fullText)
		filename := "original-email.txt"
		mimeType := "text/plain"
		noticeText := "Full text was too large to send inline — attached for review."
		if e.processor.MaxUploadBytes > 0 && len(textBytes) > e.processor.MaxUploadBytes && e.processor.GzipLargeBodies {
			if gz, ok := gzipBytes(textBytes); ok && len(gz) <= e.processor.MaxUploadBytes {
				textBytes = gz
				filename = "original-email.txt.gz"
				mimeType = "application/gzip"
				noticeText = "Text body exceeded upload limit — compressed and attached as .gz for review."
			}
		}
		if e.processor.MaxUploadBytes > 0 && len(textBytes) > e.processor.MaxUploadBytes {
			// Still too big — clear notice and skip attaching
			n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Text body too large to attach; only truncated body was sent."}
			appendPart("text-oversize-omitted", n)
		} else {
			mxc, _, err := intent.UploadMedia(ctx, "", textBytes, filename, mimeType)
			if err != nil {
				e.processor.log.Warn().Err(err).Msg("Failed to upload full text, proceeding with truncated body only")
			} else {
				n := &event.MessageEventContent{MsgType: event.MsgNotice, Body: noticeText}
				appendPart("text-attachment-notice", n)
				att := &event.MessageEventContent{MsgType: event.MsgFile, Body: filename, URL: mxc}
				att.Info = &event.FileInfo{MimeType: mimeType, Size: len(textBytes)}
				appendPart("text-attachment", att)
			}
		}
	}

	// After adjustments, if somehow still too big, drop formatted_body to be extra safe
	if content.FormattedBody != "" && !withinMatrixLimit(content, MaxMatrixContentSize) {
		content.FormattedBody = ""
	}

	// Add participant change notification if any
	if participantChangeMsg := generateParticipantChangeMessage(e.emailMessage.Thread); participantChangeMsg != "" {
		changeContent := &event.MessageEventContent{MsgType: event.MsgNotice, Body: participantChangeMsg}
		appendPart("participant-change", changeContent)
		// Clear the changes after processing
		e.emailMessage.Thread.ClearParticipantChanges()
	}

	// Add the main message part(s), chunking if necessary to stay well below server limits.
	if withinMatrixLimit(content, MaxMatrixContentSize) && len(content.Body) <= PerEventTarget {
		appendPart("body", content)
	} else {
		// Split the body into multiple UTF-8 safe chunks.
		remaining := content.Body
		chunkIndex := 1
		const maxChunks = 50 // Prevent excessive chunking that could overwhelm Matrix
		for len(remaining) > 0 && chunkIndex <= maxChunks {
			pid := fmt.Sprintf("body-chunk-%d", chunkIndex)
			// Try to cut a chunk that fits comfortably under the target
			chunk, _ := truncateUTF8PreserveWords(remaining, PerEventTarget)
			if chunk == "" {
				// Fallback to raw slice to make progress
				cut := PerEventTarget
				if cut > len(remaining) {
					cut = len(remaining)
				}
				chunk = remaining[:cut]
			}
			chunkContent := &event.MessageEventContent{MsgType: event.MsgText, Body: chunk}
			// Make extra sure this chunk fits JSON limit
			for !withinMatrixLimit(chunkContent, MaxMatrixContentSize) && len(chunk) > 0 {
				// Reduce chunk size by 10%
				reduceBy := len(chunk) / 10
				if reduceBy < 256 {
					reduceBy = 256
				}
				newLen := len(chunk) - reduceBy
				if newLen <= 0 {
					newLen = len(chunk) - 1
				}
				chunk = chunk[:newLen]
				chunkContent.Body = chunk
			}
			parts = append(parts, &bridgev2.ConvertedMessagePart{ID: networkid.PartID(pid), Type: event.EventMessage, Content: chunkContent})
			// Advance remaining
			if len(chunk) >= len(remaining) {
				remaining = ""
			} else {
				remaining = remaining[len(chunk):]
				// Trim leading whitespace in the next chunk to avoid odd spacing
				remaining = strings.TrimLeft(remaining, " \n\t\r")
			}
			chunkIndex++
		}
		// If we hit the chunk limit, warn about truncated content
		if len(remaining) > 0 && chunkIndex > maxChunks {
			truncateContent := &event.MessageEventContent{MsgType: event.MsgNotice, Body: fmt.Sprintf("Message was too large and has been truncated. %d characters omitted to prevent server overload.", len(remaining))}
			appendPart("truncate-notice", truncateContent)
		}
	}

	// Emit sidecar image messages for inline images in document order, BUT
	// only when the HTML formatted_body was dropped (size constraints) or
	// never existed — otherwise the inline <img src="mxc://..."> tags inside
	// formatted_body already render the image in Element, and emitting a
	// sidecar m.image event on top creates a duplicate "image card" below
	// the body. Keeping the sidecar as a fallback means HTML-stripped events
	// and plain-text-only Matrix clients still see the image.
	// Emit sidecars only when the formatted_body is gone AND the HTML wasn't
	// attached as a file (which already contains the inline images). Even
	// then, skip "decorative" images — tracking pixels, signature logos, and
	// other small assets that produce broken-image cards more often than they
	// add value. Real photos and screenshots tend to be well above the
	// decorative threshold.
	emitSidecars := content.FormattedBody == "" && !htmlAttachedAsFile
	if emitSidecars {
		for _, im := range inlineImages {
			if isLikelyDecorativeImage(im) {
				continue
			}
			pid := fmt.Sprintf("inline-image-%d", im.Index)
			imgContent := &event.MessageEventContent{
				MsgType: event.MsgImage,
				Body:    fmt.Sprintf("Image %d: %s", im.Index, im.Label),
				URL:     im.MXC,
			}
			imgContent.Info = &event.FileInfo{MimeType: im.Mime, Size: im.Size}
			parts = append(parts, &bridgev2.ConvertedMessagePart{ID: networkid.PartID(pid), Type: event.EventMessage, Content: imgContent})
		}
	}

	// Process attachments and upload them to Matrix (skip those used inline)
	for idx, attachment := range e.emailMessage.Attachments {
		if usedInline[idx] {
			continue
		}
		if attachmentPart, err := e.convertAttachmentToMatrix(ctx, attachment, intent); err == nil {
			if attachmentPart != nil {
				attachmentPart.ID = networkid.PartID(fmt.Sprintf("att-%d-%s", idx+1, sanitizeFilename(attachment.Filename)))
				parts = append(parts, attachmentPart)
			}
		} else {
			e.processor.log.Warn().Err(err).Str("filename", attachment.Filename).Msg("Failed to upload attachment to Matrix")
			fallbackContent := &event.MessageEventContent{MsgType: event.MsgNotice, Body: fmt.Sprintf("📎 Attachment failed to upload: %s (%s)", attachment.Filename, attachment.ContentType)}
			parts = append(parts, &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: fallbackContent})
		}
	}

	return &bridgev2.ConvertedMessage{Parts: parts}, nil
}

// boundedMinifyHTML performs a very simple minification and bounds output size without breaking tags badly.
// Returns (result, ok). If ok=false, caller should assume no useful minification happened.
func boundedMinifyHTML(html string, maxBytes int) (string, bool) {
	// Cheap removals: comments, script/style blocks, excessive whitespace.
	// Note: This is intentionally simple to avoid heavy dependencies.
	// 1) Remove <!-- comments -->
	html = removeHTMLComments(html)
	// 2) Remove <script>...</script> and <style>...</style>
	html = stripTagContent(html, "script")
	html = stripTagContent(html, "style")
	// 3) Collapse runs of whitespace
	html = collapseWhitespace(html)
	if len(html) <= maxBytes {
		return html, true
	}
	// Truncate at a safe boundary: try to cut at last closing tag before maxBytes
	if maxBytes < len(html) {
		cut := maxBytes
		// backtrack to a tag boundary to avoid cutting in the middle of a tag
		for cut > 0 {
			c := html[cut-1]
			if c == '>' || c == '\n' || c == ' ' {
				break
			}
			cut--
		}
		if cut < 1 {
			cut = maxBytes
		}
		res := html[:cut] + "\n<!-- truncated -->"
		return res, true
	}
	return html, false
}

func removeHTMLComments(s string) string {
	// Remove <!-- ... --> blocks (non-greedy). This is simplistic and won't handle edge cases with "--" in text.
	for {
		start := strings.Index(s, "<!--")
		if start == -1 {
			break
		}
		end := strings.Index(s[start+4:], "-->")
		if end == -1 {
			break
		}
		end += start + 4
		s = s[:start] + s[end+3:]
	}
	return s
}

func stripTagContent(s, tag string) string {
	open := "<" + tag
	close := "</" + tag + ">"
	for {
		start := strings.Index(strings.ToLower(s), open)
		if start == -1 {
			break
		}
		end := strings.Index(strings.ToLower(s[start:]), close)
		if end == -1 { // no close, remove from start to end
			s = s[:start]
			break
		}
		end = start + end + len(close)
		s = s[:start] + s[end:]
	}
	return s
}

func collapseWhitespace(s string) string {
	// Replace runs of spaces/tabs/newlines with a single space/newline where appropriate.
	// For simplicity, collapse consecutive whitespace to a single space.
	var b strings.Builder
	b.Grow(len(s))
	prevWS := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' { // whitespace
			if !prevWS {
				b.WriteRune(' ')
				prevWS = true
			}
			continue
		}
		prevWS = false
		b.WriteRune(r)
	}
	return b.String()
}

// gzipBytes compresses the input using gzip with default compression. Returns (gzipped, ok).
func gzipBytes(data []byte) ([]byte, bool) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, false
	}
	if err := zw.Close(); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// withinMatrixLimit marshals the content to JSON to estimate the actual event content size
func withinMatrixLimit(content *event.MessageEventContent, limit int) bool {
	// Only include fields that are part of the event content
	type minimal struct {
		MsgType       event.MessageType `json:"msgtype,omitempty"`
		Body          string            `json:"body,omitempty"`
		Format        string            `json:"format,omitempty"`
		FormattedBody string            `json:"formatted_body,omitempty"`
		URL           string            `json:"url,omitempty"`
		Info          *event.FileInfo   `json:"info,omitempty"`
	}
	m := minimal{
		MsgType:       content.MsgType,
		Body:          content.Body,
		Format:        string(content.Format),
		FormattedBody: content.FormattedBody,
		URL:           string(content.URL),
		Info:          content.Info,
	}
	b, err := json.Marshal(m)
	if err != nil {
		// Fallback: approximate using lengths
		sz := len(content.Body) + len(content.FormattedBody)
		if content.URL != "" {
			sz += len(content.URL)
		}
		if content.Info != nil {
			sz += 128 // rough overhead
		}
		return sz <= limit
	}
	return len(b) <= limit
}

// truncateUTF8PreserveWords trims a string to maxBytes without splitting UTF-8 runes and tries to cut at a space.
// Returns (result, truncated)
func truncateUTF8PreserveWords(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	// Ensure we don't cut in the middle of a rune
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	if cut <= 0 {
		cut = maxBytes
	}
	// Try to cut at last space before cut
	lastSpace := strings.LastIndexByte(s[:cut], ' ')
	if lastSpace > 0 && cut-lastSpace < 512 { // don't backtrack too far
		cut = lastSpace
	}
	return s[:cut], true
}

// convertAttachmentToMatrix uploads an email attachment to Matrix and returns a ConvertedMessagePart
func (e *EmailMatrixEvent) convertAttachmentToMatrix(ctx context.Context, attachment *EmailAttachment, intent bridgev2.MatrixAPI) (*bridgev2.ConvertedMessagePart, error) {
	e.processor.log.Debug().
		Str("filename", attachment.Filename).
		Str("content_type", attachment.ContentType).
		Int64("size", attachment.Size).
		Msg("Uploading email attachment to Matrix")

	// Enforce upload size limit for general attachments
	if e.processor.MaxUploadBytes > 0 && attachment.Size > int64(e.processor.MaxUploadBytes) {
		return nil, fmt.Errorf("attachment exceeds upload limit (%d bytes > %d bytes)", attachment.Size, e.processor.MaxUploadBytes)
	}

	// Sanitize filename for upload
	safeName := sanitizeFilename(attachment.Filename)
	if safeName == "" {
		safeName = bestFilename(attachment, "attachment")
		safeName = sanitizeFilename(safeName)
	}
	// Upload the attachment data to Matrix media repository
	uploadResp, _, err := intent.UploadMedia(ctx, "", attachment.Data, safeName, attachment.ContentType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload attachment to Matrix: %w", err)
	}

	// Determine the message type based on content type
	msgType := e.getMessageTypeForAttachment(attachment.ContentType)

	// Create the Matrix message content for the attachment
	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    attachment.Filename,
		URL:     uploadResp,
	}

	// Add basic file info - Matrix will handle the appropriate type
	content.Info = &event.FileInfo{
		MimeType: attachment.ContentType,
		Size:     int(attachment.Size),
	}

	// For images and videos, we could add more specific info in the future
	// but for now, basic FileInfo works for all types

	e.processor.log.Info().
		Str("filename", attachment.Filename).
		Str("matrix_url", string(content.URL)).
		Str("msg_type", string(msgType)).
		Msg("Successfully uploaded attachment to Matrix")

	return &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: content,
	}, nil
}

// getMessageTypeForAttachment determines the appropriate Matrix message type for an attachment
func (e *EmailMatrixEvent) getMessageTypeForAttachment(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// Helper functions for email processing

// normalizeCIDHeader cleans a Content-ID header value by trimming <> and lowercasing
func normalizeCIDHeader(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.ToLower(s)
}

func normalizeCIDRef(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "cid:")
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return s
}

func normalizeContentLocation(s string) string {
	s = strings.TrimSpace(s)
	return s
}

func findAttachmentByCID(atts []*EmailAttachment, cid string) int {
	for i, a := range atts {
		if a != nil && a.ContentID != "" {
			if normalizeCIDRef(a.ContentID) == normalizeCIDRef(cid) {
				return i
			}
		}
	}
	return -1
}

func findAttachmentByContentLocation(atts []*EmailAttachment, loc string) int {
	for i, a := range atts {
		if a != nil && a.ContentLocation != "" && strings.EqualFold(a.ContentLocation, loc) {
			return i
		}
	}
	return -1
}

func bestFilename(att *EmailAttachment, fallback string) string {
	if att.Filename != "" {
		return att.Filename
	}
	if fallback != "" {
		return fallback
	}
	return "inline"
}

// sanitizeFilename removes path separators, trims control chars, and bounds length.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	// Replace path separators with underscore
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	// Remove control characters
	builder := strings.Builder{}
	for _, r := range name {
		if r < 32 || r == 127 { // control chars
			continue
		}
		builder.WriteRune(r)
	}
	name = builder.String()
	if name == "" {
		return name
	}
	// Bound length to a reasonable size
	const maxLen = 128
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}

// externalizeDataURIs finds data: URLs in HTML, uploads them to media, and rewrites to mxc URLs.
// Returns (rewrittenHTML, replaced, failed)
func (e *EmailMatrixEvent) externalizeDataURIs(ctx context.Context, intent bridgev2.MatrixAPI, html string) (string, int, int, []*InlineImageMeta) {
	out := html
	var metas []*InlineImageMeta
	reDataImg, err := regexp.Compile(`(?i)(src\s*=\s*)(['"])\s*data:([a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+);base64,([a-z0-9+/=]+)\s*['\"]`)
	if err != nil {
		// If the regex fails to compile for any reason, don't panic; just skip externalization.
		return out, 0, 0, metas
	}
	reDataCSS, err := regexp.Compile(`(?i)url\(\s*data:([a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+);base64,([a-z0-9+/=]+)\s*\)`)
	if err != nil {
		return out, 0, 0, metas
	}
	replaced := 0
	failed := 0
	// Replace <img src="data:...">
	out = reDataImg.ReplaceAllStringFunc(out, func(m string) string {
		subs := reDataImg.FindStringSubmatch(m)
		if len(subs) < 5 {
			return m
		}
		attr := subs[1]
		quote := subs[2]
		mimeType := strings.ToLower(subs[3])
		b64 := subs[4]
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			failed++
			return m
		}
		if e.processor.MaxUploadBytes > 0 && len(data) > e.processor.MaxUploadBytes {
			failed++
			return m
		}
		name := "inline"
		if strings.HasPrefix(mimeType, "image/") {
			name = "inline." + strings.TrimPrefix(mimeType, "image/")
		}
		name = sanitizeFilename(name)
		mxc, _, err := intent.UploadMedia(ctx, "", data, name, mimeType)
		if err != nil {
			failed++
			return m
		}
		replaced++
		metas = append(metas, &InlineImageMeta{Label: name, MXC: mxc, Mime: mimeType, Size: len(data)})
		return attr + quote + string(mxc) + quote
	})
	// Replace CSS url(data:...)
	out = reDataCSS.ReplaceAllStringFunc(out, func(m string) string {
		subs := reDataCSS.FindStringSubmatch(m)
		if len(subs) < 3 {
			return m
		}
		mimeType := strings.ToLower(subs[1])
		b64 := subs[2]
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			failed++
			return m
		}
		if e.processor.MaxUploadBytes > 0 && len(data) > e.processor.MaxUploadBytes {
			failed++
			return m
		}
		name := "inline"
		name = sanitizeFilename(name)
		mxc, _, err := intent.UploadMedia(ctx, "", data, name, mimeType)
		if err != nil {
			failed++
			return m
		}
		replaced++
		metas = append(metas, &InlineImageMeta{Label: name, MXC: mxc, Mime: mimeType, Size: len(data)})
		return "url(" + string(mxc) + ")"
	})
	return out, replaced, failed, metas
}

// rewriteHTMLInline replaces cid: and content-location references with mxc urls
func rewriteHTMLInline(html string, cidToMXC map[string]string, locToMXC map[string]string) string {
	out := html
	// Replace cid: in img src and preserve the original quoting using pre-compiled regex
	out = reImgCidSrc.ReplaceAllStringFunc(out, func(m string) string {
		subs := reImgCidSrc.FindStringSubmatch(m)
		if len(subs) > 3 {
			attr := subs[1]  // src=
			quote := subs[2] // ' or "
			cidRef := subs[3]
			cid := normalizeCIDRef(cidRef)
			if mxc, ok := cidToMXC[cid]; ok && mxc != "" {
				return attr + quote + mxc + quote
			}
		}
		return m
	})

	// Replace CSS url(cid:...) using pre-compiled regex
	out = reCSSCidURL.ReplaceAllStringFunc(out, func(m string) string {
		subs := reCSSCidURL.FindStringSubmatch(m)
		if len(subs) > 1 {
			cid := normalizeCIDRef(subs[1])
			if mxc, ok := cidToMXC[cid]; ok && mxc != "" {
				return "url(" + mxc + ")"
			}
		}
		return m
	})
	// Replace content-location src references
	reLoc, err := regexp.Compile(`(?i)src\s*=\s*(['\"])([^'\"]+)(['\"])`)
	if err == nil {
		out = reLoc.ReplaceAllStringFunc(out, func(m string) string {
			subs := reLoc.FindStringSubmatch(m)
			if len(subs) > 3 {
				open := subs[1]
				val := subs[2]
				close := subs[3]
				// ensure matching quotes
				if open != close {
					return m
				}
				low := strings.ToLower(val)
				if strings.HasPrefix(low, "http:") || strings.HasPrefix(low, "https:") || strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "cid:") {
					return m
				}
				key := normalizeContentLocation(val)
				if mxc, ok := locToMXC[key]; ok && mxc != "" {
					return "src=" + open + mxc + close
				}
			}
			return m
		})
	}
	return out
}

// lightMinifyHTML removes comments and collapses whitespace conservatively to preserve formatting fidelity.
func lightMinifyHTML(s string) string {
	before := len(s)
	s = removeHTMLComments(s)
	s = collapseWhitespace(s)
	_ = before // keep variable for potential future metrics
	return s
}

// simpleHTMLToText converts basic HTML into readable plaintext.
// It strips script/style, removes tags, collapses whitespace, and decodes common entities.
func simpleHTMLToText(s string) string {
	// Remove script and style blocks
	s = stripTagContent(s, "script")
	s = stripTagContent(s, "style")
	// Replace <br> and <p> with newlines to preserve structure
	reBR := regexp.MustCompile(`(?is)<\s*br\s*/?>`)
	s = reBR.ReplaceAllString(s, "\n")
	reP := regexp.MustCompile(`(?is)<\s*/?p\s*>`)
	s = reP.ReplaceAllString(s, "\n")
	// Strip remaining tags
	reTags := regexp.MustCompile(`(?is)<[^>]+>`)
	s = reTags.ReplaceAllString(s, "")
	// Decode all HTML entities (including numeric ones like &#847; and &zwnj;)
	s = html.UnescapeString(s)
	// Filter out invisible/formatting Unicode characters
	s = filterInvisibleUnicode(s)
	// Collapse whitespace
	s = strings.TrimSpace(collapseWhitespace(s))
	return s
}

// filterInvisibleUnicode removes invisible Unicode characters in a single pass
func filterInvisibleUnicode(s string) string {
	var result strings.Builder
	result.Grow(len(s)) // Pre-allocate capacity

	for _, r := range s {
		// Skip format characters (Cf) and nonspacing marks (Mn) - covers most invisible chars
		if !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Mn, r) {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// generateParticipantChangeMessage creates a timeline message for participant changes
func generateParticipantChangeMessage(thread *EmailThread) string {
	if len(thread.AddedParticipants) == 0 && len(thread.RemovedParticipants) == 0 {
		return ""
	}

	var messages []string

	// Handle added participants
	if len(thread.AddedParticipants) > 0 {
		if len(thread.AddedParticipants) == 1 {
			messages = append(messages, fmt.Sprintf("📧 %s joined the conversation", thread.AddedParticipants[0]))
		} else {
			messages = append(messages, fmt.Sprintf("📧 %s joined the conversation", strings.Join(thread.AddedParticipants, ", ")))
		}
	}

	// Handle removed participants
	if len(thread.RemovedParticipants) > 0 {
		if len(thread.RemovedParticipants) == 1 {
			messages = append(messages, fmt.Sprintf("📧 %s was removed from the conversation", thread.RemovedParticipants[0]))
		} else {
			messages = append(messages, fmt.Sprintf("📧 %s were removed from the conversation", strings.Join(thread.RemovedParticipants, ", ")))
		}
	}

	return strings.Join(messages, "\n")
}
