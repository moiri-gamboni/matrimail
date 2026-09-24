package email

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// fakeGmail serves the three Gmail API calls the poller makes. Response bodies
// follow the documented REST schema (history.list, messages.list,
// messages.get); they are not captured from a live mailbox. In particular the
// messages inside history records carry only id and threadId, which is what
// the history.list reference says to expect, so nothing here may depend on a
// history record naming a message's labels.
type fakeGmail struct {
	history  map[string]string // labelId -> history.list body
	list     map[string]string // labelIds -> messages.list body
	messages map[string]string // message id -> messages.get body

	mu            sync.Mutex
	historyLabels []string
}

func (f *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := r.URL.Path
	var body string
	var ok bool
	switch {
	case path == "/gmail/v1/users/me/history":
		f.mu.Lock()
		f.historyLabels = append(f.historyLabels, q["labelId"]...)
		f.mu.Unlock()
		body, ok = f.history[q.Get("labelId")]
	case path == "/gmail/v1/users/me/messages":
		body, ok = f.list[strings.Join(q["labelIds"], ",")]
	case strings.HasPrefix(path, "/gmail/v1/users/me/messages/"):
		body, ok = f.messages[strings.TrimPrefix(path, "/gmail/v1/users/me/messages/")]
	}
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":{"code":404,"message":"no fixture for %s"}}`, r.URL), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

// gmailMessage renders a messages.get(format=full) body. internalDate and
// historyId are int64/uint64 fields, which the API encodes as JSON strings.
func gmailMessage(id string, internalDate int64, labels ...string) string {
	return fmt.Sprintf(`{"id":%q,"threadId":"thread-1","labelIds":[%s],"historyId":"1","internalDate":"%d",`+
		`"payload":{"mimeType":"text/plain","headers":[{"name":"Message-ID","value":"<%s@example.com>"}],"body":{"size":2,"data":"aGk="}}}`,
		id, quoteAll(labels), internalDate, id)
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ",")
}

type fed struct{ id, mailbox string }

func newTestPoller(t *testing.T, f *fakeGmail, labels ...string) (*GmailHistoryPoller, *[]fed) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	log := zerolog.Nop()
	var got []fed
	g := &GmailHistoryPoller{
		Email:             "me@example.com",
		MonitoredLabelIDs: labels,
		TokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		OnMessage: func(ctx context.Context, msg *gmail.Message, mailbox string) error {
			got = append(got, fed{msg.Id, mailbox})
			return nil
		},
		Log:           &log,
		clientOptions: []option.ClientOption{option.WithEndpoint(srv.URL + "/")},
	}
	return g, &got
}

// Watching Sent beside INBOX. history.list takes a single labelId, so each
// monitored label needs its own call; a message carrying both labels (mail to
// yourself) is reported under each and must be bridged once. Its mailbox is
// read from the fetched message, since history records need not carry labels,
// and SENT wins so the copy counts as the user's own. Messages are fed in
// history order, which the API documents as chronological.
func TestPollOnce_WatchesEveryLabelAndFeedsEachMessageOnce(t *testing.T) {
	t.Parallel()
	f := &fakeGmail{
		history: map[string]string{
			"INBOX": `{"historyId":"110","history":[
				{"id":"101","messagesAdded":[{"message":{"id":"m-in","threadId":"thread-1"}}]},
				{"id":"103","messagesAdded":[{"message":{"id":"m-self","threadId":"thread-1"}}]}]}`,
			"SENT": `{"historyId":"120","history":[
				{"id":"102","messagesAdded":[{"message":{"id":"m-out","threadId":"thread-1"}}]},
				{"id":"103","messagesAdded":[{"message":{"id":"m-self","threadId":"thread-1"}}]}]}`,
		},
		messages: map[string]string{
			"m-in":   gmailMessage("m-in", 1000, "INBOX", "UNREAD"),
			"m-out":  gmailMessage("m-out", 2000, "SENT"),
			"m-self": gmailMessage("m-self", 3000, "INBOX", "SENT"),
		},
	}
	g, got := newTestPoller(t, f, "INBOX", "SENT")
	log := zerolog.Nop()

	cursor, err := g.pollOnce(context.Background(), 100, &log)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if strings.Join(f.historyLabels, ",") != "INBOX,SENT" {
		t.Errorf("history.list was asked for labels %v; want one call per monitored label", f.historyLabels)
	}
	want := []fed{{"m-in", "INBOX"}, {"m-out", "SENT"}, {"m-self", "SENT"}}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Errorf("fed %v; want %v", *got, want)
	}
	// The labels are listed one after another, so the mailbox can move on in
	// between. Advancing to the older of the two cursors can re-read a message
	// (the bridge drops a repeat by its ID); the newer one could skip one.
	if cursor != 110 {
		t.Errorf("cursor = %d; want 110, the older of the per-label cursors", cursor)
	}
}

// A label added after delivery is picked up, but an unrelated label added to
// a message that already had a monitored one is not a new message.
func TestPollOnce_LabelAddedCountsOnlyForMonitoredLabels(t *testing.T) {
	t.Parallel()
	f := &fakeGmail{
		history: map[string]string{
			"INBOX": `{"historyId":"110","history":[
				{"id":"101","labelsAdded":[{"message":{"id":"m-moved","threadId":"t"},"labelIds":["INBOX"]}]},
				{"id":"102","labelsAdded":[{"message":{"id":"m-starred","threadId":"t"},"labelIds":["STARRED"]}]}]}`,
		},
		messages: map[string]string{
			"m-moved":   gmailMessage("m-moved", 1000, "INBOX"),
			"m-starred": gmailMessage("m-starred", 2000, "INBOX", "STARRED"),
		},
	}
	g, got := newTestPoller(t, f, "INBOX")
	log := zerolog.Nop()
	if _, err := g.pollOnce(context.Background(), 100, &log); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	want := []fed{{"m-moved", "INBOX"}}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Errorf("fed %v; want %v", *got, want)
	}
}

// backlogFixture lists six messages across INBOX and SENT, one of them (m4)
// under both. messages.list documents no ordering, so each label lists newest
// first, which is what Gmail's own UI shows.
func backlogFixture() *fakeGmail {
	list := func(ids ...string) string {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprintf(`{"id":%q,"threadId":"thread-1"}`, id)
		}
		return `{"messages":[` + strings.Join(parts, ",") + `],"resultSizeEstimate":` + fmt.Sprint(len(ids)) + `}`
	}
	return &fakeGmail{
		list: map[string]string{
			"INBOX": list("m6", "m5", "m4", "m2", "m1"),
			"SENT":  list("m4", "m3"),
		},
		messages: map[string]string{
			"m1": gmailMessage("m1", 1000, "INBOX"),
			"m2": gmailMessage("m2", 2000, "INBOX"),
			"m3": gmailMessage("m3", 3000, "SENT"),
			"m4": gmailMessage("m4", 4000, "INBOX", "SENT"),
			"m5": gmailMessage("m5", 5000, "INBOX"),
			"m6": gmailMessage("m6", 6000, "INBOX"),
		},
	}
}

// Backlog lists each label separately. A message under both is fed once, and
// as the user's own: the label that happened to list it first says nothing
// about who sent it.
func TestBacklog_FeedsEachMessageOnceUnderItsOwnLabels(t *testing.T) {
	t.Parallel()
	g, got := newTestPoller(t, backlogFixture(), "INBOX", "SENT")
	log := zerolog.Nop()

	n, err := g.Backlog(context.Background(), 7, &log)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	byID := map[string][]string{}
	for _, f := range *got {
		byID[f.id] = append(byID[f.id], f.mailbox)
	}
	want := map[string][]string{
		"m1": {"INBOX"}, "m2": {"INBOX"}, "m3": {"SENT"}, "m4": {"SENT"}, "m5": {"INBOX"}, "m6": {"INBOX"},
	}
	if fmt.Sprint(byID) != fmt.Sprint(want) {
		t.Errorf("fed %v; want %v", byID, want)
	}
	if n != 6 {
		t.Errorf("Backlog reported %d fed; want 6", n)
	}
}

// Whichever message Backlog feeds last becomes its room's newest event, so it
// must feed oldest first. Otherwise a thread's older message can land after
// its newer one and read as the latest word. messages.list documents no order,
// and the per-label lists interleave, so the order is Gmail's internalDate.
func TestBacklog_FeedsOldestFirst(t *testing.T) {
	t.Parallel()
	g, got := newTestPoller(t, backlogFixture(), "INBOX", "SENT")
	log := zerolog.Nop()

	if _, err := g.Backlog(context.Background(), 7, &log); err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	var order []string
	for _, f := range *got {
		order = append(order, f.id)
	}
	if strings.Join(order, ",") != "m1,m2,m3,m4,m5,m6" {
		t.Errorf("fed in order %v; want oldest first, m1 to m6", order)
	}
}
