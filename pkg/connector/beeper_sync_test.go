package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/Leicas/matrimail/pkg/beeper"
	"github.com/Leicas/matrimail/pkg/email"
)

// The sync is tested against stateful fakes of both APIs: a write changes
// what the next read returns, as on the real services. The fakes speak the
// documented shapes: Beeper's chat object and routes from its OpenAPI document
// and live captures (pkg/beeper/testdata), Gmail's threads.get(format=minimal)
// and the threads/messages modify bodies from the Gmail API reference.

const testLoginID = "email:me@example.com"

// fakeBeeperChat is one chat's state on the fake Beeper Server.
type fakeBeeperChat struct {
	archived     bool
	unreadCount  int
	markedUnread bool
}

type fakeBeeper struct {
	mu    sync.Mutex
	chats map[string]*fakeBeeperChat // room ID -> state
	order []string                   // room IDs in list order
	// down makes every call answer 502, as Beeper Server does when it cannot
	// reach its backend.
	down bool
	// ignoreWrites accepts writes without applying them.
	ignoreWrites bool
	writes       []string
}

func newFakeBeeper() *fakeBeeper {
	return &fakeBeeper{chats: map[string]*fakeBeeperChat{}}
}

func (f *fakeBeeper) add(room string, c fakeBeeperChat) {
	f.chats[room] = &c
	f.order = append(f.order, room)
}

func (f *fakeBeeper) chatJSON(room string) map[string]any {
	c := f.chats[room]
	return map[string]any{
		"id": room, "accountID": "sh-email_" + testLoginID, "type": "group",
		"isArchived": c.archived, "unreadCount": c.unreadCount, "isMarkedUnread": c.markedUnread,
	}
}

func (f *fakeBeeper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.down {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Bad gateway","code":"BAD_GATEWAY"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v1/accounts":
		fmt.Fprintf(w, `[{"accountID":"matrix","loginID":"@me:beeper.example"},{"accountID":"sh-email_%s","loginID":%q}]`, testLoginID, testLoginID)
		return
	case r.Method == http.MethodGet && path == "/v1/chats":
		if r.URL.Query().Get("accountIDs") != "sh-email_"+testLoginID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		items := []map[string]any{}
		for _, room := range f.order {
			items = append(items, f.chatJSON(room))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "hasMore": false, "oldestCursor": nil, "newestCursor": nil})
		return
	}
	rest, _ := strings.CutPrefix(path, "/v1/chats/")
	room, action, _ := strings.Cut(rest, "/")
	c, ok := f.chats[room]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"Chat not found: %s","code":"NOT_FOUND"}`, room)
		return
	}
	before := f.chatJSON(room)
	switch {
	case r.Method == http.MethodGet && action == "":
		_ = json.NewEncoder(w).Encode(before)
		return
	case r.Method == http.MethodPatch && action == "":
		// Observed on Beeper Server 4.3.123: PATCH answers 200 but a bridged
		// chat's updateThread has no isArchived branch, so nothing changes.
		f.writes = append(f.writes, room+" patch (ignored)")
	case r.Method == http.MethodPost && action == "archive":
		req := struct {
			Archived *bool `json:"archived"`
		}{}
		_ = json.Unmarshal(body, &req)
		archived := req.Archived == nil || *req.Archived // the API defaults to true
		f.writes = append(f.writes, fmt.Sprintf("%s archived=%v", room, archived))
		if !f.ignoreWrites {
			c.archived = archived
		}
	case r.Method == http.MethodPost && action == "read":
		f.writes = append(f.writes, room+" read")
		if !f.ignoreWrites {
			c.unreadCount, c.markedUnread = 0, false
		}
	case r.Method == http.MethodPost && action == "unread":
		f.writes = append(f.writes, room+" unread")
		if !f.ignoreWrites {
			c.markedUnread = true
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Observed on Beeper Server: a write answers with the chat as it was
	// before the write.
	_ = json.NewEncoder(w).Encode(before)
}

func (f *fakeBeeper) setDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = v
}

func (f *fakeBeeper) setIgnoreWrites(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ignoreWrites = v
}

func (f *fakeBeeper) setUnreadCount(room string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats[room].unreadCount = n
}

func (f *fakeBeeper) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakeBeeper) state(room string) email.ThreadState {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.chats[room]
	return email.ThreadState{Archived: c.archived, Unread: c.unreadCount > 0 || c.markedUnread}
}

type fakeGmailMessage struct {
	id     string
	labels []string
}

// fakeGmailThreads holds each thread's messages and applies modify calls to
// them. threads.get lists them oldest first, the order Gmail returns them in
// (observed behaviour; the reference does not state an order).
type fakeGmailThreads struct {
	mu      sync.Mutex
	threads map[string][]*fakeGmailMessage
	gets    []string
	writes  []string
	// onGet runs, unlocked, after each threads.get has been answered.
	onGet func(threadID string)
}

func (f *fakeGmailThreads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`)
	}
	if r.Method == http.MethodGet {
		tid, _ := strings.CutPrefix(path, "threads/")
		msgs, ok := f.threads[tid]
		if !ok {
			notFound()
			return
		}
		f.gets = append(f.gets, tid)
		var out []map[string]any
		for _, m := range msgs {
			out = append(out, map[string]any{"id": m.id, "threadId": tid, "labelIds": m.labels})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": tid, "historyId": "7", "messages": out})
		if f.onGet != nil {
			f.mu.Unlock()
			f.onGet(tid)
			f.mu.Lock()
		}
		return
	}
	var req struct{ AddLabelIds, RemoveLabelIds []string }
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	target := strings.TrimSuffix(path, "/modify")
	f.writes = append(f.writes, fmt.Sprintf("%s +%v -%v", target, req.AddLabelIds, req.RemoveLabelIds))
	var msgs []*fakeGmailMessage
	if tid, ok := strings.CutPrefix(target, "threads/"); ok {
		msgs = f.threads[tid]
	} else {
		mid, _ := strings.CutPrefix(target, "messages/")
		for _, th := range f.threads {
			for _, m := range th {
				if m.id == mid {
					msgs = append(msgs, m)
				}
			}
		}
	}
	if len(msgs) == 0 {
		notFound()
		return
	}
	for _, m := range msgs {
		m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return slices.Contains(req.RemoveLabelIds, l) })
		for _, l := range req.AddLabelIds {
			if !slices.Contains(m.labels, l) {
				m.labels = append(m.labels, l)
			}
		}
	}
	fmt.Fprint(w, `{"id":"x"}`)
}

func (f *fakeGmailThreads) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakeGmailThreads) takeGets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.gets
	f.gets = nil
	return g
}

func (f *fakeGmailThreads) setLabels(tid, mid string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.threads[tid] {
		if m.id == mid {
			m.labels = labels
			return
		}
	}
	f.threads[tid] = append(f.threads[tid], &fakeGmailMessage{id: mid, labels: labels})
}

// newTestDB opens a private in-memory database with the bridgev2 schema and
// this connector's portal metadata type.
func newTestDB(t *testing.T) *database.Database {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	rawDB, err := dbutil.NewWithDialect(fmt.Sprintf("file:%s?mode=memory&cache=shared&_foreign_keys=on", name), "sqlite3")
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	db := database.New(testBridgeID, database.MetaTypes{Portal: func() any { return &PortalMetadata{} }}, rawDB)
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("apply bridgev2 schema: %v", err)
	}
	return db
}

type syncFixture struct {
	beeper *fakeBeeper
	gmail  *fakeGmailThreads
	store  *BeeperSyncQuery
	syncer *beeperSyncer
	logs   *bytes.Buffer
}

// newSyncFixture wires a syncer to fresh fakes. rooms maps each Beeper room
// to the Gmail thread its portal holds.
func newSyncFixture(t *testing.T, rooms map[string]string) *syncFixture {
	t.Helper()
	fb := newFakeBeeper()
	fg := &fakeGmailThreads{threads: map[string][]*fakeGmailMessage{}}
	bsrv := httptest.NewServer(fb)
	t.Cleanup(bsrv.Close)
	gsrv := httptest.NewServer(fg)
	t.Cleanup(gsrv.Close)

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	log := zerolog.New(logs).Level(zerolog.InfoLevel)

	store := &BeeperSyncQuery{DB: newTestDB(t)}
	if err := store.CreateTable(context.Background()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	threads := email.NewGmailThreads(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), &log,
		option.WithEndpoint(gsrv.URL+"/"))
	s := &beeperSyncer{
		loginID: testLoginID,
		beeper:  &beeper.Client{BaseURL: bsrv.URL, TokenFile: tokenFile, HTTP: bsrv.Client(), Log: &log},
		gmail:   threads,
		store:   store,
		threadForChat: func(ctx context.Context, chatID string) (string, error) {
			return rooms[chatID], nil
		},
		log: &log,
	}
	return &syncFixture{beeper: fb, gmail: fg, store: store, syncer: s, logs: logs}
}

// tryTick lists and syncs every chat once, as runTick does, returning the
// failure instead of logging it.
func (f *syncFixture) tryTick() error {
	ctx := context.Background()
	chats, err := f.syncer.listChats(ctx)
	if err != nil {
		return err
	}
	return f.syncer.syncChats(ctx, chats)
}

func (f *syncFixture) tick(t *testing.T) {
	t.Helper()
	if err := f.tryTick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (f *syncFixture) synced(t *testing.T, thread string) *email.ThreadState {
	t.Helper()
	row, err := f.store.Get(context.Background(), testLoginID, thread)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if row == nil {
		return nil
	}
	return &row.State
}

func (f *syncFixture) seed(t *testing.T, thread string, s email.ThreadState) {
	t.Helper()
	if err := f.store.Put(context.Background(), testLoginID, thread, s); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

// A thread seen for the first time takes Gmail's state: a mailbox import
// creates its chats unread and in the inbox, whatever the mail's state in
// Gmail. Once the two sides agree, a tick with nothing changed calls neither
// API beyond the chat list.
func TestBeeperSync_FirstSyncPushesGmailStateToBeeper(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!old:beeper.local": "t-old", "!new:beeper.local": "t-new"})
	f.beeper.add("!old:beeper.local", fakeBeeperChat{unreadCount: 3})
	f.beeper.add("!new:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.gmail.setLabels("t-old", "m1", "CATEGORY_UPDATES")
	f.gmail.setLabels("t-new", "m2", "INBOX", "UNREAD")

	f.tick(t)

	eq(t, "Beeper writes", f.beeper.takeWrites(), []string{"!old:beeper.local archived=true", "!old:beeper.local read"})
	eq(t, "Gmail writes", f.gmail.takeWrites(), nil)
	if got := f.synced(t, "t-old"); got == nil || *got != (email.ThreadState{Archived: true}) {
		t.Errorf("synced state of t-old = %v; want archived and read", got)
	}
	if got := f.synced(t, "t-new"); got == nil || *got != (email.ThreadState{Unread: true}) {
		t.Errorf("synced state of t-new = %v; want inbox and unread", got)
	}

	f.gmail.takeGets()
	f.tick(t)
	eq(t, "Beeper writes on a quiet tick", f.beeper.takeWrites(), nil)
	eq(t, "Gmail reads on a quiet tick", f.gmail.takeGets(), nil)
}

// Each change made in Beeper is made to the Gmail thread, and the change the
// bridge made in Gmail does not come back: when the poller then reports the
// thread as changed, both sides already match what was stored.
func TestBeeperSync_BeeperChangeAppliedToGmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		synced     email.ThreadState
		chat       fakeBeeperChat
		labels     [][]string // per message, oldest first
		wantGmail  []string
		wantSynced email.ThreadState
	}{
		{"archive", email.ThreadState{}, fakeBeeperChat{archived: true}, [][]string{{"INBOX"}},
			[]string{"threads/t +[] -[INBOX]"}, email.ThreadState{Archived: true}},
		{"unarchive", email.ThreadState{Archived: true}, fakeBeeperChat{}, [][]string{{"SENT"}},
			[]string{"threads/t +[INBOX] -[]"}, email.ThreadState{}},
		{"read", email.ThreadState{Unread: true}, fakeBeeperChat{}, [][]string{{"INBOX", "UNREAD"}, {"INBOX", "UNREAD"}},
			[]string{"threads/t +[] -[UNREAD]"}, email.ThreadState{}},
		{"mark unread", email.ThreadState{}, fakeBeeperChat{markedUnread: true}, [][]string{{"INBOX"}, {"INBOX"}},
			[]string{"messages/m1 +[UNREAD] -[]"}, email.ThreadState{Unread: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
			f.beeper.add("!r:beeper.local", tc.chat)
			for i, l := range tc.labels {
				f.gmail.setLabels("t", fmt.Sprintf("m%d", i), l...)
			}
			f.seed(t, "t", tc.synced)

			f.tick(t)

			eq(t, "Gmail writes", f.gmail.takeWrites(), tc.wantGmail)
			eq(t, "Beeper writes", f.beeper.takeWrites(), nil)
			if got := f.synced(t, "t"); got == nil || *got != tc.wantSynced {
				t.Errorf("synced state = %v; want %+v", got, tc.wantSynced)
			}

			// The history poller sees the bridge's own label change.
			if err := f.syncer.gmailThreadsChanged(context.Background(), []string{"t"}); err != nil {
				t.Fatalf("gmailThreadsChanged: %v", err)
			}
			f.tick(t)
			eq(t, "Gmail writes after the echo", f.gmail.takeWrites(), nil)
			eq(t, "Beeper writes after the echo", f.beeper.takeWrites(), nil)
		})
	}
}

// A change made in Gmail, once the poller reports the thread, is made to the
// chat, and does not come back to Gmail.
func TestBeeperSync_GmailChangeAppliedToBeeper(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.gmail.setLabels("t", "m1", "INBOX", "UNREAD")
	f.tick(t)
	f.beeper.takeWrites()

	f.gmail.setLabels("t", "m1", "IMPORTANT") // archived and read in Gmail
	if err := f.syncer.gmailThreadsChanged(context.Background(), []string{"t"}); err != nil {
		t.Fatalf("gmailThreadsChanged: %v", err)
	}
	f.tick(t)

	eq(t, "Beeper writes", f.beeper.takeWrites(), []string{"!r:beeper.local archived=true", "!r:beeper.local read"})
	eq(t, "Gmail writes", f.gmail.takeWrites(), nil)
	if got := f.beeper.state("!r:beeper.local"); got != (email.ThreadState{Archived: true}) {
		t.Errorf("chat state = %+v; want archived and read", got)
	}

	f.tick(t)
	eq(t, "Beeper writes on the next tick", f.beeper.takeWrites(), nil)
	eq(t, "Gmail writes on the next tick", f.gmail.takeWrites(), nil)
}

// A change the poller has not reported yet is still seen when Beeper changed
// the same thread: the thread is read from Gmail before anything is written
// to it, and each side's change is kept.
func TestBeeperSync_ChangesOnBothSidesAreBothKept(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{archived: true, unreadCount: 1}) // archived in Beeper
	f.gmail.setLabels("t", "m1", "INBOX")                                           // read in Gmail
	f.seed(t, "t", email.ThreadState{Unread: true})

	f.tick(t)

	eq(t, "Gmail writes", f.gmail.takeWrites(), []string{"threads/t +[] -[INBOX]"})
	eq(t, "Beeper writes", f.beeper.takeWrites(), []string{"!r:beeper.local read"})
	if got := f.synced(t, "t"); got == nil || *got != (email.ThreadState{Archived: true}) {
		t.Errorf("synced state = %v; want archived and read", got)
	}
}

// New mail in an archived thread puts INBOX back on it in Gmail; the chat
// comes back out of the archive, and stays unread.
func TestBeeperSync_NewMailUnarchivesTheChat(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{archived: true})
	f.gmail.setLabels("t", "m1", "SENT")
	f.tick(t)
	f.beeper.takeWrites()

	f.gmail.setLabels("t", "m2", "INBOX", "UNREAD")
	f.beeper.setUnreadCount("!r:beeper.local", 1) // the bridge posted it
	if err := f.syncer.gmailThreadsChanged(context.Background(), []string{"t"}); err != nil {
		t.Fatalf("gmailThreadsChanged: %v", err)
	}
	f.tick(t)

	eq(t, "Beeper writes", f.beeper.takeWrites(), []string{"!r:beeper.local archived=false"})
	eq(t, "Gmail writes", f.gmail.takeWrites(), nil)
	if got := f.beeper.state("!r:beeper.local"); got != (email.ThreadState{Unread: true}) {
		t.Errorf("chat state = %+v; want in the inbox and unread", got)
	}
}

// A write Beeper accepted but did not apply is not recorded as synced, so the
// next tick tries again rather than reading the unchanged chat as a change
// made in Beeper and undoing Gmail's state.
func TestBeeperSync_UnconfirmedBeeperWriteIsRetried(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.gmail.setLabels("t", "m1", "IMPORTANT")
	f.beeper.setIgnoreWrites(true)

	if err := f.tryTick(); err == nil {
		t.Errorf("tick reported success for a write Beeper did not apply")
	}
	if got := f.synced(t, "t"); got != nil {
		t.Errorf("synced state = %+v; want none recorded", *got)
	}

	f.beeper.setIgnoreWrites(false)
	f.beeper.takeWrites()
	f.tick(t)
	eq(t, "Beeper writes on retry", f.beeper.takeWrites(), []string{"!r:beeper.local archived=true", "!r:beeper.local read"})
	eq(t, "Gmail writes", f.gmail.takeWrites(), nil)
}

// Chats that are not a Gmail thread of this login -- a compose draft not yet
// sent, a room of another bridge login -- are left alone, as is a thread Gmail
// no longer has; neither stops the other chats from syncing.
func TestBeeperSync_SkipsChatsWithoutAGmailThread(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!gone:beeper.local": "t-gone", "!r:beeper.local": "t"})
	f.beeper.add("!draft:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.beeper.add("!gone:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.gmail.setLabels("t", "m1", "INBOX")

	f.tick(t)

	eq(t, "Beeper writes", f.beeper.takeWrites(), []string{"!r:beeper.local read"})
}

// The poller reports every thread whose labels moved, most of them never
// bridged; only threads already synced are marked.
func TestBeeperSync_GmailChangesToUnsyncedThreadsAreIgnored(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, nil)
	f.seed(t, "t-synced", email.ThreadState{})
	if err := f.syncer.gmailThreadsChanged(context.Background(), []string{"t-other", "t-synced"}); err != nil {
		t.Fatalf("gmailThreadsChanged: %v", err)
	}
	if got := f.synced(t, "t-other"); got != nil {
		t.Errorf("an unsynced thread got a row: %+v", *got)
	}
	row, err := f.store.Get(context.Background(), testLoginID, "t-synced")
	if err != nil || row == nil || !row.GmailChanged {
		t.Errorf("synced thread row = %+v, %v; want it marked changed in Gmail", row, err)
	}
}

// Beeper Server being unreachable is logged once when it starts and once when
// it ends, not on every interval, and nothing is written to Gmail meanwhile.
func TestBeeperSync_OutageIsLoggedOncePerEpisode(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{archived: true})
	f.gmail.setLabels("t", "m1", "INBOX")
	f.seed(t, "t", email.ThreadState{})
	f.beeper.setDown(true)

	ctx := context.Background()
	f.syncer.runTick(ctx)
	f.syncer.runTick(ctx)
	f.syncer.runTick(ctx)
	eq(t, "Gmail writes during the outage", f.gmail.takeWrites(), nil)

	f.beeper.setDown(false)
	f.syncer.runTick(ctx)
	f.syncer.runTick(ctx)

	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry struct{ Level string }
		_ = json.Unmarshal([]byte(line), &entry)
		levels = append(levels, entry.Level)
	}
	eq(t, "log levels", levels, []string{"warn", "info"})
	if !strings.Contains(f.logs.String(), "502") {
		t.Errorf("the outage log does not carry the API's answer: %s", f.logs.String())
	}
}

// A chat is traced to its Gmail thread through the portal the bridge created
// for its room, and only for the login that owns the portal.
func TestGmailThreadForChat(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()
	insert := func(room, receiver, gmailThread string) {
		t.Helper()
		if err := db.Portal.Insert(ctx, &database.Portal{
			BridgeID:  testBridgeID,
			PortalKey: networkid.PortalKey{ID: networkid.PortalID("thread:" + room), Receiver: networkid.UserLoginID(receiver)},
			MXID:      id.RoomID(room),
			Metadata:  &PortalMetadata{ThreadID: room, GmailThreadID: gmailThread},
		}); err != nil {
			t.Fatalf("insert portal: %v", err)
		}
	}
	insert("!mine:example.com", testLoginID, "t-mine")
	insert("!theirs:example.com", "email:other@example.com", "t-theirs")
	insert("!draft:example.com", testLoginID, "")

	resolve := gmailThreadForChat(db, testLoginID)
	for room, want := range map[string]string{
		"!mine:example.com":    "t-mine",
		"!theirs:example.com":  "",
		"!draft:example.com":   "",
		"!unknown:example.com": "",
	} {
		got, err := resolve(ctx, room)
		if err != nil || got != want {
			t.Errorf("gmailThreadForChat(%s) = %q, %v; want %q", room, got, err, want)
		}
	}
}

// A chat whose thread Gmail no longer has is recorded as it stands, so it is
// not fetched from Gmail again on every tick; only a change to it in Beeper
// brings it back.
func TestBeeperSync_DeletedThreadIsNotFetchedEveryTick(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!gone:beeper.local": "t-gone"})
	f.beeper.add("!gone:beeper.local", fakeBeeperChat{unreadCount: 1})

	f.tick(t)
	f.tick(t)

	eq(t, "Beeper writes", f.beeper.takeWrites(), nil)
	eq(t, "Gmail writes", f.gmail.takeWrites(), nil)
	if got := f.synced(t, "t-gone"); got == nil || *got != (email.ThreadState{Unread: true}) {
		t.Errorf("synced state = %v; want the chat's own state recorded", got)
	}
}

// A chat that keeps failing is reported once, and does not mask an outage of
// Beeper Server that starts while it is failing.
func TestBeeperSync_ChatFailureDoesNotMaskAnOutage(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{unreadCount: 1})
	f.gmail.setLabels("t", "m1", "IMPORTANT")
	f.beeper.setIgnoreWrites(true) // the read-back never matches

	ctx := context.Background()
	f.syncer.runTick(ctx)
	f.syncer.runTick(ctx)
	f.beeper.setDown(true)
	f.syncer.runTick(ctx)
	f.syncer.runTick(ctx)

	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry struct{ Level, Message string }
		_ = json.Unmarshal([]byte(line), &entry)
		msgs = append(msgs, entry.Level+": "+entry.Message)
	}
	if len(msgs) != 2 || !strings.HasPrefix(msgs[0], "warn") || !strings.HasPrefix(msgs[1], "warn") || msgs[0] == msgs[1] {
		t.Errorf("logs %q; want one warning for the failing chat, then one for the outage", msgs)
	}
}

// Gmail reporting a thread changed while that thread is being synced must not
// be lost: the report waits for the sync, and the thread is read from Gmail
// again on the next tick.
func TestBeeperSync_GmailChangeDuringSyncIsKept(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t, map[string]string{"!r:beeper.local": "t"})
	f.beeper.add("!r:beeper.local", fakeBeeperChat{archived: true})
	f.gmail.setLabels("t", "m1", "INBOX")
	f.seed(t, "t", email.ThreadState{})

	reported := make(chan struct{})
	var once sync.Once
	f.gmail.onGet = func(string) {
		once.Do(func() {
			go func() {
				_ = f.syncer.gmailThreadsChanged(context.Background(), []string{"t"})
				close(reported)
			}()
			// Give an unserialised report every chance to land before the
			// sync records its result.
			select {
			case <-reported:
			case <-time.After(100 * time.Millisecond):
			}
		})
	}

	f.tick(t)
	<-reported

	row, err := f.store.Get(context.Background(), testLoginID, "t")
	if err != nil || row == nil || !row.GmailChanged {
		t.Errorf("row after the sync = %+v, %v; want the Gmail change still marked", row, err)
	}
}
