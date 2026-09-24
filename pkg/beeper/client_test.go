package beeper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// fakeServer answers like Beeper Server's Desktop API. The list and account
// bodies are live captures with names and IDs replaced (testdata/); the
// shapes of the write calls and errors are from the API's OpenAPI document
// and live error captures.
type fakeServer struct {
	t      *testing.T
	token  string
	routes map[string]string // "METHOD path?query" -> body; the path as received, decoded

	mu       sync.Mutex
	requests []string // "METHOD path?query body"
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.requests = append(f.requests, strings.TrimSpace(key+" "+string(body)))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Invalid token","code":"unauthorized"}`)
		return
	}
	resp, ok := f.routes[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"Chat not found: %s","code":"NOT_FOUND"}`, r.URL.Path)
		return
	}
	fmt.Fprint(w, resp)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestClient(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := zerolog.Nop()
	return &Client{BaseURL: srv.URL, TokenFile: tokenFile, HTTP: srv.Client(), Log: &log}
}

// The list is read to its end, page by page, from the cursor each page hands
// back: an account can hold more chats than one page's limit of 200.
func TestListChats_ReadsEveryPage(t *testing.T) {
	t.Parallel()
	acct := "accountIDs=sh-email_email%3Ame%40example.com"
	f := &fakeServer{t: t, token: "tok", routes: map[string]string{
		"GET /v1/chats?" + acct + "&limit=200":                                           fixture(t, "chats-page1.json"),
		"GET /v1/chats?" + acct + "&cursor=1790000000000%3A7&direction=before&limit=200": fixture(t, "chats-page2.json"),
	}}
	c := newTestClient(t, f)

	chats, err := c.ListChats(context.Background(), "sh-email_email:me@example.com")
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	want := []Chat{
		{ID: "!roomA:beeper.local", IsArchived: false, UnreadCount: 2, IsMarkedUnread: false},
		{ID: "!roomB:beeper.local", IsArchived: true, UnreadCount: 0, IsMarkedUnread: true},
	}
	if fmt.Sprint(chats) != fmt.Sprint(want) {
		t.Errorf("ListChats = %+v; want %+v", chats, want)
	}
}

// A chat is unread while it holds unread messages or was marked unread by hand.
func TestChatUnread(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c    Chat
		want bool
	}{
		{Chat{UnreadCount: 0}, false},
		{Chat{UnreadCount: 3}, true},
		{Chat{IsMarkedUnread: true}, true},
	} {
		if got := tc.c.Unread(); got != tc.want {
			t.Errorf("%+v.Unread() = %v; want %v", tc.c, got, tc.want)
		}
	}
}

// The account a bridge login appears under is found by its login ID.
func TestAccountID_MatchesTheLogin(t *testing.T) {
	t.Parallel()
	f := &fakeServer{t: t, token: "tok", routes: map[string]string{"GET /v1/accounts": fixture(t, "accounts.json")}}
	c := newTestClient(t, f)

	got, err := c.AccountID(context.Background(), "email:me@example.com")
	if err != nil || got != "sh-email_email:me@example.com" {
		t.Errorf("AccountID = %q, %v; want sh-email_email:me@example.com", got, err)
	}
	if _, err := c.AccountID(context.Background(), "email:other@example.com"); err == nil ||
		!strings.Contains(err.Error(), "email:other@example.com") {
		t.Errorf("AccountID of an unknown login returned %v; want an error naming it", err)
	}
}

// Each write goes to the route and body the API documents.
func TestWrites(t *testing.T) {
	t.Parallel()
	chat := `{"id":"!roomA:beeper.local","isArchived":false,"unreadCount":0,"isMarkedUnread":false}`
	f := &fakeServer{t: t, token: "tok", routes: map[string]string{
		"PATCH /v1/chats/!roomA:beeper.local":       chat,
		"POST /v1/chats/!roomA:beeper.local/read":   chat,
		"POST /v1/chats/!roomA:beeper.local/unread": chat,
	}}
	c := newTestClient(t, f)
	ctx := context.Background()

	for _, step := range []error{
		c.SetArchived(ctx, "!roomA:beeper.local", true),
		c.SetArchived(ctx, "!roomA:beeper.local", false),
		c.SetUnread(ctx, "!roomA:beeper.local", false),
		c.SetUnread(ctx, "!roomA:beeper.local", true),
	} {
		if step != nil {
			t.Fatalf("write failed: %v", step)
		}
	}
	want := []string{
		`PATCH /v1/chats/!roomA:beeper.local {"isArchived":true}`,
		`PATCH /v1/chats/!roomA:beeper.local {"isArchived":false}`,
		`POST /v1/chats/!roomA:beeper.local/read {}`,
		`POST /v1/chats/!roomA:beeper.local/unread {}`,
	}
	if strings.Join(f.requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}
}

func TestGetChat(t *testing.T) {
	t.Parallel()
	f := &fakeServer{t: t, token: "tok", routes: map[string]string{
		"GET /v1/chats/!roomA:beeper.local": `{"id":"!roomA:beeper.local","isArchived":true,"unreadCount":1,"isMarkedUnread":false}`,
	}}
	c := newTestClient(t, f)
	got, err := c.GetChat(context.Background(), "!roomA:beeper.local")
	want := Chat{ID: "!roomA:beeper.local", IsArchived: true, UnreadCount: 1}
	if err != nil || got != want {
		t.Errorf("GetChat = %+v, %v; want %+v", got, err, want)
	}
}

// A refused token is an error carrying the status and the server's message,
// and the token file is read on every call, so a replaced token is picked up
// without a restart.
func TestUnauthorizedThenReplacedToken(t *testing.T) {
	t.Parallel()
	f := &fakeServer{t: t, token: "tok", routes: map[string]string{"GET /v1/accounts": fixture(t, "accounts.json")}}
	c := newTestClient(t, f)
	f.token = "rotated"

	_, err := c.AccountID(context.Background(), "email:me@example.com")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "Invalid token") {
		t.Fatalf("AccountID with a refused token returned %v; want a 401 APIError with the body", err)
	}

	if err := os.WriteFile(c.TokenFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccountID(context.Background(), "email:me@example.com"); err != nil {
		t.Errorf("AccountID after the token file changed: %v", err)
	}
}
