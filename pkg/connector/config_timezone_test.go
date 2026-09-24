package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrimail/pkg/email"
)

func TestValidateConfig_Timezone(t *testing.T) {
	ec := &EmailConnector{Config: Config{Timezone: "Europe/Lisbon"}}
	if err := ec.ValidateConfig(); err != nil {
		t.Fatal(err)
	}
	if got := ec.Config.location.String(); got != "Europe/Lisbon" {
		t.Errorf("location = %q, want Europe/Lisbon", got)
	}

	ec = &EmailConnector{}
	if err := ec.ValidateConfig(); err != nil {
		t.Fatal(err)
	}
	if ec.Config.location != time.Local {
		t.Errorf("empty timezone: location = %v, want the process's local zone", ec.Config.location)
	}

	ec = &EmailConnector{Config: Config{Timezone: "Mars/Olympus_Mons"}}
	err := ec.ValidateConfig()
	if err == nil || !strings.Contains(err.Error(), "network.timezone") {
		t.Errorf("bad timezone: err = %v, want an error naming network.timezone", err)
	}
}

func TestBuildOutgoingMessage_AttributionInConfiguredZone(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	thread := &email.EmailThread{
		Subject:      "Plans",
		LastFrom:     "Alex Example <alex@example.com>",
		LastDate:     time.Date(2026, 9, 24, 12, 34, 0, 0, time.UTC),
		LastTextBody: "reply 2",
		LastHTMLBody: "<div>reply 2</div>",
	}
	msg := &bridgev2.MatrixMessage{}
	msg.Content = &event.MessageEventContent{MsgType: event.MsgText, Body: "reply 3"}
	om := buildOutgoingMessage("me@example.com", "Me", thread, msg, "<parent@example.com>", nil, nil, nil, lisbon)
	want := "On Thu, Sep 24, 2026 at 1:34 PM"
	if !strings.Contains(om.TextBody, want) {
		t.Errorf("text body = %q, want %q in it", om.TextBody, want)
	}
	if !strings.Contains(om.HTMLBody, want) {
		t.Errorf("html body = %q, want %q in it", om.HTMLBody, want)
	}
}
