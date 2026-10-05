package email

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"

	msgtextproto "github.com/emersion/go-message/textproto"
	"github.com/rs/zerolog"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func (g *GmailAPISender) React(ctx context.Context, threadID, messageID string, own mail.Address, emoji string, record func(string)) (string, error) {
	if g.tokenSource == nil {
		return "", errors.New("Gmail reactions require an authenticated Gmail account")
	}
	svc, err := gmail.NewService(ctx, option.WithTokenSource(g.tokenSource))
	if err != nil {
		return "", err
	}
	return sendGmailReaction(ctx, svc, threadID, messageID, own, emoji, record)
}

func sendGmailReaction(ctx context.Context, svc *gmail.Service, threadID, messageID string, own mail.Address, emoji string, record func(string)) (string, error) {
	if threadID == "" || messageID == "" {
		return "", errors.New("reaction target has no Gmail thread or Message-ID")
	}
	thread, err := svc.Users.Threads.Get("me", threadID).Format("full").Context(ctx).Do()
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
	log := zerolog.Ctx(ctx)
	log.Debug().Str("step", "send").Str("thread_id", threadID).Msg("Gmail reaction request")
	// A reaction must stay in its target thread: never retry without ThreadId.
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{ThreadId: threadID, Raw: base64.RawURLEncoding.EncodeToString(raw)}).Context(ctx).Do()
	logReactionMetadata(ctx, "send", sent, err)
	if err != nil {
		return "", fmt.Errorf("send Gmail reaction: %w", err)
	}
	log.Debug().Str("step", "metadata").Str("gmail_message_id", sent.Id).Msg("Gmail reaction request")
	metadata, err := svc.Users.Messages.Get("me", sent.Id).Format("metadata").MetadataHeaders("Message-ID").Context(ctx).Do()
	logReactionMetadata(ctx, "metadata", metadata, err)
	originalID := ""
	if err != nil {
		// Delivery succeeded; failing repair must never invite another reaction email.
		log.Error().Err(err).Str("step", "metadata").Str("gmail_message_id", sent.Id).Msg("Gmail reaction echo lookup failed")
	} else if metadata.Payload != nil {
		originalID = cleanMessageID(headerValue(metadata.Payload.Headers, "Message-ID"))
		if originalID != "" && record != nil {
			record(originalID)
		}
	}
	_, domain, _ := strings.Cut(own.Address, "@")
	copyID := cleanMessageID(GenerateMessageID(domain))
	copyRaw, err := reactionCopyWithMessageID(raw, copyID)
	if err != nil {
		log.Error().Err(err).Str("step", "copy").Str("gmail_message_id", sent.Id).Msg("Gmail reaction own-view repair failed")
		return originalID, nil
	}
	log.Debug().Str("step", "trash").Str("gmail_message_id", sent.Id).Msg("Gmail reaction request")
	trashed, err := svc.Users.Messages.Trash("me", sent.Id).Context(ctx).Do()
	logReactionMetadata(ctx, "trash", trashed, err)
	if err != nil {
		log.Error().Err(err).Str("step", "trash").Str("gmail_message_id", sent.Id).Str("copy_message_id", copyID).Msg("Gmail reaction own-view repair failed")
		return originalID, nil
	}
	// Record before import so the history poller cannot observe an unsuppressed copy.
	if record != nil {
		record(copyID)
	}
	log.Debug().Str("step", "import").Str("original_gmail_message_id", sent.Id).Str("message_id", copyID).Strs("label_ids", []string{"SENT"}).Bool("never_mark_spam", true).Str("internal_date_source", "dateHeader").Msg("Gmail reaction request")
	imported, err := svc.Users.Messages.Import("me", &gmail.Message{LabelIds: []string{"SENT"}, Raw: base64.RawURLEncoding.EncodeToString(copyRaw)}).NeverMarkSpam(true).InternalDateSource("dateHeader").Context(ctx).Do()
	logReactionMetadata(ctx, "import", imported, err)
	if err != nil {
		log.Error().Err(err).Str("step", "import").Str("gmail_message_id", sent.Id).Str("copy_message_id", copyID).Msg("Gmail reaction own-view repair failed")
	}
	return originalID, nil
}

func logReactionMetadata(ctx context.Context, step string, message *gmail.Message, err error) {
	entry := zerolog.Ctx(ctx).Debug().Str("step", step).Err(err)
	if message != nil {
		entry.Str("gmail_message_id", message.Id).Str("thread_id", message.ThreadId).Strs("label_ids", message.LabelIds)
	}
	entry.Msg("Gmail reaction response")
}

func reactionCopyWithMessageID(raw []byte, messageID string) ([]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	headers, err := msgtextproto.ReadHeader(reader)
	if err != nil {
		return nil, err
	}
	headers.Set("Message-ID", "<"+messageID+">")
	var result bytes.Buffer
	if err := msgtextproto.WriteHeader(&result, headers); err != nil {
		return nil, err
	}
	if _, err := io.Copy(&result, reader); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}
