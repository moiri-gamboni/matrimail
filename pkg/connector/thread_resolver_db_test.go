package connector

import (
	"context"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"

	"github.com/Leicas/matrimail/pkg/common"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// These run the resolver against a real bridgev2 schema in an in-memory SQLite
// database, because the bug they exist to prevent is precisely one that only a
// real schema can catch.
//
// The resolver previously queried `message` and `messages` for `portal_id`
// filtered on `network`, `remote_id` and `receiver`. None of those columns or
// the plural table exist. Every query failed on an unknown column, the error
// was discarded as an expected schema mismatch, and the function returned "no
// match" -- which is a valid answer, so nothing calling it could tell that it had
// never once returned anything else. No unit test over hand-built structs can
// see that; the query has to meet the schema.

const testBridgeID networkid.BridgeID = "email"

func newTestBridge(t *testing.T) *bridgev2.Bridge {
	t.Helper()

	rawDB, err := dbutil.NewWithDialect("file::memory:?cache=shared&_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })

	db := database.New(testBridgeID, database.MetaTypes{}, rawDB)
	if err := db.Upgrade(context.Background()); err != nil {
		t.Fatalf("apply bridgev2 schema: %v", err)
	}
	return &bridgev2.Bridge{DB: db}
}

// bridgeMessage stores a message the way the bridge does, satisfying the
// portal and ghost foreign keys the message table declares.
func bridgeMessage(t *testing.T, br *bridgev2.Bridge, receiver networkid.UserLoginID, portalID networkid.PortalID, msgID networkid.MessageID) {
	t.Helper()
	ctx := context.Background()
	key := networkid.PortalKey{ID: portalID, Receiver: receiver}

	if err := br.DB.Portal.Insert(ctx, &database.Portal{
		BridgeID:  testBridgeID,
		PortalKey: key,
		MXID:      id.RoomID("!room:example.com"),
	}); err != nil {
		t.Fatalf("insert portal: %v", err)
	}
	if err := br.DB.Ghost.Insert(ctx, &database.Ghost{
		BridgeID: testBridgeID,
		ID:       networkid.UserID("alice@example.com"),
	}); err != nil {
		t.Fatalf("insert ghost: %v", err)
	}
	if err := br.DB.Message.Insert(ctx, &database.Message{
		BridgeID:   testBridgeID,
		ID:         msgID,
		PartID:     "",
		MXID:       id.EventID("$evt:example.com"),
		Room:       key,
		SenderID:   networkid.UserID("alice@example.com"),
		SenderMXID: id.UserID("@alice:example.com"),
		Timestamp:  time.Unix(1750000000, 0),
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

// The case the whole resolver exists for: an inbound arrives whose parent is no
// longer in the in-memory cache, and the answer decides whether the
// conversation continues in its room or opens a second one.
func TestDBThreadResolver_FindsAPreviouslyBridgedMessage(t *testing.T) {
	br := newTestBridge(t)
	const receiver networkid.UserLoginID = "moiri@apartresearch.com"
	// Stored through the same constructor the inbound path uses, so this asserts
	// the resolver reads back what that constructor writes.
	bridgeMessage(t, br, receiver, "thread:original@example.com", common.EmailToMessageID("parent@example.com"))

	log := zerolog.Nop()
	r := &DBThreadMetadataResolver{Bridge: br, Log: &log}

	tid, ok := r.ResolveThreadID(string(receiver), "parent@example.com")
	if !ok {
		t.Fatal("no match for a message this bridge stored itself — every thread would open a second room after a cache eviction")
	}
	// The "thread:" portal-key prefix must be stripped: the ThreadManager keys
	// on the bare thread ID, so returning the prefixed form is a silent miss.
	if tid != "original@example.com" {
		t.Errorf("thread ID = %q, want original@example.com (prefix stripped)", tid)
	}
}

// A miss must stay a miss. The header heuristics downstream are the correct
// answer for a message this bridge has never seen, and inventing a thread here
// would merge unrelated conversations into one room.
func TestDBThreadResolver_UnknownMessageDoesNotMatch(t *testing.T) {
	br := newTestBridge(t)
	const receiver networkid.UserLoginID = "moiri@apartresearch.com"
	bridgeMessage(t, br, receiver, "thread:original@example.com", common.EmailToMessageID("parent@example.com"))

	log := zerolog.Nop()
	r := &DBThreadMetadataResolver{Bridge: br, Log: &log}

	if tid, ok := r.ResolveThreadID(string(receiver), "never-seen@example.com"); ok {
		t.Errorf("unknown Message-ID resolved to %q; it must fall through to the header heuristics", tid)
	}
	if tid, ok := r.ResolveThreadID(string(receiver), ""); ok {
		t.Errorf("empty Message-ID resolved to %q", tid)
	}
}

func TestNormalizeThreadID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"thread:abc@example.com", "abc@example.com"},
		{"  thread:abc@example.com  ", "abc@example.com"},
		{"abc@example.com", "abc@example.com"},
		{"", ""},
	} {
		if got := normalizeThreadID(tc.in); got != tc.want {
			t.Errorf("normalizeThreadID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
