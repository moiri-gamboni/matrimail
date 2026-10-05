package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/appservice"
	bridgematrix "maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/sqlstatestore"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type roomPowerMatrix struct {
	bridgev2.MatrixConnector
	levels     map[id.RoomID]*event.PowerLevelsEventContent
	failRead   id.RoomID
	emptyCache bool
	rawContent map[id.RoomID]json.RawMessage
}

func (m *roomPowerMatrix) GetPowerLevels(_ context.Context, room id.RoomID) (*event.PowerLevelsEventContent, error) {
	if m.emptyCache {
		return &event.PowerLevelsEventContent{}, nil
	}
	if room == m.failRead {
		return nil, errors.New("read failed")
	}
	return m.levels[room], nil
}

func (m *roomPowerMatrix) GetStateEvent(_ context.Context, room id.RoomID, typ event.Type, key string) (*event.Event, error) {
	if typ != event.StatePowerLevels || key != "" {
		return nil, errors.New("unexpected state read")
	}
	if room == m.failRead {
		return nil, errors.New("read failed")
	}
	raw := m.rawContent[room]
	if raw == nil {
		var err error
		raw, err = json.Marshal(m.levels[room])
		if err != nil {
			return nil, err
		}
	}
	return &event.Event{Type: event.StatePowerLevels, RoomID: room, Content: event.Content{VeryRaw: raw}}, nil
}

type roomPowerBot struct {
	bridgev2.MatrixAPI
	matrix   *roomPowerMatrix
	writes   int
	failSend id.RoomID
}

func (b *roomPowerBot) GetMXID() id.UserID { return "@bot:example.com" }
func (b *roomPowerBot) SendState(_ context.Context, room id.RoomID, typ event.Type, key string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	if typ != event.StatePowerLevels || key != "" {
		return nil, errors.New("unexpected state")
	}
	if room == b.failSend {
		return nil, errors.New("send failed")
	}
	b.matrix.levels[room] = content.Parsed.(*event.PowerLevelsEventContent)
	b.writes++
	return &mautrix.RespSendEvent{}, nil
}

func reactionRoomConnector(t *testing.T) (*EmailConnector, *roomPowerBot, *bytes.Buffer) {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()
	matrix := &roomPowerMatrix{rawContent: make(map[id.RoomID]json.RawMessage), levels: make(map[id.RoomID]*event.PowerLevelsEventContent)}
	for _, name := range []string{"one", "two", "uncreated"} {
		room := id.RoomID("!" + name + ":example.com")
		if name == "uncreated" {
			room = ""
		}
		if err := db.Portal.Insert(ctx, &database.Portal{BridgeID: testBridgeID, PortalKey: networkid.PortalKey{ID: networkid.PortalID("thread:" + name)}, MXID: room}); err != nil {
			t.Fatal(err)
		}
		// Derived from bridgev2 room creation defaults and the old email overrides, not a live capture.
		raw, err := os.ReadFile("testdata/email_room_power_levels.json")
		if err != nil {
			t.Fatal(err)
		}
		var state event.Event
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		power := &event.PowerLevelsEventContent{}
		if err := json.Unmarshal(state.Content.VeryRaw, power); err != nil {
			t.Fatal(err)
		}
		matrix.levels[room] = power
	}
	bot := &roomPowerBot{matrix: matrix}
	output := &bytes.Buffer{}
	return &EmailConnector{Bridge: &bridgev2.Bridge{DB: db, Matrix: matrix, Bot: bot, Log: zerolog.New(output)}}, bot, output
}

func TestStartupUpdatesExistingEmailRoomPowerLevelsIdempotently(t *testing.T) {
	ec, bot, output := reactionRoomConnector(t)
	for run := 0; run < 2; run++ {
		if err := ec.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if bot.writes != 2 {
			t.Fatalf("state writes = %d, want 2 after run %d", bot.writes, run)
		}
	}
	for _, room := range []id.RoomID{"!one:example.com", "!two:example.com"} {
		power := bot.matrix.levels[room]
		if power.GetEventLevel(event.EventReaction) != 0 {
			t.Error("reactions blocked")
		}
		if power.GetEventLevel(event.EventMessage) != 101 || power.GetEventLevel(event.EventRedaction) != 101 || power.Redact() != 101 {
			t.Error("message/redaction restrictions lost")
		}
		if power.Events["com.example.custom"] != 42 || power.Users["@bot:example.com"] != 9001 {
			t.Error("unrelated power levels lost")
		}
	}
	if !strings.Contains(output.String(), `"updated":2`) || !strings.Contains(output.String(), `"updated":0`) {
		t.Fatalf("missing counts: %s", output)
	}
}

func TestStartupPowerLevelFailuresAreReported(t *testing.T) {
	for _, failure := range []string{"read", "send"} {
		t.Run(failure, func(t *testing.T) {
			ec, bot, output := reactionRoomConnector(t)
			if failure == "read" {
				bot.matrix.failRead = "!one:example.com"
			} else {
				bot.failSend = "!one:example.com"
			}
			if err := ec.Start(context.Background()); err != nil {
				t.Errorf("refresh failure stopped startup: %v", err)
			}
			if bot.writes != 1 {
				t.Errorf("healthy room was not updated: %d writes", bot.writes)
			}
			errorLogged := false
			for _, line := range strings.Split(output.String(), "\n") {
				if strings.Contains(line, `"level":"error"`) && strings.Contains(line, `"room_id":"!one:example.com"`) && strings.Contains(line, `"error":"`+failure+` failed"`) {
					errorLogged = true
				}
			}
			if !errorLogged || !strings.Contains(output.String(), `"updated":1,"already_ok":0,"failed":1`) {
				t.Errorf("missing error and summary logs: %s", output)
			}
			if failure == "send" {
				if bot.matrix.levels["!one:example.com"].GetEventLevel(event.EventReaction) != 101 {
					t.Error("failed write changed cached state")
				}
				bot.failSend = ""
				if err := ec.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				if bot.writes != 2 {
					t.Errorf("failed writes were not retried: %d", bot.writes)
				}
			}
		})
	}
}

func TestStartupContinuesWhenPortalListingFails(t *testing.T) {
	ec, _, output := reactionRoomConnector(t)
	if err := ec.Bridge.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ec.Start(context.Background()); err != nil {
		t.Errorf("list failure stopped startup: %v", err)
	}
	if !strings.Contains(output.String(), `"level":"error"`) {
		t.Errorf("list failure was not logged: %s", output)
	}
}

func TestStartupReadsFreshStateWhenCachedPowerLevelsAreEmpty(t *testing.T) {
	ec, bot, output := reactionRoomConnector(t)
	bot.matrix.emptyCache = true
	if err := ec.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bot.writes != 2 {
		t.Errorf("writes = %d, want both homeserver rooms updated", bot.writes)
	}
	if got := bot.matrix.levels["!one:example.com"].GetEventLevel(event.EventReaction); got != 0 {
		t.Errorf("reaction level = %d, want 0", got)
	}
	if !strings.Contains(output.String(), `"updated":2,"already_ok":0,"failed":0`) {
		t.Errorf("missing fresh-state summary: %s", output)
	}
	output.Reset()
	if err := ec.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"updated":0,"already_ok":2,"failed":0`) {
		t.Errorf("missing already-ok count: %s", output)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, "refresh decision") && !strings.Contains(line, `"level":"debug"`) {
			t.Errorf("unchanged room decision not at debug level: %s", line)
		}
	}
}

func TestStartupRejectsIncompleteOrInvalidFreshPowerLevels(t *testing.T) {
	for _, raw := range []string{`{}`, `{"users":{"@human:example.com":0}}`, `{"users":[]}`, `{"users":{"@bot:example.com":0},"events":{"m.reaction":101}}`} {
		t.Run(raw, func(t *testing.T) {
			ec, bot, output := reactionRoomConnector(t)
			bot.matrix.rawContent["!one:example.com"] = json.RawMessage(raw)
			if err := ec.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if bot.writes != 1 {
				t.Errorf("writes = %d, want healthy room only", bot.writes)
			}
			if !strings.Contains(output.String(), `"level":"error"`) || !strings.Contains(output.String(), `"updated":1,"already_ok":0,"failed":1`) {
				t.Errorf("incomplete state silently skipped: %s", output)
			}
		})
	}
}

func TestStartupFreshStateBypassesSQLNullPowerLevels(t *testing.T) {
	ec, bot, output := reactionRoomConnector(t)
	ctx := context.Background()
	store := sqlstatestore.NewSQLStateStore(ec.Bridge.DB.Database, dbutil.NoopLogger, true)
	if err := store.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec(ctx, "INSERT INTO mx_room_state (room_id) VALUES ($1)", "!one:example.com"); err != nil {
		t.Fatal(err)
	}
	cached, err := store.GetPowerLevels(ctx, "!one:example.com")
	if err != nil || cached == nil || len(cached.Users) != 0 {
		t.Fatalf("SQL NULL cache = %v, %v", cached, err)
	}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("format") != "event" || !strings.HasSuffix(r.URL.Path, "/state/m.room.power_levels/") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		room := id.RoomID("!one:example.com")
		if strings.Contains(r.URL.Path, "!two:example.com") {
			room = "!two:example.com"
		}
		stateKey := ""
		reads++
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&event.Event{Type: event.StatePowerLevels, StateKey: &stateKey, RoomID: room, Content: event.Content{Parsed: bot.matrix.levels[room]}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@bot:example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.StateStore = store
	ec.Bridge.Matrix = &bridgematrix.Connector{Bot: &appservice.IntentAPI{Client: client}}
	if err := ec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || bot.writes != 2 {
		t.Errorf("fresh reads/writes = %d/%d, want 2/2", reads, bot.writes)
	}
	if !strings.Contains(output.String(), `"bot_level":9001,"human_level":0`) || !strings.Contains(output.String(), `"reaction_level":101,"events_default":0,"changed":true`) {
		t.Errorf("missing decision fields: %s", output)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, "refresh decision") && strings.Contains(line, `"changed":true`) && !strings.Contains(line, `"level":"info"`) {
			t.Errorf("changed room decision not at info level: %s", line)
		}
	}
}
