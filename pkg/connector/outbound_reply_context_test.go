package connector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Leicas/matrimail/pkg/email"
)

var selvesForTest = []string{"me@example.com"}

// The user answers Alice from Gmail's web client; the copy arrives through the
// Sent label and is threaded. A reply then typed in Matrix must still go to
// Alice -- in DM mode above all, where the recipient is the last sender alone
// and treating the user's own message as that sender addresses the user.
func TestReplyAfterOwnMessage_AddressesTheOtherParty(t *testing.T) {
	t.Parallel()
	tm := email.NewThreadManager(nil)
	thread := tm.DetermineThread("login-1", &email.ParsedEmail{
		MessageID: "in-1@example.com",
		From:      "Alice <alice@example.com>",
		To:        []string{"me@example.com"},
		Cc:        []string{"bob@example.com"},
	})
	tm.CacheForReceiver("login-1", thread)
	thread = tm.DetermineThread("login-1", &email.ParsedEmail{
		MessageID: "out-1@example.com",
		InReplyTo: "in-1@example.com",
		From:      "Me <me@example.com>",
		To:        []string{"alice@example.com"},
		Outbound:  true,
	})

	dm, err := resolveDMRecipients(thread, selvesForTest)
	if err != nil {
		t.Fatalf("DM recipients: %v", err)
	}
	if len(dm) != 1 || dm[0].Address != "alice@example.com" {
		t.Errorf("DM recipients = %v; want Alice alone", dm)
	}

	to, cc, _, err := resolveReplyAllRecipients(thread, selvesForTest)
	if err != nil {
		t.Fatalf("reply-all recipients: %v", err)
	}
	if len(to) != 1 || to[0].Address != "alice@example.com" {
		t.Errorf("reply-all To = %v; want Alice", to)
	}
	if len(cc) != 1 || cc[0].Address != "bob@example.com" {
		t.Errorf("reply-all Cc = %v; want Bob, from Alice's message", cc)
	}
}

// A user's own message that arrives cold -- after a restart, or once the
// thread has aged out of the cache -- is threaded onto a skeleton carrying no
// reply context at all. Persisting that snapshot must not erase the stored
// inbound context, which is the only copy left.
func TestMergePortalMetadata_OwnMessageKeepsStoredInboundContext(t *testing.T) {
	t.Parallel()
	date := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	stored := &PortalMetadata{
		ThreadID:             "t1",
		LastFrom:             "Alice <alice@example.com>",
		LastTo:               []string{"me@example.com"},
		LastCc:               []string{"bob@example.com"},
		LastInboundMessageID: "in-1@example.com",
		LastDate:             date,
		LastTextBody:         "Can you send the numbers?",
		LastHTMLBody:         "<p>Can you send the numbers?</p>",
	}
	ownMessage := &PortalMetadata{
		ThreadID:              "t1",
		LastMessageID:         "out-1@example.com",
		LastOutboundMessageID: "out-1@example.com",
		References:            []string{"out-1@example.com"},
	}

	got := mergePortalMetadata(stored, ownMessage)

	if got.LastFrom != "Alice <alice@example.com>" || got.LastInboundMessageID != "in-1@example.com" {
		t.Errorf("reply context = from %q (%q); want Alice's inbound kept", got.LastFrom, got.LastInboundMessageID)
	}
	if len(got.LastTo) != 1 || len(got.LastCc) != 1 {
		t.Errorf("recipients = to %v cc %v; want the stored inbound's", got.LastTo, got.LastCc)
	}
	if !got.LastDate.Equal(date) || got.LastTextBody == "" || got.LastHTMLBody == "" {
		t.Errorf("quote context = %v %q %q; want the stored inbound's", got.LastDate, got.LastTextBody, got.LastHTMLBody)
	}
	if got.LastOutboundMessageID != "out-1@example.com" || got.LastMessageID != "out-1@example.com" {
		t.Errorf("own message = last %q, outbound %q; want out-1", got.LastMessageID, got.LastOutboundMessageID)
	}
}

// The in-memory half of the case above: the cached skeleton has no inbound
// context, while the portal row does. A reply typed in Matrix reads the cached
// thread, so it must be completed from the row rather than addressed from the
// user's own message's recipients.
func TestResolveThreadForPortal_FillsMissingInboundContextFromMetadata(t *testing.T) {
	t.Parallel()
	tm := email.NewThreadManager(nil)
	cached := &email.EmailThread{
		ThreadID:              "t1",
		MessageID:             "out-1@example.com",
		LastOutboundMessageID: "out-1@example.com",
		Participants:          []string{"me@example.com", "alice@example.com"},
	}
	tm.CacheForReceiver("login-1", cached)

	ec := &EmailClient{
		Main:      &EmailConnector{ThreadManager: tm},
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login-1"}},
	}
	portal := &bridgev2.Portal{Portal: &database.Portal{
		PortalKey: networkid.PortalKey{ID: "thread:t1"},
		Metadata: &PortalMetadata{
			ThreadID:             "t1",
			LastFrom:             "Alice <alice@example.com>",
			LastTo:               []string{"me@example.com"},
			LastCc:               []string{"bob@example.com"},
			LastInboundMessageID: "in-1@example.com",
			LastDeliveredTo:      "me@example.com",
			LastTextBody:         "Can you send the numbers?",
		},
	}}

	thread, err := ec.resolveThreadForPortalWithMetadata(portal)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if thread.LastFrom != "Alice <alice@example.com>" || thread.LastInboundMessageID != "in-1@example.com" {
		t.Errorf("reply context = from %q (%q); want Alice's inbound from the portal row", thread.LastFrom, thread.LastInboundMessageID)
	}
	if len(thread.LastCc) != 1 || thread.LastDeliveredTo != "me@example.com" || thread.LastTextBody == "" {
		t.Errorf("cc %v, delivered-to %q, body %q; want the stored inbound's", thread.LastCc, thread.LastDeliveredTo, thread.LastTextBody)
	}
	if thread.LastOutboundMessageID != "out-1@example.com" || thread.MessageID != "out-1@example.com" {
		t.Errorf("cached own message lost: last %q, outbound %q", thread.MessageID, thread.LastOutboundMessageID)
	}
}

// Sending reads the stored row on every message now, not only on a cache
// miss, while the inbound pollers replace that row through PersistThreadState.
// Portal.Metadata is an interface value with no lock of its own; an unguarded
// read beside a write is a data race. Only observable under -race, which CI
// runs.
func TestResolveThreadForPortal_ReadsMetadataUnderThePersistLock(t *testing.T) {
	br := newTestBridge(t)
	ctx := context.Background()
	key := networkid.PortalKey{ID: "thread:race-1", Receiver: "login-race"}
	if err := br.DB.Portal.Insert(ctx, &database.Portal{BridgeID: testBridgeID, PortalKey: key}); err != nil {
		t.Fatalf("insert portal: %v", err)
	}
	dbPortal, err := br.DB.Portal.GetByKey(ctx, key)
	if err != nil || dbPortal == nil {
		t.Fatalf("load portal: %v", err)
	}
	portal := &bridgev2.Portal{Portal: dbPortal, Bridge: br}

	tm := email.NewThreadManager(nil)
	tm.CacheForReceiver("login-race", &email.EmailThread{ThreadID: "race-1"})
	ec := &EmailClient{
		Main:      &EmailConnector{ThreadManager: tm},
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login-race"}},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			inbound := &email.EmailThread{ThreadID: "race-1", LastFrom: "alice@example.com", LastInboundMessageID: fmt.Sprintf("in-%d@example.com", i)}
			if err := PersistThreadState(ctx, portal, inbound); err != nil {
				t.Errorf("persist: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if _, err := ec.resolveThreadForPortalWithMetadata(portal); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	<-done
}
