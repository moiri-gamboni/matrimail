package connector

import (
	"bytes"
	"context"
	"fmt"
	"golang.org/x/oauth2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Leicas/matrimail/pkg/email"
	"github.com/rs/zerolog"
	gmail "google.golang.org/api/gmail/v1"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type retryTestMatrix struct{ bridgev2.MatrixConnector }

func (retryTestMatrix) Init(*bridgev2.Bridge)         {}
func (retryTestMatrix) BotIntent() bridgev2.MatrixAPI { return nil }

type retryTestNetwork struct{ bridgev2.NetworkConnector }

func (retryTestNetwork) Init(*bridgev2.Bridge) {}
func (retryTestNetwork) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}
func (retryTestNetwork) GetDBMetaTypes() database.MetaTypes {
	return (&EmailConnector{}).GetDBMetaTypes()
}

func TestGmailInboundFailsWhenReplyContextCannotBeSaved(t *testing.T) {
	previousBuffer := bridgev2.PortalEventBuffer
	bridgev2.PortalEventBuffer = 0
	defer func() { bridgev2.PortalEventBuffer = previousBuffer }()
	db := newTestDB(t)
	log := zerolog.Nop()
	br := bridgev2.NewBridge(testBridgeID, db.Database, log, nil, retryTestMatrix{}, retryTestNetwork{}, func(*bridgev2.Bridge) bridgev2.CommandProcessor { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A stopped event queue prevents delivery from obscuring a persistence failure.
	br.BackgroundCtx = ctx
	key := networkid.PortalKey{ID: "thread:message@example.com", Receiver: testLoginID}
	if err := br.DB.Portal.Insert(context.Background(), &database.Portal{BridgeID: testBridgeID, PortalKey: key, MXID: "!room:example.com", Metadata: &PortalMetadata{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := br.GetExistingPortalByKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `CREATE TRIGGER fail_portal_update BEFORE UPDATE ON portal BEGIN SELECT RAISE(FAIL, 'database unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	manager := &GmailInboundManager{connector: &EmailConnector{Bridge: br, Processor: email.NewProcessor(&log, email.NewThreadManager(nil), false, "")}}
	login := &bridgev2.UserLogin{Bridge: br, UserLogin: &database.UserLogin{ID: testLoginID}, Log: log}
	msg := &gmail.Message{Id: "gmail-message", LabelIds: []string{"INBOX"}, Payload: &gmail.MessagePart{MimeType: "text/plain", Headers: []*gmail.MessagePartHeader{{Name: "Message-ID", Value: "<message@example.com>"}, {Name: "From", Value: "alice@example.com"}}, Body: &gmail.MessagePartBody{Data: "aGk="}}}
	err := manager.handleMessage(context.Background(), login, msg, "INBOX", nil)
	if err == nil || !strings.Contains(err.Error(), "persist thread state") || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("message error=%v", err)
	}
}

type recordingRetryIntent struct {
	bridgev2.MatrixAPI
	ids []networkid.MessageID
}

func (i *recordingRetryIntent) GetMXID() id.UserID                                        { return "@ghost:example.com" }
func (i *recordingRetryIntent) IsDoublePuppet() bool                                      { return false }
func (i *recordingRetryIntent) EnsureInvited(context.Context, id.RoomID, id.UserID) error { return nil }
func (i *recordingRetryIntent) SendMessage(_ context.Context, _ id.RoomID, _ event.Type, _ *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	if extra.MessageMeta.PartID == "body" {
		i.ids = append(i.ids, extra.MessageMeta.ID)
	}
	return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$%d", len(i.ids)))}, nil
}

type recordingRetryMatrix struct {
	retryTestMatrix
	intent *recordingRetryIntent
}

func (m recordingRetryMatrix) NewUserIntent(context.Context, id.UserID, string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", nil
}
func (m recordingRetryMatrix) BotIntent() bridgev2.MatrixAPI                   { return m.intent }
func (m recordingRetryMatrix) GhostIntent(networkid.UserID) bridgev2.MatrixAPI { return m.intent }

func TestGmailPollerReplayDeduplicatesThroughConnector(t *testing.T) {
	previousBuffer := bridgev2.PortalEventBuffer
	bridgev2.PortalEventBuffer = 0
	defer func() { bridgev2.PortalEventBuffer = previousBuffer }()
	db := newTestDB(t)
	var logs bytes.Buffer
	log := zerolog.New(&logs)
	intent := &recordingRetryIntent{}
	br := bridgev2.NewBridge(testBridgeID, db.Database, log, nil, recordingRetryMatrix{intent: intent}, retryTestNetwork{}, func(*bridgev2.Bridge) bridgev2.CommandProcessor { return nil })
	br.BackgroundCtx = context.Background()
	key := networkid.PortalKey{ID: "thread:a@example.com", Receiver: testLoginID}
	if err := br.DB.Portal.Insert(context.Background(), &database.Portal{BridgeID: testBridgeID, PortalKey: key, MXID: "!room:example.com", Metadata: &PortalMetadata{GmailThreadID: "gmail"}}); err != nil {
		t.Fatal(err)
	}
	if err := br.DB.Ghost.Insert(context.Background(), &database.Ghost{BridgeID: testBridgeID, ID: "email:alice@example.com", Name: "Alice", NameSet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `CREATE TRIGGER fail_b BEFORE UPDATE ON portal WHEN json_extract(NEW.metadata, '$.last_message_id') = 'b@example.com' BEGIN SELECT RAISE(FAIL, 'database unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	tm := email.NewThreadManager(&DBThreadMetadataResolver{Bridge: br, Log: &log})
	manager := &GmailInboundManager{connector: &EmailConnector{Bridge: br, Processor: email.NewProcessor(&log, tm, false, "")}}
	dbUser := &database.User{BridgeID: testBridgeID, MXID: "@me:example.com"}
	if err := br.DB.User.Insert(context.Background(), dbUser); err != nil {
		t.Fatal(err)
	}
	dbLogin := &database.UserLogin{BridgeID: testBridgeID, ID: testLoginID, UserMXID: dbUser.MXID}
	if err := br.DB.UserLogin.Insert(context.Background(), dbLogin); err != nil {
		t.Fatal(err)
	}
	login := &bridgev2.UserLogin{Bridge: br, UserLogin: dbLogin, User: &bridgev2.User{Bridge: br, User: dbUser, Log: log}, Log: log}
	originalTransport := http.DefaultTransport
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"historyId":"110","history":[{"id":"101","messagesAdded":[{"message":{"id":"a"}},{"message":{"id":"b"}},{"message":{"id":"c"}}]}]}`
		if strings.Contains(r.URL.Path, "/messages/") {
			mid := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body = fmt.Sprintf(`{"id":%q,"threadId":"gmail","labelIds":["INBOX"],"payload":{"mimeType":"text/plain","headers":[{"name":"Message-ID","value":"<%s@example.com>"},{"name":"From","value":"alice@example.com"}],"body":{"data":"aGk="}}}`, mid, mid)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	defer transport.CloseIdleConnections()
	defer func() { http.DefaultTransport = originalTransport }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved []uint64
	var attempts []string
	poller := &email.GmailHistoryPoller{Email: "me@example.com", MonitoredLabelIDs: []string{"INBOX"}, PollInterval: time.Millisecond, Log: &log, TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), CursorLoad: func(context.Context) (uint64, error) { return 100, nil }, CursorSave: func(_ context.Context, c uint64) error { saved = append(saved, c); cancel(); return nil }, OnMessage: func(ctx context.Context, m *gmail.Message, mailbox string) error {
		attempts = append(attempts, m.Id)
		err := manager.handleMessage(ctx, login, m, mailbox, nil)
		if err != nil {
			if _, dropErr := db.Exec(context.Background(), "DROP TRIGGER fail_b"); dropErr != nil {
				t.Fatal(dropErr)
			}
		}
		return err
	}}
	timeout := time.AfterFunc(5*time.Second, cancel)
	defer timeout.Stop()
	_ = poller.Run(ctx)
	if strings.Join(attempts, ",") != "a,b,a,b,c" || fmt.Sprint(saved) != "[110]" || fmt.Sprint(intent.ids) != "[email:a@example.com email:b@example.com email:c@example.com]" {
		t.Fatalf("attempts=%v saved=%v sent=%v logs=%s", attempts, saved, intent.ids, logs.String())
	}
	portal, err := br.DB.Portal.GetByKey(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got := portal.Metadata.(*PortalMetadata).LastInboundMessageID; got != "c@example.com" {
		t.Fatalf("reply context=%s", got)
	}
}
