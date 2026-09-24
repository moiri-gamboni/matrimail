package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// fakeGmailThreads serves users.threads.get and records the modify calls.
// Bodies follow the Gmail API reference: threads.get with format=minimal
// returns each message's id and labelIds; a modify body is
// {"addLabelIds":[...],"removeLabelIds":[...]}. Messages are listed oldest
// first, the order Gmail returns them in; the reference does not state one.
type fakeGmailThreads struct {
	threads map[string]string // thread id -> threads.get body

	mu      sync.Mutex
	gets    []string // format of each threads.get
	modifes []string // "<path> <body>" of each modify call
}

func (f *fakeGmailThreads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(path, "/modify") && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		norm, _ := json.Marshal(req)
		f.mu.Lock()
		f.modifes = append(f.modifes, path+" "+string(norm))
		f.mu.Unlock()
		fmt.Fprint(w, `{"id":"x"}`)
		return
	}
	if id, ok := strings.CutPrefix(path, "threads/"); ok && r.Method == http.MethodGet {
		f.mu.Lock()
		f.gets = append(f.gets, r.URL.Query().Get("format"))
		f.mu.Unlock()
		if body, ok := f.threads[id]; ok {
			fmt.Fprint(w, body)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`)
}

func newTestThreads(t *testing.T, f *fakeGmailThreads) *GmailThreads {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	log := zerolog.Nop()
	return NewGmailThreads(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), &log,
		option.WithEndpoint(srv.URL+"/"))
}

// minimalThread renders a threads.get(format=minimal) body; each message is
// "id:LABEL,LABEL".
func minimalThread(id string, msgs ...string) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		mid, labels, _ := strings.Cut(m, ":")
		parts[i] = fmt.Sprintf(`{"id":%q,"threadId":%q,"labelIds":[%s],"historyId":"5"}`, mid, id, quoteAll(strings.Split(labels, ",")))
	}
	return fmt.Sprintf(`{"id":%q,"historyId":"5","messages":[%s]}`, id, strings.Join(parts, ","))
}

// A thread is in the inbox while any of its messages carries INBOX, which is
// how Gmail lists it, and unread while any carries UNREAD.
func TestGmailThreadsGet_StateFromMessageLabels(t *testing.T) {
	t.Parallel()
	f := &fakeGmailThreads{threads: map[string]string{
		"t-inbox-read":   minimalThread("t-inbox-read", "m1:INBOX", "m2:SENT"),
		"t-archived":     minimalThread("t-archived", "m1:CATEGORY_UPDATES", "m2:SENT,UNREAD"),
		"t-inbox-unread": minimalThread("t-inbox-unread", "m1:SENT", "m2:INBOX,UNREAD,IMPORTANT"),
	}}
	g := newTestThreads(t, f)
	for id, want := range map[string]ThreadState{
		"t-inbox-read":   {Archived: false, Unread: false},
		"t-archived":     {Archived: true, Unread: true},
		"t-inbox-unread": {Archived: false, Unread: true},
	} {
		th, err := g.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if th.State != want {
			t.Errorf("Get(%s).State = %+v; want %+v", id, th.State, want)
		}
	}
	if strings.Join(f.gets, ",") != "minimal,minimal,minimal" {
		t.Errorf("threads.get formats %v; want minimal, labels are all the state needs", f.gets)
	}
}

// A thread Gmail no longer has is reported as such, so the caller can tell it
// from a failed call.
func TestGmailThreadsGet_MissingThreadIsNotFound(t *testing.T) {
	t.Parallel()
	g := newTestThreads(t, &fakeGmailThreads{})
	_, err := g.Get(context.Background(), "t-gone")
	if err == nil || !IsGmailNotFound(err) {
		t.Errorf("Get of a missing thread returned %v; want a not-found error", err)
	}
}

// Archive and read act on the whole thread, the way Gmail's own buttons do.
// Unread marks only the newest message, as Gmail's "mark as unread" does, so
// the thread shows unread without every old message turning bold; a draft is
// not a message anyone reads, so it is passed over.
func TestGmailThreadsApply_ChangesOnlyWhatDiffers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		thread string
		target ThreadState
		want   string
	}{
		{"archive", minimalThread("t", "m1:INBOX"), ThreadState{Archived: true}, `threads/t/modify {"removeLabelIds":["INBOX"]}`},
		{"unarchive", minimalThread("t", "m1:SENT"), ThreadState{Archived: false}, `threads/t/modify {"addLabelIds":["INBOX"]}`},
		{"read", minimalThread("t", "m1:INBOX,UNREAD", "m2:INBOX,UNREAD"), ThreadState{Unread: false}, `threads/t/modify {"removeLabelIds":["UNREAD"]}`},
		{"archive and read", minimalThread("t", "m1:INBOX,UNREAD"), ThreadState{Archived: true, Unread: false}, `threads/t/modify {"removeLabelIds":["INBOX","UNREAD"]}`},
		{"unread", minimalThread("t", "m1:INBOX", "m2:INBOX", "m3:DRAFT"), ThreadState{Unread: true}, `messages/m2/modify {"addLabelIds":["UNREAD"]}`},
		{"unarchive and unread", minimalThread("t", "m1:SENT"), ThreadState{Archived: false, Unread: true},
			`threads/t/modify {"addLabelIds":["INBOX"]}|messages/m1/modify {"addLabelIds":["UNREAD"]}`},
		{"nothing to do", minimalThread("t", "m1:INBOX,UNREAD"), ThreadState{Unread: true}, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeGmailThreads{threads: map[string]string{"t": tc.thread}}
			g := newTestThreads(t, f)
			th, err := g.Get(context.Background(), "t")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if err := g.Apply(context.Background(), th, tc.target); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := strings.Join(f.modifes, "|"); got != tc.want {
				t.Errorf("modify calls %q; want %q", got, tc.want)
			}
		})
	}
}
