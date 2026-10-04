package email

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/rs/zerolog"
	"strings"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func (g *GmailAPISender) React(ctx context.Context, threadID, messageID, own, emoji string) (string, error) {
	if g.tokenSource == nil {
		return "", errors.New("Gmail reactions require an authenticated Gmail account")
	}
	svc, err := gmail.NewService(ctx, option.WithTokenSource(g.tokenSource))
	if err != nil {
		return "", err
	}
	return sendGmailReaction(ctx, svc, threadID, messageID, own, emoji)
}

func sendGmailReaction(ctx context.Context, svc *gmail.Service, threadID, messageID, own, emoji string) (string, error) {
	if threadID == "" || messageID == "" {
		return "", errors.New("reaction target has no Gmail thread or Message-ID")
	}
	thread, err := svc.Users.Threads.Get("me", threadID).Format("metadata").MetadataHeaders("Message-ID", "From", "To", "Cc", "Reply-To", "List-Id", "Subject").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("fetch reaction target: %w", err)
	}
	var target *gmail.Message
	for _, message := range thread.Messages {
		if message.Payload != nil && cleanMessageID(headerValue(message.Payload.Headers, "Message-ID")) == cleanMessageID(messageID) {
			target = message
			break
		}
	}
	if target == nil {
		return "", errors.New("reaction target was not found in its Gmail thread")
	}
	raw, err := BuildGmailReaction(target, own, emoji)
	if err != nil {
		return "", err
	}
	// A reaction must stay in its target thread: never retry without ThreadId.
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{ThreadId: threadID, Raw: base64.RawURLEncoding.EncodeToString(raw)}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("send Gmail reaction: %w", err)
	}
	metadata, err := svc.Users.Messages.Get("me", sent.Id).Format("metadata").MetadataHeaders("Message-ID").Context(ctx).Do()
	if err != nil {
		// The send succeeded. Treating the dedup lookup as failure would invite another email.
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Gmail reaction sent but echo suppression Message-ID lookup failed")
		return "", nil
	}
	if metadata.Payload == nil {
		return "", nil
	}
	return strings.Trim(headerValue(metadata.Payload.Headers, "Message-ID"), "<>"), nil
}
