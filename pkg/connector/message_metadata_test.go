package connector

import (
	"context"
	"github.com/Leicas/matrimail/pkg/email"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
	"testing"
	"time"
)

func TestBodyAddressingThroughQueueDatabaseJoin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	key := networkid.PortalKey{ID: "thread:root", Receiver: "email:me@example.com"}
	portal := &database.Portal{BridgeID: testBridgeID, PortalKey: key, MXID: "!room:example.com"}
	if err := db.Portal.Insert(ctx, portal); err != nil {
		t.Fatal(err)
	}
	if err := db.Ghost.Insert(ctx, &database.Ghost{BridgeID: testBridgeID, ID: "email:alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	log := zerolog.Nop()
	processor := email.NewProcessor(&log, email.NewThreadManager(nil), false, "")
	login := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: key.Receiver}}
	stamp := time.UnixMilli(1750000000123)
	for i, recipient := range []string{"Me <ME@Example.com>", "other@example.com"} {
		parsed := &email.ParsedEmail{MessageID: string(rune('a'+i)) + "@example.com", From: "Alice <ALICE@Example.com>", To: []string{recipient}, Date: stamp.Add(time.Duration(i) * time.Second), TextContent: "fixture"}
		message, err := processor.ProcessParsedEmail(ctx, parsed, login, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		converted, err := processor.ToMatrixEvent(ctx, message, login).ConvertMessage(ctx, &bridgev2.Portal{Portal: portal}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range converted.Parts {
			if part.ID != "body" {
				continue
			}
			row := &database.Message{BridgeID: testBridgeID, Room: key, ID: message.MessageID, PartID: part.ID, MXID: id.EventID("$" + parsed.MessageID), SenderID: "email:alice@example.com", Timestamp: message.Timestamp}
			row.Metadata = part.DBMetadata
			if err := db.Message.Insert(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
	}
	var raw string
	err := db.QueryRow(ctx, `SELECT message.metadata FROM portal JOIN message ON portal.id=message.room_id AND portal.receiver=message.room_receiver WHERE portal.mxid=? AND message.part_id='body' AND message.timestamp/1000000=?`, "!room:example.com", stamp.UnixMilli()).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if raw != `{"from":"alice@example.com","to":["me@example.com"],"cc":[],"delivered_to":[]}` {
		t.Fatalf("queue join metadata=%s", raw)
	}
}
