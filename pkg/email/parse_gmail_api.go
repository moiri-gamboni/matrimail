package email

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	gmail "google.golang.org/api/gmail/v1"
)

// GmailAttachmentFetch downloads one attachment's bytes
// (users.messages.attachments.get).
type GmailAttachmentFetch func(ctx context.Context, messageID, attachmentID string) ([]byte, error)

// ParseGmailAPIMessage converts a gmail.Message (as returned by
// users.messages.get with format="full") into the ParsedEmail shape the
// processor's threading/dedup/portal pipeline consumes.
//
// Gmail's payload tree is recursive: a top-level part can be multipart with
// children that are themselves multipart, and so on. This function walks the
// tree to find the first text/plain and text/html bodies and the
// attachments. An attachment's bytes come inline in body.data when small;
// otherwise they sit behind users.messages.attachments.get, and fetch is
// only called when the message is converted for the room, for an attachment
// under the upload limit, so a poll never downloads what is not bridged.
func ParseGmailAPIMessage(msg *gmail.Message, fetch GmailAttachmentFetch) (*ParsedEmail, error) {
	if msg == nil {
		return nil, fmt.Errorf("ParseGmailAPIMessage: nil message")
	}
	if msg.Payload == nil {
		return nil, fmt.Errorf("ParseGmailAPIMessage: message %s has no payload (was format=full passed?)", msg.Id)
	}

	parsed := &ParsedEmail{
		// Fallback Message-ID if no header is present (rare; Gmail synthesises
		// one on send, but inbound messages from broken senders may lack one).
		MessageID:     fmt.Sprintf("gmail-api-%s", msg.Id),
		Date:          time.Unix(0, msg.InternalDate*int64(time.Millisecond)),
		GmailThreadID: msg.ThreadId,
	}

	for _, h := range msg.Payload.Headers {
		if h == nil {
			continue
		}
		switch strings.ToLower(h.Name) {
		case "message-id":
			if v := cleanMessageID(h.Value); v != "" {
				parsed.MessageID = v
			}
		case "subject":
			parsed.Subject = h.Value
		case "from":
			parsed.From = h.Value
		case "to":
			parsed.To = splitAddressList(h.Value)
		case "cc":
			parsed.Cc = splitAddressList(h.Value)
		case "bcc":
			parsed.Bcc = splitAddressList(h.Value)
		case "in-reply-to":
			if v := cleanMessageID(h.Value); v != "" {
				parsed.InReplyTo = v
			}
		case "references":
			parsed.References = parseReferencesHeader(h.Value)
		case "date":
			if t, err := mail.ParseDate(h.Value); err == nil {
				parsed.Date = t
			}
		}
	}

	// Walk the payload tree for body parts and attachments.
	messageID := msg.Id // not msg: the closure outlives the parse
	walkGmailPayload(msg.Payload, parsed, 0, func(ctx context.Context, attachmentID string) ([]byte, error) {
		return fetch(ctx, messageID, attachmentID)
	})
	parsed.Attachments = dropUnusedInlineParts(parsed.Attachments, parsed.HTMLContent)

	// Fall back to the snippet if no body was found (rare, but can happen for
	// messages with all parts marked as attachments).
	if parsed.TextContent == "" && msg.Snippet != "" {
		parsed.TextContent = msg.Snippet
	}
	return parsed, nil
}

// walkGmailPayload recursively descends a gmail.MessagePart, populating
// TextContent / HTMLContent (first match wins) and Attachments.
//
// Bounded at the same depth as the MIME parsers in processor.go. The nesting
// here arrives already decoded by the Gmail client, so the quadratic re-read
// that motivated the limit there does not apply, and encoding/json's own
// nesting cap means a hostile message cannot currently reach a stack overflow
// through this path. That is an accident of a limit in another package, though
// -- it is not documented as a guarantee and costs nothing to stop relying on.
func walkGmailPayload(part *gmail.MessagePart, out *ParsedEmail, depth int, fetch func(ctx context.Context, attachmentID string) ([]byte, error)) {
	if part == nil || depth >= maxMultipartDepth {
		return
	}

	mimeType := strings.ToLower(part.MimeType)
	disposition := strings.ToLower(headerValue(part.Headers, "Content-Disposition"))
	isMultipart := strings.HasPrefix(mimeType, "multipart/")
	isAttachment := strings.HasPrefix(disposition, "attachment") || (part.Filename != "" && !isMultipart)
	// A part the HTML shows in place: an image pasted into the message, or a
	// logo, referenced by Content-ID or Content-Location.
	isInlineReference := !isMultipart && mimeType != "text/plain" && mimeType != "text/html" &&
		(headerValue(part.Headers, "Content-ID") != "" || headerValue(part.Headers, "Content-Location") != "")

	switch {
	case isAttachment || isInlineReference:
		att := gmailAttachment(part, disposition, fetch)
		// The same tracking-pixel filter the IMAP path applies.
		if isInlineReference && strings.HasPrefix(mimeType, "image/") && att.Size < 256 {
			return
		}
		out.Attachments = append(out.Attachments, att)
	case isMultipart:
		for _, child := range part.Parts {
			walkGmailPayload(child, out, depth+1, fetch)
		}
	case mimeType == "text/plain":
		if out.TextContent == "" && part.Body != nil && part.Body.Data != "" {
			if decoded, err := decodeGmailData(part.Body.Data); err == nil {
				out.TextContent = string(decoded)
			}
		}
	case mimeType == "text/html":
		if out.HTMLContent == "" && part.Body != nil && part.Body.Data != "" {
			if decoded, err := decodeGmailData(part.Body.Data); err == nil {
				out.HTMLContent = string(decoded)
			}
		}
	default:
		// Unknown leaf — skip. Recurse if it somehow has children.
		for _, child := range part.Parts {
			walkGmailPayload(child, out, depth+1, fetch)
		}
	}
}

// gmailAttachment records one attachment part. Its bytes are decoded now
// when the part carries them in body.data, and otherwise fetched on first
// use through the part's attachment ID.
func gmailAttachment(part *gmail.MessagePart, disposition string, fetch func(ctx context.Context, attachmentID string) ([]byte, error)) *EmailAttachment {
	contentID := normalizeCIDHeader(headerValue(part.Headers, "Content-ID"))
	contentLocation := normalizeContentLocation(headerValue(part.Headers, "Content-Location"))
	disposition = strings.TrimSpace(strings.Split(disposition, ";")[0])
	att := &EmailAttachment{
		Filename:        part.Filename,
		ContentType:     part.MimeType,
		ContentID:       contentID,
		ContentLocation: contentLocation,
		Disposition:     disposition,
		IsInline:        disposition == "inline" || contentID != "" || contentLocation != "",
	}
	// part.Body is optional in the API's schema. Unguarded, a part carrying
	// a filename and no body panics -- inside the poller goroutine, which
	// takes the bridge down rather than failing one message.
	if part.Body == nil {
		return att
	}
	att.Size = part.Body.Size
	switch {
	case part.Body.Data != "":
		data, err := decodeGmailData(part.Body.Data)
		if err != nil {
			att.load = func(context.Context) ([]byte, error) { return nil, fmt.Errorf("decode body data: %w", err) }
		} else {
			att.Data = data
		}
	case part.Body.AttachmentId != "":
		attachmentID := part.Body.AttachmentId
		att.load = func(ctx context.Context) ([]byte, error) { return fetch(ctx, attachmentID) }
	}
	return att
}

// dropUnusedInlineParts removes the parts that exist only to be shown in
// place (no file name, not marked as an attachment) when the HTML never
// shows them, so they are neither downloaded nor sent as files.
func dropUnusedInlineParts(atts []*EmailAttachment, htmlBody string) []*EmailAttachment {
	used := referencedInline(atts, htmlBody)
	kept := atts[:0]
	for _, a := range atts {
		if a.Filename == "" && a.Disposition != "attachment" && !used[a] {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// padBase64URL adds the missing '=' padding that base64.URLEncoding requires
// but Gmail's API omits. RawURLEncoding would be the right decoder, but the
// rest of the codebase uses URLEncoding consistently, so we pad here.
func padBase64URL(s string) string {
	switch len(s) % 4 {
	case 2:
		return s + "=="
	case 3:
		return s + "="
	}
	return s
}

// headerValue returns the first matching header value (case-insensitive) from
// a gmail.MessagePart's header slice. Empty string when not present.
func headerValue(headers []*gmail.MessagePartHeader, name string) string {
	want := strings.ToLower(name)
	for _, h := range headers {
		if h != nil && strings.ToLower(h.Name) == want {
			return h.Value
		}
	}
	return ""
}

// splitAddressList splits a comma-separated address list while respecting
// quoted display names (which can contain commas). Falls back to a simple
// split if the strict parser rejects the input.
func splitAddressList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if list, err := mail.ParseAddressList(raw); err == nil && len(list) > 0 {
		out := make([]string, 0, len(list))
		for _, a := range list {
			if a == nil {
				continue
			}
			if a.Name != "" {
				// Must round-trip through net/mail so a display name containing
				// a comma ("Doe, John", the Exchange default) is quoted. Emitted
				// unquoted it fails to re-parse on the send path, where the
				// recipient is then dropped from reply-all with no log.
				out = append(out, (&mail.Address{Name: a.Name, Address: a.Address}).String())
			} else {
				out = append(out, a.Address)
			}
		}
		return out
	}
	// Fallback for input the strict parser rejects: split on commas. This
	// does lose display names containing commas -- but only here; the branch
	// above preserves them.
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseReferencesHeader splits a References header into individual Message-IDs.
// References is whitespace-separated <id>-bracketed values.
func parseReferencesHeader(raw string) []string {
	fields := strings.Fields(raw)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := cleanMessageID(f); id != "" {
			out = append(out, id)
		}
	}
	return out
}
