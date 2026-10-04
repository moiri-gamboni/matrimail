package connector

import (
	"context"
	"github.com/Leicas/matrimail/pkg/email"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"strings"
	"testing"
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
	client := &EmailClient{Main: &EmailConnector{ThreadManager: tm}, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "email:me@example.com"}}, Email: "me@example.com", Sender: sender}
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
