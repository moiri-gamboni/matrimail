package email

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Media reaches the room only through uploadAttachment, which encrypts it
// for the portal's room. bridgev2's UploadMedia encrypts only when it is
// given the ID of an encrypted room; with no room ID it uploads the bytes,
// file name and type in the clear. Inline images follow the same path: an
// encrypted file needs its key in the event's `file` object, which an
// <img src="mxc://..."> in the formatted body cannot carry, so each one is
// sent as its own image event after the text and the HTML keeps its alt
// text.

// The reasons a file is not bridged, as the room's notice names them; the
// underlying error goes to the log.
var (
	errNoRoom   = errors.New("no room to encrypt it for")
	errTooLarge = errors.New("too large")
	errDownload = errors.New("download failed")
	errUpload   = errors.New("upload failed")
)

// uploadAttachment uploads att for roomID and returns the event content that
// shows it: content.File when the room is encrypted, content.URL when it is
// not. Nothing is uploaded without a room ID, and an attachment over maxBytes
// (0: no limit) is refused on its declared size, before any download.
func uploadAttachment(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, att *EmailAttachment, msgType event.MessageType, body string, maxBytes int) (*event.MessageEventContent, error) {
	if maxBytes > 0 && att.Size > int64(maxBytes) {
		return nil, errTooLarge
	}
	if roomID == "" {
		return nil, errNoRoom
	}
	if att.Data == nil && att.load != nil {
		data, err := att.load(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errDownload, err)
		}
		att.Data = data
	}
	if maxBytes > 0 && len(att.Data) > maxBytes {
		return nil, errTooLarge
	}
	name := sanitizeFilename(att.Filename)
	if name == "" {
		name = sanitizeFilename(bestFilename(att, "attachment"))
	}
	url, file, err := intent.UploadMedia(ctx, roomID, att.Data, name, att.ContentType)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUpload, err)
	}
	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    body,
		Info:    &event.FileInfo{MimeType: att.ContentType, Size: len(att.Data)},
	}
	if file != nil {
		content.File = file
	} else {
		content.URL = url
	}
	return content, nil
}

// mediaPart is the message part for one attachment: the uploaded media, or
// (ok false) a notice naming what did not arrive and why, so a failure costs
// that file and never the message.
func (e *EmailMatrixEvent) mediaPart(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, partID string, att *EmailAttachment, msgType event.MessageType, body string) (part *bridgev2.ConvertedMessagePart, ok bool) {
	content, err := uploadAttachment(ctx, intent, roomID, att, msgType, body, e.processor.MaxUploadBytes)
	if err != nil {
		e.processor.log.Warn().Err(err).Str("filename", att.Filename).Str("content_type", att.ContentType).
			Int64("size", att.Size).Msg("Attachment not bridged")
		content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: notBridgedNotice(att, err, e.processor.MaxUploadBytes)}
	}
	return &bridgev2.ConvertedMessagePart{ID: networkid.PartID(partID), Type: event.EventMessage, Content: content}, err == nil
}

func notBridgedNotice(att *EmailAttachment, err error, maxBytes int) string {
	var reason string
	switch {
	case errors.Is(err, errTooLarge):
		reason = "too large (upload limit " + formatSize(int64(maxBytes)) + ")"
	case errors.Is(err, errDownload):
		reason = errDownload.Error()
	case errors.Is(err, errUpload):
		reason = errUpload.Error()
	default:
		reason = err.Error()
	}
	name := att.Filename
	if name == "" {
		name = bestFilename(att, "attachment")
	}
	return fmt.Sprintf("📎 not bridged: %s (%s, %s): %s", name, att.ContentType, formatSize(att.Size), reason)
}

func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}

// inlineImage is an image the HTML shows in place, sent as its own image
// event after the text.
type inlineImage struct {
	index int
	label string
	att   *EmailAttachment
}

var (
	reImgTag     = regexp.MustCompile(`(?is)<\s*img\b[^>]*>`)
	reCSSDataURL = regexp.MustCompile(`(?i)url\(\s*['"]?data:[^)]*\)`)
	reDataURI    = regexp.MustCompile(`(?is)^data:([a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+);base64,(.*)$`)
	reSrcAttr    = attrRegexp("src")
	reAltAttr    = attrRegexp("alt")
	reTitleAttr  = attrRegexp("title")
)

// attrRegexp matches one attribute of a tag, not a longer name ending in it
// (data-src); the value is group 1.
func attrRegexp(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\s` + name + `\s*=\s*('[^']*'|"[^"]*"|[^\s>]+)`)
}

// tagAttr returns an attribute's value, unquoted and with its character
// references decoded.
func tagAttr(tag string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(strings.Trim(m[1], `"'`)))
}

// decodeDataURI turns a base64 data: URI into an attachment.
func decodeDataURI(src string) (*EmailAttachment, bool) {
	m := reDataURI.FindStringSubmatch(strings.TrimSpace(src))
	if m == nil {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m[2]), ""))
	if err != nil {
		return nil, false
	}
	mimeType := strings.ToLower(m[1])
	name := "inline"
	if sub, ok := strings.CutPrefix(mimeType, "image/"); ok {
		name += "." + sub
	}
	return &EmailAttachment{Filename: sanitizeFilename(name), ContentType: mimeType, Size: int64(len(data)), Data: data}, true
}

// inlineSource resolves an <img> src to the attachment holding its bytes:
// a cid: reference, a data: URI or a Content-Location. remote reports an
// http(s) image, which is never fetched.
func inlineSource(atts []*EmailAttachment, src string) (att *EmailAttachment, fallbackName string, remote bool) {
	low := strings.ToLower(src)
	switch {
	case strings.HasPrefix(low, "http:"), strings.HasPrefix(low, "https:"):
		return nil, "", true
	case strings.HasPrefix(low, "data:"):
		if a, ok := decodeDataURI(src); ok {
			return a, a.Filename, false
		}
	case strings.HasPrefix(low, "cid:"):
		cid := normalizeCIDRef(src)
		if idx := findAttachmentByCID(atts, cid); idx >= 0 {
			return atts[idx], cid, false
		}
	case src != "" && !strings.HasPrefix(low, "mxc:"):
		key := normalizeContentLocation(src)
		if idx := findAttachmentByContentLocation(atts, key); idx >= 0 {
			return atts[idx], key, false
		}
	}
	return nil, "", false
}

// referencedInline returns the attachments the HTML shows in place, through
// an <img> or a cid: background, quoted history included. They are not
// also sent as file attachments.
func referencedInline(atts []*EmailAttachment, htmlBody string) map[*EmailAttachment]bool {
	used := map[*EmailAttachment]bool{}
	if htmlBody == "" {
		return used
	}
	for _, tag := range reImgTag.FindAllString(htmlBody, -1) {
		src := tagAttr(tag, reSrcAttr)
		if strings.HasPrefix(strings.ToLower(src), "data:") {
			continue // a data: URI is its own image, never an attachment
		}
		if att, _, _ := inlineSource(atts, src); att != nil {
			used[att] = true
		}
	}
	for _, cid := range backgroundImageCIDs(htmlBody) {
		if idx := findAttachmentByCID(atts, cid); idx >= 0 {
			used[atts[idx]] = true
		}
	}
	return used
}

// extractInlineImages takes the images out of the HTML: each <img> becomes
// its alt text, with the number of the image event that carries it, and
// each image shown only as a cid: CSS background (which the client's
// sanitizer strips anyway) is listed after them. Remote images are removed
// and never fetched; a decorative image is not sent, and the text says so,
// so a small image that was content is not lost without trace. Data URIs in
// CSS are dropped: the client never renders them and they bloat the body.
func extractInlineImages(atts []*EmailAttachment, htmlBody string) (string, []*inlineImage) {
	var images []*inlineImage
	queued := map[*EmailAttachment]*inlineImage{}
	queue := func(att *EmailAttachment, label string) *inlineImage {
		if im := queued[att]; im != nil {
			return im
		}
		if isLikelyDecorativeImage(att.Filename, att.Size) {
			return nil
		}
		im := &inlineImage{index: len(images) + 1, label: label, att: att}
		images = append(images, im)
		queued[att] = im
		return im
	}

	out := reImgTag.ReplaceAllStringFunc(htmlBody, func(tag string) string {
		src := tagAttr(tag, reSrcAttr)
		// Read the alt text with the src attribute taken out, so a data:
		// URI's payload is never mistaken for an attribute.
		rest := reSrcAttr.ReplaceAllString(tag, " ")
		alt := tagAttr(rest, reAltAttr)
		if alt == "" {
			alt = tagAttr(rest, reTitleAttr)
		}
		att, fallback, remote := inlineSource(atts, src)
		switch {
		case remote:
			return ""
		case att == nil:
			return "[Image]"
		}
		label := alt
		if label == "" {
			label = bestFilename(att, fallback)
		}
		if im := queue(att, label); im != nil {
			return html.EscapeString(fmt.Sprintf("[Image %d: %s]", im.index, im.label))
		}
		return html.EscapeString("[Image not shown: " + label + "]")
	})
	for _, cid := range backgroundImageCIDs(out) {
		if idx := findAttachmentByCID(atts, cid); idx >= 0 {
			queue(atts[idx], bestFilename(atts[idx], cid))
		}
	}
	return reCSSDataURL.ReplaceAllString(out, "none"), images
}
