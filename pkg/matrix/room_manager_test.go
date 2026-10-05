package matrix

import (
	"maunium.net/go/mautrix/event"
	"testing"
)

func TestEmailRoomAllowsReactionsButNotMessagesOrRedactions(t *testing.T) {
	power := &event.PowerLevelsEventContent{}
	BuildReadOnlyPowerLevels().Apply("", power)
	human := power.GetUserLevel("@human:example.com")
	if human < power.GetEventLevel(event.EventReaction) {
		t.Error("human cannot react")
	}
	for _, typ := range []event.Type{event.EventMessage, event.EventRedaction} {
		if human >= power.GetEventLevel(typ) {
			t.Errorf("human can send %s", typ)
		}
	}
	if human >= power.Redact() {
		t.Error("human can redact other events")
	}
}
