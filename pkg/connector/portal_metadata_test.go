package connector

import (
	"testing"

	"github.com/Leicas/matrimail/pkg/email"
)

// The stored snapshot is what a reply is addressed from after a restart, so it
// has to carry the reply context of the newest message -- not the state as of
// the user's last send. Persisting on send alone was how the recipient
// inheritance bug came back: a room whose newest event is inbound rehydrated
// from a copy written before that message arrived.
func TestPortalMetadataFromThread_CarriesReplyContext(t *testing.T) {
	t.Parallel()
	thread := &email.EmailThread{
		ThreadID:              "t1",
		Subject:               "Grant reporting",
		LastFrom:              "alice@example.com",
		LastTo:                []string{"moiri@apartresearch.com"},
		LastCc:                nil, // the funder was dropped by the newest message
		LastInboundMessageID:  "msg-2@example.com",
		LastOutboundMessageID: "mine-1@example.com",
		LastTextBody:          "Between us, the numbers are not going to work.",
	}

	pm := PortalMetadataFromThread(thread)
	if pm == nil {
		t.Fatal("nil metadata")
	}
	if len(pm.LastCc) != 0 {
		t.Errorf("LastCc = %v; a dropped Cc must not survive into the snapshot", pm.LastCc)
	}
	if pm.LastInboundMessageID != "msg-2@example.com" {
		t.Errorf("LastInboundMessageID = %q; the guard depends on this", pm.LastInboundMessageID)
	}
	if pm.LastOutboundMessageID != "mine-1@example.com" {
		t.Errorf("LastOutboundMessageID = %q; the guard depends on this too", pm.LastOutboundMessageID)
	}
	if pm.LastTextBody != thread.LastTextBody {
		t.Errorf("quoted body not carried: %q", pm.LastTextBody)
	}
}

func TestPortalMetadataFromThread_NilThread(t *testing.T) {
	t.Parallel()
	if pm := PortalMetadataFromThread(nil); pm != nil {
		t.Fatalf("expected nil for a nil thread, got %+v", pm)
	}
}
