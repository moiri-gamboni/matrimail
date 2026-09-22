package email

import (
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
	"time"

	gmail "google.golang.org/api/gmail/v1"
)

// ParseGmailAPIMessage converts a gmail.Message (as returned by
// users.messages.get with format="full") into the ParsedEmail shape the
// processor's threading/dedup/portal pipeline consumes.
//
// Gmail's payload tree is recursive: a top-level part can be multipart with
// children that are themselves multipart, and so on. This function walks the
// tree to find the first text/plain and text/html bodies and surfaces
// attachments by metadata only — actual attachment bytes require a separate
// users.messages.attachments.get call per part and are deferred to the
// connector's bridge-side ingestion path so we don't pull megabytes of
// attachment data on every poll iteration.
func ParseGmailAPIMessage(msg *gmail.Message) (*ParsedEmail, error) {
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
	walkGmailPayload(msg.Payload, parsed, 0)

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
func walkGmailPayload(part *gmail.MessagePart, out *ParsedEmail, depth int) {
	if part == nil || depth >= maxMultipartDepth {
		return
	}

	mimeType := strings.ToLower(part.MimeType)
	disposition := strings.ToLower(headerValue(part.Headers, "Content-Disposition"))
	isAttachment := strings.HasPrefix(disposition, "attachment") ||
		(part.Filename != "" && !strings.HasPrefix(mimeType, "multipart/"))

	switch {
	case isAttachment:
		// Surface metadata only; the attachment bytes live behind a separate
		// users.messages.attachments.get call (part.Body.AttachmentId). We
		// record enough for downstream code to fetch them on demand if it
		// chooses; v1 of the Gmail-API inbound path skips actually attaching
		// files to keep poll cost low — IMAP mode already covers attachment
		// fidelity when users need it.
		// part.Body is optional in the API's schema, and the two text
		// branches below already guard it. Unguarded here, a part carrying a
		// filename and no body panics -- inside the poller goroutine, which
		// takes the bridge down rather than failing one message.
		var size int64
		if part.Body != nil {
			size = part.Body.Size
		}
		out.Attachments = append(out.Attachments, &EmailAttachment{
			Filename:    part.Filename,
			ContentType: part.MimeType,
			Size:        size,
		})
	case strings.HasPrefix(mimeType, "multipart/"):
		for _, child := range part.Parts {
			walkGmailPayload(child, out, depth+1)
		}
	case mimeType == "text/plain":
		if out.TextContent == "" && part.Body != nil && part.Body.Data != "" {
			if decoded, err := base64.URLEncoding.DecodeString(padBase64URL(part.Body.Data)); err == nil {
				out.TextContent = string(decoded)
			}
		}
	case mimeType == "text/html":
		if out.HTMLContent == "" && part.Body != nil && part.Body.Data != "" {
			if decoded, err := base64.URLEncoding.DecodeString(padBase64URL(part.Body.Data)); err == nil {
				out.HTMLContent = string(decoded)
			}
		}
	default:
		// Unknown leaf — skip. Recurse if it somehow has children.
		for _, child := range part.Parts {
			walkGmailPayload(child, out, depth+1)
		}
	}
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
				out = append(out, fmt.Sprintf("%s <%s>", a.Name, a.Address))
			} else {
				out = append(out, a.Address)
			}
		}
		return out
	}
	// Fallback: dumb split. Loses fidelity for display names with commas
	// but better than dropping the addresses entirely.
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
