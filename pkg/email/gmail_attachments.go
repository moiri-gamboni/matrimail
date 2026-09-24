package email

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	gmail "google.golang.org/api/gmail/v1"
)

// GmailAttachments downloads attachment bodies with
// users.messages.attachments.get, which the gmail.modify scope allows.
type GmailAttachments struct {
	TokenSource oauth2.TokenSource
	Log         *zerolog.Logger

	clientOptions []option.ClientOption
}

// NewGmailAttachments builds the client; opts are added to the Gmail
// service's options, after the token source.
func NewGmailAttachments(ts oauth2.TokenSource, log *zerolog.Logger, opts ...option.ClientOption) *GmailAttachments {
	return &GmailAttachments{TokenSource: ts, Log: log, clientOptions: opts}
}

// fetchTimeout bounds one download. It runs in the room's event queue, so a
// stalled connection would otherwise hold up every later message of the
// thread.
const fetchTimeout = 2 * time.Minute

// Fetch returns the decoded bytes of one attachment of one message.
func (g *GmailAttachments) Fetch(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	opts := append([]option.ClientOption{option.WithTokenSource(g.TokenSource)}, g.clientOptions...)
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail.NewService: %w", err)
	}
	g.Log.Debug().Str("gmail_msg_id", messageID).Str("attachment_id", attachmentID).Msg("Gmail messages.attachments.get request")
	body, err := svc.Users.Messages.Attachments.Get("me", messageID, attachmentID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("attachments.get %s/%s: %w", messageID, attachmentID, err)
	}
	// The data is the attachment itself, mail content, so only its shape is
	// logged.
	g.Log.Debug().Str("gmail_msg_id", messageID).Str("attachment_id", attachmentID).
		Int64("size", body.Size).Int("data_len", len(body.Data)).Msg("Gmail messages.attachments.get response")
	data, err := decodeGmailData(body.Data)
	if err != nil {
		return nil, fmt.Errorf("attachments.get %s/%s: decode data: %w", messageID, attachmentID, err)
	}
	return data, nil
}

// decodeGmailData decodes a MessagePartBody's data, base64url with or
// without padding.
func decodeGmailData(s string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(padBase64URL(s))
}
