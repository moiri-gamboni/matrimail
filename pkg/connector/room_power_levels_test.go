package connector

import (
	"bytes"
	"context"
	"errors"
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
	levels   map[id.RoomID]*event.PowerLevelsEventContent
	failRead id.RoomID
}

func (m *roomPowerMatrix) GetPowerLevels(_ context.Context, room id.RoomID) (*event.PowerLevelsEventContent, error) {
	if room == m.failRead {
		return nil, errors.New("read failed")
	}
	return m.levels[room], nil
}

type roomPowerBot struct {
	bridgev2.MatrixAPI
	matrix   *roomPowerMatrix
	writes   int
	failSend bool
}

func (b *roomPowerBot) GetMXID() id.UserID { return "@bot:example.com" }
func (b *roomPowerBot) SendState(_ context.Context, room id.RoomID, typ event.Type, key string, content *event.Content, _ time.Time) (*mautrix.RespSendEvent, error) {
	if typ != event.StatePowerLevels || key != "" {
		return nil, errors.New("unexpected state")
	}
	if b.failSend {
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
	matrix := &roomPowerMatrix{levels: make(map[id.RoomID]*event.PowerLevelsEventContent)}
	for _, name := range []string{"one", "two", "uncreated"} {
		room := id.RoomID("!" + name + ":example.com")
		if name == "uncreated" {
			room = ""
		}
		if err := db.Portal.Insert(ctx, &database.Portal{BridgeID: testBridgeID, PortalKey: networkid.PortalKey{ID: networkid.PortalID("thread:" + name)}, MXID: room}); err != nil {
			t.Fatal(err)
		}
		matrix.levels[room] = &event.PowerLevelsEventContent{Users: map[id.UserID]int{"@bot:example.com": 9001, "@human:example.com": 0}, Events: map[string]int{"m.reaction": 101, "m.room.message": 101, "m.room.redaction": 101, "com.example.custom": 42}}
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
			ec, bot, _ := reactionRoomConnector(t)
			if failure == "read" {
				bot.matrix.failRead = "!one:example.com"
			} else {
				bot.failSend = true
			}
			if err := ec.Start(context.Background()); err == nil {
				t.Error("startup silently succeeded")
			}
			if failure == "read" && bot.writes != 1 {
				t.Errorf("healthy room was not updated: %d writes", bot.writes)
			}
			if failure == "send" {
				if bot.matrix.levels["!one:example.com"].GetEventLevel(event.EventReaction) != 101 {
					t.Error("failed write changed cached state")
				}
				bot.failSend = false
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
