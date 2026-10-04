package email

import (
	"encoding/json"
	gmail "google.golang.org/api/gmail/v1"
	"os"
	"testing"
)

func TestGmailPreservesRawDeliveredTo(t *testing.T) {
	data, err := os.ReadFile("testdata/gmail_message_attachments.json")
	if err != nil {
		t.Fatal(err)
	}
	var input gmail.Message
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	input.Payload.Headers = append(input.Payload.Headers, &gmail.MessagePartHeader{Name: "Delivered-To", Value: "Me <ME@Example.com>"})
	parsed, err := ParseGmailAPIMessage(&input, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := convertForTest(t, parsed, "!room:example.com", newFakeIntent(t), 1)
	body := partByID(parts, "body")
	if body == nil || body.DBMetadata == nil {
		t.Fatal("body has no addressing metadata")
	}
	data, err = json.Marshal(body.DBMetadata)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["from"] != "alex@example.com" {
		t.Fatalf("from=%v", fields["from"])
	}
	if got := fields["delivered_to"].([]any); len(got) != 1 || got[0] != "me@example.com" {
		t.Fatalf("delivered_to=%v", got)
	}
	for _, part := range parts {
		if part.ID != "body" && part.DBMetadata != nil {
			t.Fatalf("metadata on attachment %s", part.ID)
		}
	}
}

func TestEmptyMessageMetadataRemainsUnknown(t *testing.T) {
	raw, err := json.Marshal(&MessageMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("old messages and attachment parts acquired addressing: %s", raw)
	}
}

func TestMessageAddressingIsIndependentOfLaterMail(t *testing.T) {
	parsed := &ParsedEmail{From: "Alice <ALICE@Example.com>", To: []string{"Me <ME@Example.com>"}, TextContent: "first"}
	body := partByID(convertForTest(t, parsed, "!r:example.com", newFakeIntent(t), 1), "body")
	before, err := json.Marshal(body.DBMetadata)
	if err != nil {
		t.Fatal(err)
	}
	parsed.To[0] = "other@example.com"
	later := &ParsedEmail{From: "bob@example.com", To: []string{"other@example.com"}, TextContent: "second"}
	convertForTest(t, later, "!r:example.com", newFakeIntent(t), 1)
	after, err := json.Marshal(body.DBMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || string(before) != `{"from":"alice@example.com","to":["me@example.com"],"cc":[],"delivered_to":[]}` {
		t.Fatalf("before=%s after=%s", before, after)
	}
}
