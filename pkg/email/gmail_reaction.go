package email

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"

	gomail "github.com/emersion/go-message/mail"
	msgtextproto "github.com/emersion/go-message/textproto"
	"github.com/rivo/uniseg"
	"go.mau.fi/util/emojirunes"
	gmail "google.golang.org/api/gmail/v1"
)

// isSingleEmoji reports whether s is exactly one emoji: every rune is an emoji
// rune (or a joiner, variation selector or keycap part of one), the whole string
// is one grapheme cluster, and it is not a bare joiner, selector or lone
// regional-indicator letter. Gmail shows anything else as an ordinary email.
func isSingleEmoji(s string) bool {
	if !emojirunes.IsOnlyEmojis(s) || uniseg.GraphemeClusterCount(s) != 1 {
		return false
	}
	var visible, regional int
	for _, r := range s {
		switch {
		case r == '\u200d' || r == '\ufe0f':
		case r >= 0x1F1E6 && r <= 0x1F1FF:
			regional++
			visible++
		default:
			visible++
		}
	}
	return visible > 0 && (regional == 0 || regional == 2)
}

func BuildGmailReaction(target *gmail.Message, own, emoji string) ([]byte, error) {
	if !isSingleEmoji(emoji) {
		return nil, errors.New("Gmail reactions require exactly one Unicode emoji")
	}
	headers := target.Payload.Headers
	parse := func(name string) ([]*mail.Address, error) {
		var values []*mail.Address
		for _, h := range headers {
			if h == nil || !strings.EqualFold(h.Name, name) {
				continue
			}
			addresses, err := mail.ParseAddressList(h.Value)
			if err != nil {
				return nil, fmt.Errorf("reaction target has invalid %s", name)
			}
			values = append(values, addresses...)
		}
		return values, nil
	}
	from, err := parse("From")
	if err != nil {
		return nil, err
	}
	if len(from) != 1 {
		return nil, errors.New("reaction target must have one sender")
	}
	to, err := parse("To")
	if err != nil {
		return nil, err
	}
	cc, err := parse("Cc")
	if err != nil {
		return nil, err
	}
	recipients := make(map[string]bool)
	for _, addr := range append(to, cc...) {
		recipients[strings.ToLower(addr.Address)] = true
	}
	if !recipients[strings.ToLower(own)] {
		return nil, errors.New("Gmail reactions are unavailable for Bcc or group delivery")
	}
	if len(recipients) > 20 {
		return nil, errors.New("Gmail reactions are unavailable for more than 20 recipients")
	}
	for _, h := range headers {
		if h != nil && strings.EqualFold(h.Name, "List-Id") {
			return nil, errors.New("Gmail reactions are unavailable for mailing lists")
		}
	}
	reply, err := parse("Reply-To")
	if err != nil {
		return nil, err
	}
	if len(reply) > 0 && (len(reply) != 1 || !strings.EqualFold(reply[0].Address, from[0].Address)) {
		return nil, errors.New("Gmail reactions are unavailable for a custom Reply-To")
	}
	targetID := cleanMessageID(headerValue(headers, "Message-ID"))
	if targetID == "" || strings.ContainsAny(targetID, "\r\n<> \t") {
		return nil, errors.New("reaction target has no single valid Message-ID")
	}
	subject := strings.ReplaceAll(strings.ReplaceAll(headerValue(headers, "Subject"), "\r", ""), "\n", "")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	payload, err := json.Marshal(struct {
		Version int    `json:"version"`
		Emoji   string `json:"emoji"`
	}{1, emoji})
	if err != nil {
		return nil, err
	}
	for _, part := range []struct{ kind, content string }{
		{"text/plain", own + " reacted via Gmail: " + emoji},
		{"text/vnd.google.email-reaction+json", string(payload)},
		{"text/html", "<p>" + html.EscapeString(own+" reacted via Gmail: "+emoji) + "</p>"},
	} {
		stream, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {part.kind + "; charset=UTF-8"}, "Content-Transfer-Encoding": {"8bit"}})
		if err != nil {
			return nil, err
		}
		if _, err := stream.Write([]byte(part.content)); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	// Reply to the sender and the original visible recipients, excluding ourselves.
	destinations := []*mail.Address{from[0]}
	seen := map[string]bool{strings.ToLower(own): true, strings.ToLower(from[0].Address): true}
	for _, addr := range append(to, cc...) {
		key := strings.ToLower(addr.Address)
		if !seen[key] {
			destinations = append(destinations, addr)
			seen[key] = true
		}
	}
	var raw bytes.Buffer

	var header gomail.Header
	header.SetAddressList("From", []*mail.Address{{Address: own}})
	header.SetAddressList("To", destinations)
	header.SetSubject(subject)
	header.Set("In-Reply-To", "<"+targetID+">")
	references := parseReferencesHeader(headerValue(headers, "References"))
	references = append(references, targetID)
	encodedReferences := make([]string, 0, len(references))
	for _, ref := range references {
		if strings.ContainsAny(ref, "\r\n<> \t") {
			return nil, errors.New("reaction target has invalid References")
		}
		encodedReferences = append(encodedReferences, "<"+ref+">")
	}
	header.Set("References", strings.Join(encodedReferences, " "))
	header.Set("MIME-Version", "1.0")
	header.SetContentType("multipart/alternative", map[string]string{"boundary": writer.Boundary()})
	if err := msgtextproto.WriteHeader(&raw, header.Header.Header); err != nil {
		return nil, err
	}
	raw.Write(body.Bytes())
	return raw.Bytes(), nil
}
