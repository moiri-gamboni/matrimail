package connector

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
)

func TestOAuthPasteTokenPreservesWatchedFolders(t *testing.T) {
	t.Setenv("MATRIMAIL_PASSPHRASE", "test-only-passphrase")
	t.Chdir(t.TempDir())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == "POST" && req.URL.Path == "/token":
			io.WriteString(w, `{"access_token":"test-access","token_type":"Bearer","expires_in":3600}`)
		case req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/users/me/profile"):
			io.WriteString(w, `{"emailAddress":"me@example.com"}`)
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	ctx := context.Background()
	log := zerolog.Nop()
	q := &EmailAccountQuery{DB: newTestDB(t)}
	if err := q.CreateTable(ctx); err != nil {
		t.Fatal(err)
	}
	bot := &reactionNoticeBot{}
	br := &bridgev2.Bridge{Config: &bridgeconfig.BridgeConfig{}, Bot: bot, Log: log}
	connector := &EmailConnector{Bridge: br, DB: q, Config: Config{GmailOAuth: GmailOAuthConfig{ClientID: "test-client", ClientSecret: "test-secret"}}}
	user := &bridgev2.User{User: &database.User{MXID: "@me:example.com"}}
	for _, saved := range [][]string{nil, {"INBOX", "SENT", "custom"}} {
		if saved != nil {
			account := &EmailAccount{UserMXID: user.MXID.String(), Email: "me@example.com", MonitoredFolders: saved, CreatedAt: time.Now()}
			if err := q.UpsertAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
		}
		ce := &commands.Event{User: user, Bridge: br, Bot: bot, Ctx: ctx, Args: []string{"paste-token", "me@example.com", "test-refresh"}}
		fnOAuthPasteToken(ce, connector)
		got, err := q.GetAccount(ctx, user.MXID.String(), "me@example.com")
		if err != nil || got == nil {
			t.Fatalf("account missing: %v; command reply: %s", err, bot.body)
		}
		want := saved
		if want == nil {
			want = []string{"INBOX", "SENT"}
		}
		if !reflect.DeepEqual(got.MonitoredFolders, want) {
			t.Errorf("folders=%v want=%v", got.MonitoredFolders, want)
		}
	}
}
