package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func TestGmailReactionCreatesOwnViewAfterDelivery(t *testing.T) {
	for _, failure := range []string{"", "trash", "import"} {
		t.Run(failure, func(t *testing.T) {
			target := reactionTarget(t)
			var steps []string
			var sentBody []byte
			var copyID string
			recorded := map[string]bool{}
			logBytes := &bytes.Buffer{}
			log := zerolog.New(logBytes)
			svcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/threads/"):
					json.NewEncoder(w).Encode(&gmail.Thread{Messages: []*gmail.Message{target}})
				case strings.HasSuffix(r.URL.Path, "/messages/send"):
					steps = append(steps, "send")
					var message gmail.Message
					json.NewDecoder(r.Body).Decode(&message)
					raw, err := base64.RawURLEncoding.DecodeString(message.Raw)
					if err != nil {
						t.Error(err)
					}
					parsed, err := mail.ReadMessage(bytes.NewReader(raw))
					if err != nil {
						t.Error(err)
						return
					}
					sentBody, _ = io.ReadAll(parsed.Body)
					io.WriteString(w, `{"id":"sent","threadId":"thread","labelIds":["SENT"]}`)
				case strings.HasSuffix(r.URL.Path, "/messages/sent"):
					steps = append(steps, "metadata")
					io.WriteString(w, `{"id":"sent","payload":{"headers":[{"name":"Message-ID","value":"<original@example.com>"}]}}`)
				case strings.HasSuffix(r.URL.Path, "/messages/sent/trash"):
					steps = append(steps, "trash")
					if !recorded["original@example.com"] {
						t.Error("original was not recorded before trash")
					}
					if failure == "trash" {
						w.WriteHeader(403)
						io.WriteString(w, `{"error":{"code":403,"message":"trash denied"}}`)
						return
					}
					io.WriteString(w, `{"id":"sent","labelIds":["TRASH","SENT"]}`)
				case strings.HasSuffix(r.URL.Path, "/messages/import"):
					steps = append(steps, "import")
					var message gmail.Message
					json.NewDecoder(r.Body).Decode(&message)
					if message.ThreadId != "" || len(message.LabelIds) != 1 || message.LabelIds[0] != "SENT" {
						t.Errorf("import metadata=%+v", message)
					}
					if r.URL.Query().Get("neverMarkSpam") != "true" || r.URL.Query().Get("internalDateSource") != "dateHeader" {
						t.Errorf("import query=%s", r.URL.RawQuery)
					}
					raw, err := base64.RawURLEncoding.DecodeString(message.Raw)
					if err != nil {
						t.Error(err)
					}
					parsed, err := mail.ReadMessage(bytes.NewReader(raw))
					if err != nil {
						t.Error(err)
						return
					}
					copyID = strings.Trim(parsed.Header.Get("Message-ID"), "<>")
					if !recorded[copyID] {
						t.Error("copy was not recorded before import")
					}
					if copyID == "" || copyID == "original@example.com" {
						t.Errorf("copy Message-ID=%q", copyID)
					}
					if _, err := mail.ParseDate(parsed.Header.Get("Date")); err != nil {
						t.Error("copy has no valid Date")
					}
					body, _ := io.ReadAll(parsed.Body)
					if !bytes.Equal(body, sentBody) {
						t.Error("import changed reaction body")
					}
					if failure == "import" {
						w.WriteHeader(403)
						io.WriteString(w, `{"error":{"code":403,"message":"import denied"}}`)
						return
					}
					io.WriteString(w, `{"id":"copy","threadId":"thread","labelIds":["SENT"]}`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer svcServer.Close()
			svc, err := gmail.NewService(context.Background(), option.WithEndpoint(svcServer.URL+"/"), option.WithHTTPClient(svcServer.Client()))
			if err != nil {
				t.Fatal(err)
			}
			mid, err := sendGmailReaction(log.WithContext(context.Background()), svc, target.ThreadId, cleanMessageID(headerValue(target.Payload.Headers, "Message-ID")), mail.Address{Address: "sam@example.com"}, "👍", func(mid string) { recorded[mid] = true })
			if err != nil || mid != "original@example.com" {
				t.Errorf("recipient delivery failed: id=%q err=%v", mid, err)
			}
			want := "send,metadata,trash,import"
			if failure == "trash" {
				want = "send,metadata,trash"
			}
			if strings.Join(steps, ",") != want {
				t.Errorf("steps=%v want %s", steps, want)
			}
			if failure != "" && (!strings.Contains(logBytes.String(), `"level":"error"`) || !strings.Contains(logBytes.String(), `"step":"`+failure+`"`)) {
				t.Errorf("no step error: %s", logBytes)
			}
		})
	}
}
