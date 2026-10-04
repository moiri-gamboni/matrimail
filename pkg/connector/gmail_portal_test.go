package connector

import (
	"context"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
	"testing"
	"time"
)

func TestGmailPortalResolverAndSyncUseOldestRoom(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.Ghost.Insert(ctx, &database.Ghost{BridgeID: testBridgeID, ID: "email:alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"new", "old", "other"} {
		receiver := networkid.UserLoginID(testLoginID)
		if room == "other" {
			receiver = "email:other@example.com"
		}
		key := networkid.PortalKey{ID: networkid.PortalID("thread:" + room), Receiver: receiver}
		if err := db.Portal.Insert(ctx, &database.Portal{BridgeID: testBridgeID, PortalKey: key, MXID: id.RoomID("!" + room + ":example.com"), Metadata: &PortalMetadata{GmailThreadID: "gmail"}}); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(200, 0)
		if room != "new" {
			stamp = time.Unix(100, 0)
		}
		if err := db.Message.Insert(ctx, &database.Message{BridgeID: testBridgeID, Room: key, ID: networkid.MessageID(room), PartID: "body", MXID: id.EventID("$" + room), SenderID: "email:alice@example.com", Timestamp: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	resolve := gmailThreadForChat(db, testLoginID)
	for room, want := range map[string]string{"!old:example.com": "gmail", "!new:example.com": ""} {
		got, err := resolve(ctx, room)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v want %q", room, got, err, want)
		}
	}
	log := zerolog.Nop()
	r := &DBThreadMetadataResolver{Bridge: &bridgev2.Bridge{DB: db}, Log: &log}
	got, ok := r.ResolveGmailThreadID(testLoginID, "gmail")
	if !ok || got != "old" {
		t.Fatalf("resolved %q, %v", got, ok)
	}
}
