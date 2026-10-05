package connector

import (
	"context"
	"fmt"
	"github.com/Leicas/matrimail/pkg/email"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"strings"
	"testing"
	"time"
)

type reactionSender struct {
	noopSender
	calls                      int
	thread, target, own, emoji string
}

func (s *reactionSender) React(ctx context.Context, thread, target, own, emoji string) (string, error) {
	s.calls++
	s.thread = thread
	s.target = target
	s.own = own
	s.emoji = emoji
	return "reaction@example.com", nil
}

func TestGmailReactionCapability(t *testing.T) {
	client := &EmailClient{Sender: &reactionSender{}}
	if client.GetCapabilities(context.Background(), nil).Reaction < event.CapLevelPartialSupport {
		t.Fatal("Gmail reactions are rejected before the handler")
	}
}

func TestMatrixReactionTargetsTheSelectedEmail(t *testing.T) {
	sender := &reactionSender{}
	tm := email.NewThreadManager(nil)
	tm.CacheForReceiver("email:me@example.com", &email.EmailThread{ThreadID: "root", GmailThreadID: "gmail", MessageID: "latest@example.com"})
	client := &EmailClient{Main: &EmailConnector{ThreadManager: tm}, UserLogin: &bridgev2.UserLogin{Bridge: &bridgev2.Bridge{DB: newTestDB(t)}, UserLogin: &database.UserLogin{ID: "email:me@example.com"}}, Email: "me@example.com", Sender: sender}
	msg := &bridgev2.MatrixReaction{TargetMessage: &database.Message{ID: "email:selected@example.com"}}
	msg.Portal = &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "thread:root", Receiver: "email:me@example.com"}}}
	msg.Content = &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: "✅"}}
	pre, err := client.PreHandleMatrixReaction(context.Background(), msg)
	if err != nil || pre.EmojiID != "✅" || pre.SenderID != "email:me@example.com" {
		t.Fatalf("pre=%+v err=%v", pre, err)
	}
	if _, err := client.HandleMatrixReaction(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 || sender.thread != "gmail" || sender.target != "selected@example.com" || sender.own != "me@example.com" || sender.emoji != "✅" {
		t.Fatalf("reaction=%+v", sender)
	}
}

type reactionNoticeBot struct {
	bridgev2.MatrixAPI
	room id.RoomID
	body string
}

func (b *reactionNoticeBot) SendMessage(ctx context.Context, room id.RoomID, typ event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	b.room = room
	b.body = content.AsMessage().Body
	return &mautrix.RespSendEvent{EventID: "$notice"}, nil
}

func TestReactionRefusalPostsNoticeInTargetChat(t *testing.T) {
	bot := &reactionNoticeBot{}
	client := &EmailClient{UserLogin: &bridgev2.UserLogin{Bridge: &bridgev2.Bridge{Bot: bot}}, Sender: noopSender{}}
	msg := &bridgev2.MatrixReaction{}
	msg.Portal = &bridgev2.Portal{Portal: &database.Portal{MXID: "!target:example.com"}}
	if _, err := client.HandleMatrixReaction(context.Background(), msg); err == nil {
		t.Fatal("non-Gmail reaction accepted")
	}
	if bot.room != "!target:example.com" || !strings.Contains(bot.body, "Reaction refused") {
		t.Fatalf("notice to %s: %s", bot.room, bot.body)
	}
}

func TestSameEmojiOnEmailPartsSendsOneReaction(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	br := &bridgev2.Bridge{DB: db}
	key := networkid.PortalKey{ID: "thread:root", Receiver: "email:me@example.com"}
	if err := db.Portal.Insert(ctx, &database.Portal{BridgeID: testBridgeID, PortalKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := db.Ghost.Insert(ctx, &database.Ghost{BridgeID: testBridgeID, ID: "email:me@example.com"}); err != nil {
		t.Fatal(err)
	}
	sender := &reactionSender{}
	tm := email.NewThreadManager(nil)
	tm.CacheForReceiver(string(key.Receiver), &email.EmailThread{ThreadID: "root", GmailThreadID: "gmail"})
	client := &EmailClient{Main: &EmailConnector{ThreadManager: tm}, UserLogin: &bridgev2.UserLogin{Bridge: br, UserLogin: &database.UserLogin{ID: key.Receiver}}, Email: "me@example.com", Sender: sender}
	for _, part := range []networkid.PartID{"body", "attachment-1"} {
		target := &database.Message{BridgeID: testBridgeID, ID: "email:selected@example.com", PartID: part, Room: key, MXID: id.EventID("$" + part), SenderID: "email:me@example.com"}
		if err := db.Message.Insert(ctx, target); err != nil {
			t.Fatal(err)
		}
		msg := &bridgev2.MatrixReaction{TargetMessage: target}
		msg.Portal = &bridgev2.Portal{Portal: &database.Portal{PortalKey: key}}
		msg.Content = &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: "✅"}}
		if _, err := client.HandleMatrixReaction(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if err := db.Reaction.Upsert(ctx, &database.Reaction{BridgeID: testBridgeID, Room: key, MessageID: target.ID, MessagePartID: part, SenderID: "email:me@example.com", EmojiID: "✅", MXID: id.EventID("$reaction-" + part)}); err != nil {
			t.Fatal(err)
		}
	}
	if sender.calls != 1 {
		t.Fatalf("sent %d reactions for one emoji on one email", sender.calls)
	}
}

func TestActionsInSplitRoomPreserveGmailRouting(t *testing.T) {
	for _, action := range []string{"reaction", "reply"} {
		for _, warm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/warm=%v", action, warm), func(t *testing.T) {
				ctx := context.Background()
				db := newTestDB(t)
				log := zerolog.Nop()
				br := &bridgev2.Bridge{DB: db, Log: log}
				receiver := networkid.UserLoginID("email:me@example.com")
				if err := db.Ghost.Insert(ctx, &database.Ghost{BridgeID: testBridgeID, ID: "email:alice@example.com"}); err != nil {
					t.Fatal(err)
				}
				var split *bridgev2.Portal
				for i, room := range []string{"primary", "split"} {
					key := networkid.PortalKey{ID: networkid.PortalID("thread:" + room), Receiver: receiver}
					portal := &database.Portal{BridgeID: testBridgeID, PortalKey: key, MXID: id.RoomID("!" + room + ":example.com"), Metadata: &PortalMetadata{ThreadID: room, GmailThreadID: "gmail", Participants: []string{"me@example.com", "alice@example.com"}, LastMessageID: "target@example.com", LastInboundMessageID: "target@example.com", LastFrom: "alice@example.com"}}
					if err := db.Portal.Insert(ctx, portal); err != nil {
						t.Fatal(err)
					}
					if err := db.Message.Insert(ctx, &database.Message{BridgeID: testBridgeID, Room: key, ID: networkid.MessageID(room + "@example.com"), PartID: "body", MXID: id.EventID("$" + room), SenderID: "email:alice@example.com", Timestamp: time.Unix(int64(100+i), 0)}); err != nil {
						t.Fatal(err)
					}
					if room == "split" {
						split = &bridgev2.Portal{Bridge: br, Portal: portal}
					}
				}
				tm := email.NewThreadManager(&DBThreadMetadataResolver{Bridge: br, Log: &log})
				if warm {
					tm.CacheForReceiver(string(receiver), &email.EmailThread{ThreadID: "primary", GmailThreadID: "gmail"})
				}
				client := &EmailClient{Main: &EmailConnector{Bridge: br, ThreadManager: tm}, UserLogin: &bridgev2.UserLogin{Bridge: br, Log: log, UserLogin: &database.UserLogin{ID: receiver}}, Email: "me@example.com", Sender: &reactionSender{}}
				if action == "reaction" {
					msg := &bridgev2.MatrixReaction{TargetMessage: &database.Message{ID: "email:target@example.com"}}
					msg.Portal = split
					msg.Content = &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: "✅"}}
					if _, err := client.HandleMatrixReaction(ctx, msg); err != nil {
						t.Fatal(err)
					}
				} else {
					msg := &bridgev2.MatrixMessage{}
					msg.Portal = split
					msg.Content = &event.MessageEventContent{MsgType: event.MsgText, Body: "fixture reply"}
					if _, err := client.handleMatrixMessageOutbound(ctx, msg); err != nil {
						t.Fatal(err)
					}
				}
				thread, err := tm.DetermineThread(string(receiver), &email.ParsedEmail{GmailThreadID: "gmail", MessageID: "next@example.com", From: "alice@example.com"})
				if err != nil {
					t.Fatal(err)
				}
				if thread.ThreadID != "primary" {
					t.Fatalf("inbound Gmail mail routed to %s after %s in split room", thread.ThreadID, action)
				}
			})
		}
	}
}
