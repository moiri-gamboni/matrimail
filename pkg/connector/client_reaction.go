package connector

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/Leicas/matrimail/pkg/common"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type gmailReactor interface {
	React(context.Context, string, string, mail.Address, string) (string, error)
}

var _ bridgev2.ReactionHandlingNetworkAPI = (*EmailClient)(nil)

func (ec *EmailClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	// Matrix retries use the original key, so EmojiID must retain its variation selectors.
	emoji := msg.Content.RelatesTo.Key
	return bridgev2.MatrixReactionPreResponse{SenderID: MakeUserID(ec.Email), EmojiID: networkid.EmojiID(emoji), Emoji: variationselector.FullyQualify(emoji)}, nil
}

func (ec *EmailClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	sentID, err := ec.sendReaction(ctx, msg)
	if err != nil {
		if noticeErr := postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Reaction refused", err.Error()); noticeErr != nil {
			return nil, fmt.Errorf("%w (could not post refusal notice: %v)", err, noticeErr)
		}
		return nil, err
	}
	if sentID != "" && ec.Main.SentDedup != nil {
		if err := ec.Main.SentDedup.Record(ctx, string(ec.UserLogin.ID), sentID, string(msg.Event.ID)); err != nil {
			ec.UserLogin.Log.Warn().Err(err).Msg("Reaction sent but echo suppression could not be recorded")
		}
	}
	return &database.Reaction{Emoji: variationselector.FullyQualify(msg.Content.RelatesTo.Key)}, nil
}

func (ec *EmailClient) sendReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (string, error) {
	reactor, ok := ec.Sender.(gmailReactor)
	if !ok {
		return "", errors.New("Email reactions require Gmail OAuth")
	}

	existing, err := ec.UserLogin.Bridge.DB.Reaction.GetByIDWithoutMessagePart(ctx, ec.UserLogin.ID, msg.TargetMessage.ID, MakeUserID(ec.Email), networkid.EmojiID(msg.Content.RelatesTo.Key))
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", nil
	}
	thread, err := ec.resolveThreadForPortalWithMetadata(msg.Portal)
	if err != nil {
		return "", err
	}
	selves := map[string]bool{strings.ToLower(ec.Email): true}
	for _, alias := range ec.SendAsAliases {
		selves[strings.ToLower(alias.Email)] = true
	}
	fromAddr, fromName := pickFromAddress(ec, thread, selves)
	return reactor.React(ctx, thread.GmailThreadID, common.MessageIDFromNetworkID(msg.TargetMessage.ID), mail.Address{Address: fromAddr, Name: fromName}, variationselector.FullyQualify(msg.Content.RelatesTo.Key))
}

func (ec *EmailClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	err := errors.New("Sent Gmail reactions cannot be removed by the bridge; archive the chat to clear an email silently")
	if noticeErr := postErrorToPortal(ctx, ec.UserLogin.Bridge, msg.Portal, "Reaction removal refused", err.Error()); noticeErr != nil {
		return noticeErr
	}
	return err
}
