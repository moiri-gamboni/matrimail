package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"maunium.net/go/mautrix/event"
)

// testdata/gmail_message_attachments.json is a users.messages.get
// (format=full) response written to the Gmail API reference's Message and
// MessagePart schemas, in the layout Gmail gives a message with a pasted
// screenshot (inline disposition, a file name and a Content-ID), an inline
// part with no file name, a tracking pixel, an inline part the HTML never
// uses, a PDF and a zip behind attachment IDs, and a CSV small enough to be
// carried inline in body.data.
func loadGmailFixture(t *testing.T) *gmail.Message {
	t.Helper()
	raw, err := os.ReadFile("testdata/gmail_message_attachments.json")
	if err != nil {
		t.Fatal(err)
	}
	var msg gmail.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	return &msg
}

// recordingFetch serves attachment bytes by attachment ID, as many as the
// part declares, and records each call.
type recordingFetch struct {
	mu    sync.Mutex
	sizes map[string]int
	fail  map[string]error
	calls []string
}

func (r *recordingFetch) fetch(_ context.Context, messageID, attachmentID string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, messageID+"/"+attachmentID)
	if err := r.fail[attachmentID]; err != nil {
		return nil, err
	}
	n, ok := r.sizes[attachmentID]
	if !ok {
		return nil, fmt.Errorf("unknown attachment %s", attachmentID)
	}
	return attachmentBytes(attachmentID, n), nil
}

func attachmentBytes(attachmentID string, n int) []byte {
	return bytes.Repeat([]byte(attachmentID[len(attachmentID)-1:]), n)
}

func fixtureFetch() *recordingFetch {
	return &recordingFetch{sizes: map[string]int{
		"ANGjdJ_inline": 20480, "ANGjdJ_noname": 40960, "ANGjdJ_pixel": 43,
		"ANGjdJ_unused": 30000, "ANGjdJ_pdf": 1234, "ANGjdJ_zip": 30000000,
	}}
}

// Parsing records what the message holds without downloading any of it.
func TestParseGmailAPIMessage_RecordsAttachmentsWithoutDownloading(t *testing.T) {
	f := fixtureFetch()
	parsed, err := ParseGmailAPIMessage(loadGmailFixture(t), f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("parse downloaded %v", f.calls)
	}
	var got []string
	for _, a := range parsed.Attachments {
		got = append(got, fmt.Sprintf("%s|%s|%s|%d", a.Filename, a.ContentType, a.ContentID, a.Size))
	}
	want := []string{
		"image.png|image/png|ii_m1abc|20480",
		"|image/jpeg|ii_noname|40960",
		"report.pdf|application/pdf||1234",
		"small.csv|text/csv||8",
		"huge.zip|application/zip||30000000",
	}
	// The tracking pixel and the inline part no HTML uses are not kept.
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("attachments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range parsed.Attachments {
		if a.Filename == "small.csv" && string(a.Data) != "a,b\n1,2\n" {
			t.Errorf("inline body data not decoded: %q", a.Data)
		}
	}
}

// End to end: the bridged message carries every attachment's real bytes,
// encrypted; downloads happen only for what is sent, and the oversize zip is
// refused before its download.
func TestGmailMessage_AttachmentsDownloadedAndEncrypted(t *testing.T) {
	f := fixtureFetch()
	parsed, err := ParseGmailAPIMessage(loadGmailFixture(t), f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	intent := newFakeIntent(t)
	parts := convertForTest(t, parsed, testRoom, intent, 25*1024*1024)

	wantCalls := []string{"18c0e0a1b2c3d4e5/ANGjdJ_inline", "18c0e0a1b2c3d4e5/ANGjdJ_noname", "18c0e0a1b2c3d4e5/ANGjdJ_pdf"}
	if strings.Join(f.calls, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("downloads %v, want %v", f.calls, wantCalls)
	}

	uploaded := func(data []byte) bool {
		for _, u := range intent.uploads {
			if bytes.Equal(u.data, data) {
				return true
			}
		}
		return false
	}
	assertEncryptedMedia(t, parts, "inline-image-1", event.MsgImage)
	assertEncryptedMedia(t, parts, "inline-image-2", event.MsgImage)
	assertEncryptedMedia(t, parts, "att-3-report.pdf", event.MsgFile)
	assertEncryptedMedia(t, parts, "att-4-small.csv", event.MsgFile)
	for _, want := range [][]byte{
		attachmentBytes("ANGjdJ_inline", 20480), attachmentBytes("ANGjdJ_noname", 40960),
		attachmentBytes("ANGjdJ_pdf", 1234), []byte("a,b\n1,2\n"),
	} {
		if !uploaded(want) {
			t.Errorf("%d-byte attachment not uploaded with its bytes", len(want))
		}
	}
	for _, u := range intent.uploads {
		if len(u.data) == 0 {
			t.Errorf("empty upload %q", u.name)
		}
	}
	zip := partByID(parts, "att-5-huge.zip")
	if zip == nil || zip.Content.MsgType != event.MsgNotice || !strings.HasPrefix(zip.Content.Body, "📎 not bridged: huge.zip (application/zip, 28.6 MiB): too large") {
		t.Errorf("zip part: %+v", zip)
	}
	if f := partByID(parts, "body").Content.FormattedBody; strings.Contains(f, "cid:") || strings.Contains(f, "<img") {
		t.Errorf("formatted body keeps image references: %s", f)
	}
}

// A download that fails costs that attachment, not the message.
func TestGmailMessage_FailedDownloadIsANotice(t *testing.T) {
	f := fixtureFetch()
	f.fail = map[string]error{"ANGjdJ_pdf": errors.New("attachments.get: googleapi: Error 503: Backend Error")}
	parsed, err := ParseGmailAPIMessage(loadGmailFixture(t), f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	parts := convertForTest(t, parsed, testRoom, newFakeIntent(t), 25*1024*1024)
	p := partByID(parts, "att-3-report.pdf")
	if p == nil || p.Content.MsgType != event.MsgNotice || !strings.HasPrefix(p.Content.Body, "📎 not bridged: report.pdf (application/pdf, 1.2 KiB): download failed") {
		t.Errorf("report part: %+v", p)
	}
	assertEncryptedMedia(t, parts, "inline-image-1", event.MsgImage)
	if partByID(parts, "body") == nil {
		t.Errorf("body dropped; parts: %v", partIDs(parts))
	}
}

// fakeAttachmentsAPI serves users.messages.attachments.get with a
// MessagePartBody as the reference gives it: attachmentId, size and the
// bytes as base64url in data.
type fakeAttachmentsAPI struct {
	bodies map[string]string // "<messageId>/<attachmentId>" -> response body
	paths  []string
}

func (f *fakeAttachmentsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	key, _ := strings.CutPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
	key = strings.Replace(key, "/attachments/", "/", 1)
	w.Header().Set("Content-Type", "application/json")
	if body, ok := f.bodies[key]; ok {
		fmt.Fprint(w, body)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`)
}

func TestGmailAttachments_Fetch(t *testing.T) {
	api := &fakeAttachmentsAPI{bodies: map[string]string{
		"m1/a1": `{"attachmentId":"a1","size":5,"data":"aGVsbG8"}`,
		"m1/a2": `{"attachmentId":"a2","size":5,"data":"aGVsbG8="}`,
		"m1/a3": `{"attachmentId":"a3","size":2,"data":"-_8"}`,
	}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	log := zerolog.Nop()
	g := NewGmailAttachments(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), &log, option.WithEndpoint(srv.URL+"/"))

	// Unpadded, padded, and with the URL-safe alphabet.
	for _, c := range []struct{ id, want string }{{"a1", "hello"}, {"a2", "hello"}, {"a3", "\xfb\xff"}} {
		got, err := g.Fetch(context.Background(), "m1", c.id)
		if err != nil {
			t.Errorf("%s: %v", c.id, err)
		} else if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.id, got, c.want)
		}
	}
	if len(api.paths) == 0 || api.paths[0] != "GET /gmail/v1/users/me/messages/m1/attachments/a1" {
		t.Errorf("requests %q", api.paths)
	}
	if _, err := g.Fetch(context.Background(), "m1", "missing"); !IsGmailNotFound(err) {
		t.Errorf("missing attachment: err %v, want the API's 404", err)
	}
}
