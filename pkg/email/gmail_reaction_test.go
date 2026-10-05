package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"slices"
	"strings"
	"testing"
)

func reactionTarget(t *testing.T) *gmail.Message {
	t.Helper()
	data, err := os.ReadFile("testdata/gmail_message_attachments.json")
	if err != nil {
		t.Fatal(err)
	}
	var msg gmail.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	return &msg
}

func TestGmailReactionMIME(t *testing.T) {
	target := reactionTarget(t)
	raw, err := BuildGmailReaction(target, mail.Address{Address: "sam@example.com"}, "👍🏽")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("In-Reply-To") != "<"+cleanMessageID(headerValue(target.Payload.Headers, "Message-ID"))+">" {
		t.Fatal("wrong reaction target")
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatal("not multipart/alternative")
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for _, want := range []string{"text/plain", "text/vnd.google.email-reaction+json", "text/html"} {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		got, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if got != want || part.Header.Get("Content-Disposition") == "attachment" {
			t.Fatalf("part=%s", got)
		}
		data, _ := io.ReadAll(part)
		if want == "text/vnd.google.email-reaction+json" {
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if body["version"] != float64(1) || body["emoji"] != "👍🏽" || len(body) != 2 {
				t.Fatalf("reaction=%v", body)
			}
		}
	}
	if _, err := reader.NextPart(); err != io.EOF {
		t.Fatal("extra MIME part")
	}
}

func TestGmailReactionRefusals(t *testing.T) {
	for _, tc := range []struct{ name, header, value string }{
		{"bcc", "To", "team@example.com"},
		{"list", "List-Id", "team.example.com"},
		{"custom reply", "Reply-To", "other@example.com"},
		{"many recipients", "Cc", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := reactionTarget(t)
			headers := target.Payload.Headers[:0]
			for _, h := range target.Payload.Headers {
				if h.Name != tc.header {
					headers = append(headers, h)
				}
			}
			value := tc.value
			if tc.name == "many recipients" {
				for i := 0; i < 21; i++ {
					value += string(rune('a'+i)) + "@example.com,"
				}
				value = value[:len(value)-1]
			}
			target.Payload.Headers = append(headers, &gmail.MessagePartHeader{Name: tc.header, Value: value})
			if _, err := BuildGmailReaction(target, mail.Address{Address: "sam@example.com"}, "✅"); err == nil {
				t.Fatal("unsafe reaction accepted")
			}
		})
	}
}

func TestGmailReactionRequiresOneEmoji(t *testing.T) {
	for _, value := range []string{"👍", "✅", "✅\ufe0f", "👨‍👩‍👧‍👦", "🇨🇭", "1️⃣", "#️⃣", "👍🏽", "❤", "❤\ufe0f", "🫨"} {
		if _, err := BuildGmailReaction(reactionTarget(t), mail.Address{Address: "sam@example.com"}, value); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"", "ok", "hello", "👍👍", "✅✅", "👍🏽✅", "\u200d", "🇨", "\ufe0f", "1", ":thumbsup:", "mxc://example.org/custom"} {
		if _, err := BuildGmailReaction(reactionTarget(t), mail.Address{Address: "sam@example.com"}, value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestGmailReactionSendsOnceInTargetThread(t *testing.T) {
	target := reactionTarget(t)
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/threads/"+target.ThreadId):
			if r.URL.Query().Get("format") != "metadata" {
				t.Error("target fetched without metadata format")
			}
			for _, name := range []string{"To", "Cc", "Reply-To", "List-Id", "From", "Message-ID", "References"} {
				if !slices.Contains(r.URL.Query()["metadataHeaders"], name) {
					t.Errorf("missing requested header %s", name)
				}
			}
			json.NewEncoder(w).Encode(&gmail.Thread{Id: target.ThreadId, Messages: []*gmail.Message{target}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages/send"):
			sends++
			var sent gmail.Message
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			if sent.ThreadId != target.ThreadId {
				t.Error("wrong outgoing Gmail thread")
			}
			raw, err := base64.RawURLEncoding.DecodeString(sent.Raw)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if msg.Header.Get("In-Reply-To") != "<"+cleanMessageID(headerValue(target.Payload.Headers, "Message-ID"))+">" {
				t.Error("wrong outgoing reaction target")
			}
			io.WriteString(w, `{"id":"sent-reaction"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/messages/sent-reaction"):
			io.WriteString(w, `{"payload":{"headers":[{"name":"Message-ID","value":"<reaction@example.com>"}]}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	svc, err := gmail.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	mid, err := sendGmailReaction(context.Background(), svc, target.ThreadId, cleanMessageID(headerValue(target.Payload.Headers, "Message-ID")), mail.Address{Address: "sam@example.com"}, "✅")
	if err != nil || mid != "reaction@example.com" || sends != 1 {
		t.Fatalf("mid=%q sends=%d err=%v", mid, sends, err)
	}
}

func TestGmailReactionEncodesHeadersAndPreservesReferences(t *testing.T) {
	target := reactionTarget(t)
	for _, h := range target.Payload.Headers {
		switch h.Name {
		case "Subject":
			h.Value = "Réunion à Zürich"
		case "From":
			h.Value = "Élise <elise@example.com>"
		}
	}
	target.Payload.Headers = append(target.Payload.Headers, &gmail.MessagePartHeader{Name: "References", Value: "<root@example.com> <parent@example.com>"})
	raw, err := BuildGmailReaction(target, mail.Address{Address: "sam@example.com"}, "✅")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := "<root@example.com> <parent@example.com> <" + cleanMessageID(headerValue(target.Payload.Headers, "Message-ID")) + ">"
	if msg.Header.Get("References") != want {
		t.Errorf("references=%q want=%q", msg.Header.Get("References"), want)
	}
	subject, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "Re: Réunion à Zürich" {
		t.Fatalf("subject=%q err=%v", subject, err)
	}
	for _, name := range []string{"Subject", "From", "To"} {
		for _, r := range msg.Header.Get(name) {
			if r > 127 {
				t.Errorf("%s contains unencoded non-ASCII", name)
				break
			}
		}
	}
	addresses, err := msg.Header.AddressList("To")
	if err != nil || len(addresses) != 1 || addresses[0].Name != "Élise" || addresses[0].Address != "elise@example.com" {
		t.Fatalf("to=%v err=%v", addresses, err)
	}
}

func TestGmailReactionQualifiesEmojiInAllParts(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"✅\ufe0f", "✅"}, {"❤", "❤️"}, {"👩\u200d❤\u200d👩", "👩\u200d❤️\u200d👩"}, {"☝\ufe0f🏽", "☝🏽"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			raw, err := BuildGmailReaction(reactionTarget(t), mail.Address{Address: "sam@example.com"}, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			reader := multipart.NewReader(msg.Body, params["boundary"])
			for _, kind := range []string{"text/plain", "text/vnd.google.email-reaction+json", "text/html"} {
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "text/vnd.google.email-reaction+json" {
					var reaction struct{ Emoji string }
					if err := json.Unmarshal(body, &reaction); err != nil {
						t.Fatal(err)
					}
					if reaction.Emoji != tc.want {
						t.Errorf("payload emoji = %q, want %q", reaction.Emoji, tc.want)
					}
				} else {
					want := "sam@example.com reacted via Gmail: " + tc.want
					if kind == "text/html" {
						want = "<p>" + want + "</p>"
					}
					if string(body) != want {
						t.Errorf("fallback = %q, want %q", body, want)
					}
				}
			}
		})
	}
}

func TestGmailReactionSubjectIsAReply(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"Test", "Re: Test"}, {"rE: Test", "Re: Test"}, {"Re: Re: Test", "Re: Test"}} {
		target := reactionTarget(t)
		for _, h := range target.Payload.Headers {
			if h.Name == "Subject" {
				h.Value = tc.input
			}
		}
		raw, err := BuildGmailReaction(target, mail.Address{Address: "sam@example.com"}, "✅")
		if err != nil {
			t.Fatal(err)
		}
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || subject != tc.want {
			t.Errorf("subject=%q, want %q, err=%v", subject, tc.want, err)
		}
	}
}

func TestGmailReactionUsesNamedFromAndQuotedPrintableFallback(t *testing.T) {
	raw, err := BuildGmailReaction(reactionTarget(t), mail.Address{Name: "Émile & Sam", Address: "sam@example.com"}, "✅\ufe0f")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Émile & Sam" || from[0].Address != "sam@example.com" {
		t.Errorf("from=%v, err=%v", from, err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for _, want := range []string{"Émile & Sam reacted via Gmail: ✅", `{"version":1,"emoji":"✅"}`, "<p>Émile &amp; Sam reacted via Gmail: ✅</p>"} {
		part, err := reader.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Error("part is not quoted-printable")
		}
		body, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Errorf("decoded body = %q, want %q", body, want)
		}
	}
}
