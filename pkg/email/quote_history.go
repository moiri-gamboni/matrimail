package email

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Leicas/matrimail/pkg/common"
)

// historySeen reports whether the message a reply directly answers was bridged
// to this account, i.e. whether the reader has already seen what the room view
// would fold away.
//
// Folding quoted history assumes the reader saw it in an earlier message of
// the room. A forward breaks that assumption: when a colleague forwards a mail
// the account never received, the forwarded block is the only copy, and folding
// it leaves just the line written on top ("any action item?") with nothing to
// say what it is about. So does a reply whose parent went to others only, as
// when someone replies-all to a reply the account was left off: older messages
// of the thread may be in the room, but the parent quoted on top is not.
//
// The parent is In-Reply-To, or the last References entry when In-Reply-To is
// empty. Both the prefixed network ID and the bare Message-ID are tried, as the
// thread resolver does, because rows written before the prefix existed hold the
// bare form.
func historySeen(ctx context.Context, db *database.Database, receiver networkid.UserLoginID, inReplyTo string, references []string) (bool, error) {
	parent := strings.Trim(strings.TrimSpace(inReplyTo), "<>")
	if parent == "" && len(references) > 0 {
		parent = strings.Trim(strings.TrimSpace(references[len(references)-1]), "<>")
	}
	if parent == "" {
		return false, nil
	}
	for _, rid := range []networkid.MessageID{common.EmailToMessageID(parent), networkid.MessageID(parent)} {
		msg, err := db.Message.GetFirstPartByID(ctx, receiver, rid)
		if err != nil {
			return false, fmt.Errorf("look up parent %q: %w", rid, err)
		}
		if msg != nil {
			return true, nil
		}
	}
	return false, nil
}
