package email

import (
	"encoding/base64"
	"strings"
	"testing"

	gmail "google.golang.org/api/gmail/v1"
)

// The Gmail-API payload walker is fed entirely by remote input.

// A part carrying a filename but no body used to panic on part.Body.Size,
// while the two text branches beside it guarded the same field. The panic
// happens inside the history poller's goroutine, so it takes the whole bridge
// down rather than failing the one message.
func TestWalkGmailPayload_AttachmentWithNoBodyDoesNotPanic(t *testing.T) {
	t.Parallel()

	parsed := &ParsedEmail{}
	walkGmailPayload(&gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			{MimeType: "application/pdf", Filename: "report.pdf", Body: nil},
		},
	}, parsed, 0, nil)

	if len(parsed.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(parsed.Attachments))
	}
	if parsed.Attachments[0].Filename != "report.pdf" {
		t.Errorf("filename = %q", parsed.Attachments[0].Filename)
	}
	if parsed.Attachments[0].Size != 0 {
		t.Errorf("size = %d, want 0 for a part with no body", parsed.Attachments[0].Size)
	}
}

// Ordinary nesting must still be walked: the depth bound is worthless if it
// costs the body of a normal message.
func TestWalkGmailPayload_ReadsBodyThroughOrdinaryNesting(t *testing.T) {
	t.Parallel()

	enc := func(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
	parsed := &ParsedEmail{}
	walkGmailPayload(&gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{{
			MimeType: "multipart/alternative",
			Parts: []*gmail.MessagePart{
				{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: enc("plain body")}},
				{MimeType: "text/html", Body: &gmail.MessagePartBody{Data: enc("<p>html body</p>")}},
			},
		}},
	}, parsed, 0, nil)

	if parsed.TextContent != "plain body" {
		t.Errorf("TextContent = %q", parsed.TextContent)
	}
	if !strings.Contains(parsed.HTMLContent, "html body") {
		t.Errorf("HTMLContent = %q", parsed.HTMLContent)
	}
}

// Nesting past the limit stops rather than recursing to the depth of the
// input. Returns rather than erroring: the walker has no error channel, and a
// message nested this deep is hostile, not a delivery to preserve.
func TestWalkGmailPayload_StopsAtTheDepthLimit(t *testing.T) {
	t.Parallel()

	enc := base64.URLEncoding.EncodeToString([]byte("buried"))
	// One level deeper than the limit allows, with the body at the bottom.
	deepest := &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: enc}}
	part := deepest
	for i := 0; i < maxMultipartDepth+1; i++ {
		part = &gmail.MessagePart{MimeType: "multipart/mixed", Parts: []*gmail.MessagePart{part}}
	}

	parsed := &ParsedEmail{}
	walkGmailPayload(part, parsed, 0, nil)

	if parsed.TextContent != "" {
		t.Errorf("TextContent = %q; a body past the depth limit must not be reached", parsed.TextContent)
	}

	// And the same body inside the limit is still read, so the test above is
	// measuring the limit rather than a broken walker.
	shallow := &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: enc}}
	for i := 0; i < maxMultipartDepth-2; i++ {
		shallow = &gmail.MessagePart{MimeType: "multipart/mixed", Parts: []*gmail.MessagePart{shallow}}
	}
	within := &ParsedEmail{}
	walkGmailPayload(shallow, within, 0, nil)
	if within.TextContent != "buried" {
		t.Errorf("TextContent = %q; nesting within the limit must still be read", within.TextContent)
	}
}
