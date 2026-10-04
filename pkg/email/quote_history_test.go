package email

import (
	"context"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/Leicas/matrimail/pkg/common"
)

// The historySeen tests run against bridgev2's real schema in in-memory
// SQLite: the answer comes from mautrix's message query, with its per-account
// receiver filter, which a fake would only imitate.

const historyTestBridgeID networkid.BridgeID = "email"

func newHistoryTestDB(t *testing.T) *database.Database {
	t.Helper()
	rawDB, err := dbutil.NewWithDialect("file:"+t.Name()+"?mode=memory&cache=shared&_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	db := database.New(historyTestBridgeID, database.MetaTypes{}, rawDB)
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("apply bridgev2 schema: %v", err)
	}
	return db
}

func storeBridged(t *testing.T, db *database.Database, receiver networkid.UserLoginID, msgID networkid.MessageID) {
	t.Helper()
	ctx := context.Background()
	key := networkid.PortalKey{ID: "thread:root@example.com", Receiver: receiver}
	if err := db.Portal.Insert(ctx, &database.Portal{BridgeID: historyTestBridgeID, PortalKey: key, MXID: id.RoomID("!room:example.com")}); err != nil {
		t.Fatalf("insert portal: %v", err)
	}
	if err := db.Ghost.Insert(ctx, &database.Ghost{BridgeID: historyTestBridgeID, ID: "alice@example.com"}); err != nil {
		t.Fatalf("insert ghost: %v", err)
	}
	if err := db.Message.Insert(ctx, &database.Message{
		BridgeID: historyTestBridgeID, ID: msgID, PartID: "body", MXID: id.EventID("$evt:example.com"),
		Room: key, SenderID: "alice@example.com", SenderMXID: id.UserID("@alice:example.com"),
		Timestamp: time.Unix(1750000000, 0),
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

const receiver networkid.UserLoginID = "email:reader@example.com"

// A forward of a message the account never received: the forwarded block is
// the only copy, and its Message-ID is in no room.
func TestHistorySeenForwardOfUnseenMessage(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, receiver, common.EmailToMessageID("unrelated@example.com"))

	seen, err := historySeen(context.Background(), db, receiver,
		"notice-1@mailer.example.com", []string{"notice-1@mailer.example.com"})
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if seen {
		t.Fatal("the forwarded original was never bridged, so its history must not be folded away")
	}
}

func TestHistorySeenParentBridged(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, receiver, common.EmailToMessageID("parent@example.com"))

	seen, err := historySeen(context.Background(), db, receiver, "<parent@example.com>", nil)
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if !seen {
		t.Fatal("the parent is in a room, so the quoted history repeats what the reader has seen")
	}
}

// Rows written before the "email:" prefix existed hold the bare Message-ID.
func TestHistorySeenParentBridgedUnprefixed(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, receiver, networkid.MessageID("parent@example.com"))

	seen, err := historySeen(context.Background(), db, receiver, "parent@example.com", nil)
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if !seen {
		t.Fatal("the parent is stored under its bare Message-ID")
	}
}

// A reply-all to a reply the account was left off: the thread's first message
// is in the room, but the parent quoted on top is not.
func TestHistorySeenOlderMessageBridgedParentNot(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, receiver, common.EmailToMessageID("first@example.com"))

	seen, err := historySeen(context.Background(), db, receiver, "offlist-reply@example.com", []string{"first@example.com", "offlist-reply@example.com"})
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if seen {
		t.Fatal("the direct parent was never bridged, so its quoted text is the reader's only copy")
	}
}

// With no In-Reply-To, the last References entry is the parent.
func TestHistorySeenParentFromReferences(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, receiver, common.EmailToMessageID("parent@example.com"))

	seen, err := historySeen(context.Background(), db, receiver, "", []string{"first@example.com", "<parent@example.com>"})
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if !seen {
		t.Fatal("the last References entry is the parent and it is in a room")
	}
}

// Another account's copy of the message says nothing about what this reader saw.
func TestHistorySeenOtherAccountOnly(t *testing.T) {
	db := newHistoryTestDB(t)
	storeBridged(t, db, "email:someone-else@example.com", common.EmailToMessageID("parent@example.com"))

	seen, err := historySeen(context.Background(), db, receiver, "parent@example.com", nil)
	if err != nil {
		t.Fatalf("historySeen: %v", err)
	}
	if seen {
		t.Fatal("only another account bridged the parent")
	}
}

// A forward keeps its forwarded block when not folded; folding would cut it,
// which is why the history check matters.
func TestDisplayBodiesKeepsForwardWhenNotFolded(t *testing.T) {
	text := "any action item?\r\n\r\n---------- Forwarded message ---------\r\nFrom: No reply - Vendor <no-reply@vendor.example.com>\r\nDate: Thu, 1 Oct 2026 at 15:01\r\nSubject: Your credits expire in 60 days\r\nTo: <colleague@example.com>\r\n\r\nYour credits are set to expire in 60 days.\r\n"
	got, _ := displayBodies(text, "", false)
	if got != text {
		t.Fatalf("forward body changed:\n%q", got)
	}
	folded, _ := displayBodies(text, "", true)
	if folded == text {
		t.Fatal("precondition: the fold would cut this forward, which is why the history check matters")
	}
}
