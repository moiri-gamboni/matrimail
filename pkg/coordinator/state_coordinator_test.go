package coordinator

import (
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/status"
)

// A transport with no IMAP socket (the Gmail API poller) is connected and
// receiving from the moment its poller runs. Reporting that as one event
// matters: two events in a row computed TRANSIENT_DISCONNECT in between, and
// the framework's resend of the last delivered state on a websocket reconnect
// could deliver that stale state after CONNECTED and leave it standing.
func TestPollerReadyAloneMeansConnected(t *testing.T) {
	log := zerolog.Nop()
	sc := NewStateCoordinator(nil, &log)

	sc.ReportSimpleEvent("inbox", string(EventPollerReady), true, "", nil)

	state, errCode := sc.computeBridgeState()
	if state != status.StateConnected || errCode != "" {
		t.Fatalf("after poller_ready: state %q error %q, want CONNECTED with no error", state, errCode)
	}
}
