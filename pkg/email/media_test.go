package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const testRoom id.RoomID = "!room:example.com"

type fakeUpload struct {
	roomID     id.RoomID
	data       []byte
	name, mime string
}

// fakeIntent stands in for the bridge's Matrix intent. Its UploadMedia
// mirrors bridgev2's ASIntent.UploadMedia: for a room the state store marks
// encrypted it encrypts the data in place, returns an empty URL and puts the
// mxc URI in the EncryptedFileInfo; for any other room ID it returns a plain URL; with an
// empty room ID it uploads the bytes as they are, which is the leak these
// tests exist to catch, so it fails the test. err simulates the upload or the
// encryption check failing, which ASIntent reports before uploading anything.
type fakeIntent struct {
	bridgev2.MatrixAPI // any other method panics

	t         *testing.T
	encrypted map[id.RoomID]bool
	err       error
	uploads   []fakeUpload
}

func (u fakeUpload) String() string {
	return fmt.Sprintf("%s (%s, %d bytes) for room %q", u.name, u.mime, len(u.data), u.roomID)
}

func (f *fakeIntent) UploadMedia(_ context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	if roomID == "" {
		f.t.Errorf("upload of %q (%s) made with no room ID: it reaches the media server unencrypted", fileName, mimeType)
	}
	f.uploads = append(f.uploads, fakeUpload{roomID: roomID, data: bytes.Clone(data), name: fileName, mime: mimeType})
	if f.err != nil {
		return "", nil, f.err
	}
	uri := id.ContentURIString(fmt.Sprintf("mxc://example.com/media%d", len(f.uploads)))
	if f.encrypted[roomID] {
		// ASIntent encrypts the caller's slice in place.
		for i := range data {
			data[i] ^= 0xff
		}
		return "", &event.EncryptedFileInfo{URL: uri}, nil
	}
	return uri, nil, nil
}

func newFakeIntent(t *testing.T) *fakeIntent {
	return &fakeIntent{t: t, encrypted: map[id.RoomID]bool{testRoom: true}}
}

func testPortal(room id.RoomID) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{MXID: room}}
}

// convertForTest runs ConvertMessage for a portal whose room is room.
func convertForTest(t *testing.T, parsed *ParsedEmail, room id.RoomID, intent *fakeIntent, maxUpload int) []*bridgev2.ConvertedMessagePart {
	t.Helper()
	log := zerolog.Nop()
	p := NewProcessor(&log, nil, false, "")
	p.MaxUploadBytes = maxUpload
	ev := &EmailMatrixEvent{
		emailMessage: &EmailMessage{ParsedEmail: parsed, Thread: &EmailThread{}, Attachments: parsed.Attachments},
		processor:    p,
	}
	cm, err := ev.ConvertMessage(context.Background(), testPortal(room), intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range cm.Parts {
		if part.ID == "error-placeholder" {
			t.Fatalf("conversion panicked: %+v", part.Content)
		}
	}
	return cm.Parts
}

func partByID(parts []*bridgev2.ConvertedMessagePart, pid string) *bridgev2.ConvertedMessagePart {
	for _, p := range parts {
		if string(p.ID) == pid {
			return p
		}
	}
	return nil
}

func partIDs(parts []*bridgev2.ConvertedMessagePart) []string {
	ids := make([]string, len(parts))
	for i, p := range parts {
		ids[i] = string(p.ID)
	}
	return ids
}

// assertEncryptedMedia checks a media part the way a client reads it: the
// file key and the mxc URI of the ciphertext in content.file, nothing in
// content.url.
func assertEncryptedMedia(t *testing.T, parts []*bridgev2.ConvertedMessagePart, pid string, msgType event.MessageType) *event.MessageEventContent {
	t.Helper()
	p := partByID(parts, pid)
	if p == nil {
		t.Fatalf("no part %q; parts: %v", pid, partIDs(parts))
	}
	c := p.Content
	if c.MsgType != msgType {
		t.Errorf("part %q: msgtype %q, want %q (body %q)", pid, c.MsgType, msgType, c.Body)
	}
	if c.File == nil || c.File.URL == "" {
		t.Errorf("part %q: no encrypted file info (file=%+v)", pid, c.File)
	}
	if c.URL != "" {
		t.Errorf("part %q: plain url %q set in an encrypted room", pid, c.URL)
	}
	return c
}

func filler(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

func TestConvertMessage_AttachmentUploadedEncryptedForTheRoom(t *testing.T) {
	data := filler(1234, 'p')
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "See attached",
		Attachments: []*EmailAttachment{{Filename: "report.pdf", ContentType: "application/pdf", Size: 1234, Data: bytes.Clone(data)}},
	}, testRoom, intent, 0)

	c := assertEncryptedMedia(t, parts, "att-1-report.pdf", event.MsgFile)
	if c.Body != "report.pdf" || c.Info == nil || c.Info.MimeType != "application/pdf" || c.Info.Size != 1234 {
		t.Errorf("content = body %q info %+v", c.Body, c.Info)
	}
	if len(intent.uploads) != 1 || intent.uploads[0].roomID != testRoom || !bytes.Equal(intent.uploads[0].data, data) {
		t.Errorf("uploads = %+v", intent.uploads)
	}
}

func TestConvertMessage_UnencryptedRoomUsesPlainURL(t *testing.T) {
	intent := newFakeIntent(t)
	const plainRoom id.RoomID = "!plain:example.com"
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "See attached",
		Attachments: []*EmailAttachment{{Filename: "report.pdf", ContentType: "application/pdf", Size: 3, Data: []byte("pdf")}},
	}, plainRoom, intent, 0)
	c := partByID(parts, "att-1-report.pdf").Content
	if c.URL == "" || c.File != nil {
		t.Errorf("url %q file %+v; want a plain url only", c.URL, c.File)
	}
	if len(intent.uploads) != 1 || intent.uploads[0].roomID != plainRoom {
		t.Errorf("uploads = %+v", intent.uploads)
	}
}

// Without a room to encrypt for, nothing is uploaded; the reader is told
// which file did not arrive.
func TestConvertMessage_NoRoomRefusesUploads(t *testing.T) {
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "See attached",
		Attachments: []*EmailAttachment{{Filename: "report.pdf", ContentType: "application/pdf", Size: 1234, Data: filler(1234, 'p')}},
	}, "", intent, 0)
	if len(intent.uploads) != 0 {
		t.Errorf("uploads made without a room: %+v", intent.uploads)
	}
	p := partByID(parts, "att-1-report.pdf")
	if p == nil {
		t.Fatalf("no part for the attachment; parts: %v", partIDs(parts))
	}
	if p.Content.MsgType != event.MsgNotice || !strings.HasPrefix(p.Content.Body, "📎 not bridged: report.pdf (application/pdf, 1.2 KiB)") {
		t.Errorf("part = %q %q", p.Content.MsgType, p.Content.Body)
	}
	if partByID(parts, "body") == nil {
		t.Errorf("message body dropped; parts: %v", partIDs(parts))
	}
}

// ASIntent returns an error without uploading when it cannot tell whether
// the room is encrypted; the file becomes a notice.
func TestConvertMessage_FailedUploadIsANotice(t *testing.T) {
	intent := newFakeIntent(t)
	intent.err = errors.New("failed to check if room is encrypted: database is locked")
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "See attached",
		Attachments: []*EmailAttachment{{Filename: "report.pdf", ContentType: "application/pdf", Size: 3, Data: []byte("pdf")}},
	}, testRoom, intent, 0)
	p := partByID(parts, "att-1-report.pdf")
	if p == nil || p.Content.MsgType != event.MsgNotice || !strings.HasPrefix(p.Content.Body, "📎 not bridged: report.pdf (application/pdf, 3 B)") {
		t.Fatalf("parts: %v; attachment part %+v", partIDs(parts), p)
	}
	if p.Content.File != nil || p.Content.URL != "" {
		t.Errorf("notice carries media: %+v", p.Content)
	}
}

func dataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// Inline images (cid, Content-Location, data: URI and cid CSS backgrounds)
// cannot stay in the HTML as mxc URIs, which carry no key: each is sent as
// an encrypted image after the text, and the HTML keeps its alt text.
func TestConvertMessage_InlineImagesSentAsEncryptedImageParts(t *testing.T) {
	shot := filler(20*1024, 's')
	photo := filler(30*1024, 'f')
	pasted := filler(25*1024, 'd')
	bg := filler(40*1024, 'b')
	htmlBody := `<p>Look at this:</p><img src="cid:shot1" alt="Screenshot of the error">` +
		`<p>and</p><img src="photo.jpg" alt="Team photo">` +
		`<img src="` + dataURI("image/png", pasted) + `" alt="Pasted chart">` +
		`<img src="https://example.com/track.gif" alt="">` +
		`<table><tr><td style="background-image: url(cid:bg1)">Banner</td></tr></table>`
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "Look at this: and",
		HTMLContent: htmlBody,
		Attachments: []*EmailAttachment{
			{Filename: "image001.png", ContentType: "image/png", Size: int64(len(shot)), Data: bytes.Clone(shot), ContentID: "shot1", IsInline: true},
			{Filename: "photo.jpg", ContentType: "image/jpeg", Size: int64(len(photo)), Data: bytes.Clone(photo), ContentLocation: "photo.jpg", IsInline: true},
			{Filename: "banner.png", ContentType: "image/png", Size: int64(len(bg)), Data: bytes.Clone(bg), ContentID: "bg1", IsInline: true},
		},
	}, testRoom, intent, 0)

	body := partByID(parts, "body")
	if body == nil {
		t.Fatalf("no body; parts: %v", partIDs(parts))
	}
	f := body.Content.FormattedBody
	if strings.Contains(f, "mxc://") || strings.Contains(f, "<img") || strings.Contains(f, "data:") {
		t.Errorf("formatted body still references images:\n%s", f)
	}
	for _, alt := range []string{"Screenshot of the error", "Team photo", "Pasted chart"} {
		if !strings.Contains(f, alt) {
			t.Errorf("alt text %q missing from formatted body:\n%s", alt, f)
		}
	}

	want := [][]byte{shot, photo, pasted, bg}
	bodyAt := -1
	for i, p := range parts {
		if p.ID == "body" {
			bodyAt = i
		}
	}
	for i, data := range want {
		pid := fmt.Sprintf("inline-image-%d", i+1)
		assertEncryptedMedia(t, parts, pid, event.MsgImage)
		for j, p := range parts {
			if string(p.ID) == pid && j < bodyAt {
				t.Errorf("%s comes before the body", pid)
			}
		}
		found := false
		for _, u := range intent.uploads {
			found = found || bytes.Equal(u.data, data)
		}
		if !found {
			t.Errorf("%s: its bytes were not uploaded", pid)
		}
	}
	if len(intent.uploads) != len(want) {
		t.Errorf("%d uploads, want %d (inline attachments must not be re-sent as files)", len(intent.uploads), len(want))
	}
	for _, p := range parts {
		if strings.HasPrefix(string(p.ID), "att-") {
			t.Errorf("inline attachment re-sent as a file: %s", p.ID)
		}
	}
}

// A tiny inline image is decoration (a logo, a spacer): it is neither
// uploaded nor sent, and the text says an image was left out.
func TestConvertMessage_DecorativeInlineImageNotUploaded(t *testing.T) {
	logo := filler(900, 'l')
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "Regards",
		HTMLContent: `<p>Regards</p><img src="cid:logo" alt="Example Inc">`,
		Attachments: []*EmailAttachment{{Filename: "logo.png", ContentType: "image/png", Size: int64(len(logo)), Data: logo, ContentID: "logo", IsInline: true}},
	}, testRoom, intent, 0)
	if len(intent.uploads) != 0 {
		t.Errorf("decorative image uploaded: %+v", intent.uploads)
	}
	if f := partByID(parts, "body").Content.FormattedBody; !strings.Contains(f, "[Image not shown: Example Inc]") || strings.Contains(f, "<img") {
		t.Errorf("formatted body: %s", f)
	}
	for _, p := range parts {
		if p.ID != "body" {
			t.Errorf("unexpected part %s: %q", p.ID, p.Content.Body)
		}
	}
}

// An image in the quoted history of a reply is not shown, so it is neither
// sent again as an image nor re-sent as a file attachment.
func TestConvertMessage_QuotedInlineImagesNotResent(t *testing.T) {
	shot := filler(20*1024, 's')
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		InReplyTo:   "parent@example.com",
		TextContent: "Thanks!",
		HTMLContent: `<div>Thanks!</div><div class="gmail_quote"><p>On Monday Alex wrote:</p><img src="cid:shot1" alt="old screenshot"></div>`,
		Attachments: []*EmailAttachment{{Filename: "image001.png", ContentType: "image/png", Size: int64(len(shot)), Data: shot, ContentID: "shot1", IsInline: true}},
	}, testRoom, intent, 0)
	if len(intent.uploads) != 0 {
		t.Errorf("quoted image uploaded: %d uploads", len(intent.uploads))
	}
	if ids := partIDs(parts); len(ids) != 1 || ids[0] != "body" {
		t.Errorf("parts = %v, want only the body", ids)
	}
}

// marketingHTML is long, layout-heavy and nearly textless, which makes the
// bridge send it as an HTML file instead of a formatted body.
func marketingHTML() string {
	return `<html><body>` + strings.Repeat(`<table><tr><td style="background-image:url(https://example.com/bg.png);padding:4px">Sale</td></tr></table>`, 200) + `</body></html>`
}

// Every upload the conversion makes, whichever path makes it, is encrypted
// for the portal's room.
func TestConvertMessage_EveryUploadIsEncryptedForTheRoom(t *testing.T) {
	img := filler(20*1024, 'i')
	cases := []struct {
		name    string
		parsed  *ParsedEmail
		wantIDs []string
	}{
		{
			name: "attachments and inline images",
			parsed: &ParsedEmail{
				TextContent: "Hello",
				HTMLContent: `<p>Hello</p><img src="cid:a1"><img src="loc.png"><img src="` + dataURI("image/gif", img) + `"><div style="background-image:url(cid:bg)">x</div>`,
				Attachments: []*EmailAttachment{
					{Filename: "a1.png", ContentType: "image/png", Size: int64(len(img)), Data: bytes.Clone(img), ContentID: "a1"},
					{Filename: "loc.png", ContentType: "image/png", Size: int64(len(img)), Data: bytes.Clone(img), ContentLocation: "loc.png"},
					{Filename: "bg.png", ContentType: "image/png", Size: int64(len(img)), Data: bytes.Clone(img), ContentID: "bg"},
					{Filename: "notes.txt", ContentType: "text/plain", Size: 5, Data: []byte("notes")},
				},
			},
			wantIDs: []string{"inline-image-1", "inline-image-2", "inline-image-3", "inline-image-4", "att-4-notes.txt"},
		},
		{
			name:    "marketing HTML sent as a file",
			parsed:  &ParsedEmail{TextContent: "Sale", HTMLContent: marketingHTML()},
			wantIDs: []string{"html-attachment"},
		},
		{
			name:    "oversized text sent as a file",
			parsed:  &ParsedEmail{TextContent: strings.Repeat("A long line of plain text. ", 4000)},
			wantIDs: []string{"text-attachment"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := newFakeIntent(t)
			parts := convertForTest(t, tc.parsed, testRoom, intent, 0)
			if len(intent.uploads) != len(tc.wantIDs) {
				t.Errorf("%d uploads, want %d; parts: %v", len(intent.uploads), len(tc.wantIDs), partIDs(parts))
			}
			for _, u := range intent.uploads {
				if u.roomID != testRoom {
					t.Errorf("upload %q for room %q", u.name, u.roomID)
				}
			}
			for _, pid := range tc.wantIDs {
				p := partByID(parts, pid)
				if p == nil {
					t.Errorf("no part %q; parts: %v", pid, partIDs(parts))
					continue
				}
				if p.Content.File == nil || p.Content.File.URL == "" || p.Content.URL != "" {
					t.Errorf("part %q not encrypted: url %q file %+v", pid, p.Content.URL, p.Content.File)
				}
			}
			for _, p := range parts {
				if p.Content.URL != "" || strings.Contains(p.Content.FormattedBody, "mxc://") {
					t.Errorf("part %q references media in the clear", p.ID)
				}
			}
		})
	}
}

// Attachment bytes that are not in memory yet are fetched when the message
// is converted, once.
func TestConvertMessage_DeferredAttachmentFetchedWhenConverted(t *testing.T) {
	data := filler(4096, 'z')
	calls := 0
	att := &EmailAttachment{Filename: "report.pdf", ContentType: "application/pdf", Size: 4096,
		load: func(context.Context) ([]byte, error) { calls++; return bytes.Clone(data), nil }}
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{TextContent: "See attached", Attachments: []*EmailAttachment{att}}, testRoom, intent, 0)
	assertEncryptedMedia(t, parts, "att-1-report.pdf", event.MsgFile)
	if calls != 1 {
		t.Errorf("fetched %d times, want 1", calls)
	}
	if len(intent.uploads) != 1 || !bytes.Equal(intent.uploads[0].data, data) {
		t.Errorf("uploaded %d files; want the fetched bytes", len(intent.uploads))
	}
}

// An attachment over the upload cap is refused on its declared size, before
// any download.
func TestConvertMessage_OversizeAttachmentNotFetched(t *testing.T) {
	att := &EmailAttachment{Filename: "big.zip", ContentType: "application/zip", Size: 5000,
		load: func(context.Context) ([]byte, error) {
			t.Error("oversize attachment downloaded")
			return nil, nil
		}}
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{TextContent: "Big file", Attachments: []*EmailAttachment{att}}, testRoom, intent, 1000)
	p := partByID(parts, "att-1-big.zip")
	if p == nil || p.Content.MsgType != event.MsgNotice || !strings.HasPrefix(p.Content.Body, "📎 not bridged: big.zip (application/zip, 4.9 KiB)") {
		t.Fatalf("parts %v; attachment part %+v", partIDs(parts), p)
	}
	if !strings.Contains(p.Content.Body, "too large") {
		t.Errorf("notice gives no reason: %q", p.Content.Body)
	}
	if len(intent.uploads) != 0 {
		t.Errorf("uploads: %+v", intent.uploads)
	}
}

// A failed download costs that one attachment, not the message.
func TestConvertMessage_FailedFetchIsANotice(t *testing.T) {
	failing := &EmailAttachment{Filename: "report.pdf", ContentType: "application/pdf", Size: 10,
		load: func(context.Context) ([]byte, error) { return nil, errors.New("attachments.get: 503") }}
	ok := &EmailAttachment{Filename: "notes.txt", ContentType: "text/plain", Size: 5, Data: []byte("notes")}
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{TextContent: "Two files", Attachments: []*EmailAttachment{failing, ok}}, testRoom, intent, 0)
	p := partByID(parts, "att-1-report.pdf")
	if p == nil || p.Content.Body != "📎 not bridged: report.pdf (application/pdf, 10 B): download failed" {
		t.Fatalf("parts %v; failed part %+v", partIDs(parts), p)
	}
	assertEncryptedMedia(t, parts, "att-2-notes.txt", event.MsgFile)
	if partByID(parts, "body") == nil {
		t.Errorf("body dropped")
	}
}

// A small image that is content, not decoration: an alt text that happens to
// contain a decorative word does not hide it.
func TestConvertMessage_AltTextDoesNotMakeAnImageDecorative(t *testing.T) {
	shot := filler(40*1024, 's')
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "Where is it?",
		HTMLContent: `<p>Where is it?</p><img src="cid:s1" alt="Package tracking screenshot">`,
		Attachments: []*EmailAttachment{{Filename: "image.png", ContentType: "image/png", Size: int64(len(shot)), Data: shot, ContentID: "s1"}},
	}, testRoom, intent, 0)
	assertEncryptedMedia(t, parts, "inline-image-1", event.MsgImage)
}

// Alt text is read as HTML: an entity in it is decoded once, then escaped
// once in the formatted body. Only the src attribute names the image, not
// data-src.
func TestConvertMessage_InlineImageAltTextDecodedOnce(t *testing.T) {
	shot := filler(40*1024, 's')
	intent := newFakeIntent(t)
	parts := convertForTest(t, &ParsedEmail{
		TextContent: "Chart",
		HTMLContent: `<p>Chart</p><img alt="Q&amp;A chart" data-src="cid:other" src="cid:c1">`,
		Attachments: []*EmailAttachment{{Filename: "chart.png", ContentType: "image/png", Size: int64(len(shot)), Data: shot, ContentID: "c1"}},
	}, testRoom, intent, 0)
	c := assertEncryptedMedia(t, parts, "inline-image-1", event.MsgImage)
	if c.Body != "Image 1: Q&A chart" {
		t.Errorf("image body %q", c.Body)
	}
	if f := partByID(parts, "body").Content.FormattedBody; !strings.Contains(f, "[Image 1: Q&amp;A chart]") {
		t.Errorf("formatted body: %s", f)
	}
}

// A newsletter built from many embedded product images is still recognised
// as marketing and sent as an HTML file.
func TestConvertMessage_ImageHeavyNewsletterSentAsFile(t *testing.T) {
	var b strings.Builder
	var atts []*EmailAttachment
	b.WriteString("<html><body>")
	for i := 0; i < 14; i++ {
		cid := fmt.Sprintf("product%d", i)
		fmt.Fprintf(&b, `<div><img src="cid:%s" alt="Product %d"></div>`, cid, i)
		img := filler(20*1024, byte('a'+i))
		atts = append(atts, &EmailAttachment{Filename: cid + ".jpg", ContentType: "image/jpeg", Size: int64(len(img)), Data: img, ContentID: cid})
	}
	b.WriteString(strings.Repeat(`<div style="padding:4px"> </div>`, 600))
	b.WriteString("</body></html>")
	parts := convertForTest(t, &ParsedEmail{TextContent: "Shop now", HTMLContent: b.String(), Attachments: atts}, testRoom, newFakeIntent(t), 0)
	if partByID(parts, "html-attachment") == nil {
		t.Errorf("image-heavy newsletter not sent as a file; parts: %v", partIDs(parts))
	}
}
