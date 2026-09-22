package connector

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Leicas/matrimail/pkg/common"
)

// DBThreadMetadataResolver resolves an email Message-ID to the thread whose
// room already holds that message, by asking the bridge database which portal a
// previously bridged message landed in.
//
// This is what keeps a conversation in one room across a restart. The in-memory
// ThreadManager evicts after 24h, and an inbound whose parent is no longer
// cached would otherwise be treated as the start of a new thread and open a
// second room for a conversation the user is already reading.
//
// It only resolves messages this bridge has seen before. A reply whose parent
// was never bridged falls through to the header heuristics, as it must.
//
// Returned thread IDs have the "thread:" portal-key prefix stripped.
type DBThreadMetadataResolver struct {
	Bridge *bridgev2.Bridge
	Log    *zerolog.Logger
}

// ResolveThreadID returns the thread ID for an email Message-ID, or false.
//
// This used to hand-roll SQL against guessed table and column names -- a UNION
// over `message` and `messages` selecting `portal_id` filtered on `network`,
// `remote_id` and `receiver`, none of which exist. bridgev2's schema names the
// columns `bridge_id`, `id`, `room_id` and `room_receiver`, and there is no
// plural table, so every arm failed on an unknown column and the errors were
// discarded as "schema didn't match, best effort". The resolver therefore
// answered "no match" to every query ever made of it, and because a miss is a
// legitimate answer here, nothing calling it could tell the difference: the
// symptom was the restart behaviour above, with no error anywhere.
//
// Going through the framework's own typed query instead of any SQL is what
// stops that recurring -- the schema can now only drift under us in a way the
// compiler sees.
func (r *DBThreadMetadataResolver) ResolveThreadID(receiver, messageID string) (string, bool) {
	if r == nil || r.Bridge == nil || r.Bridge.DB == nil {
		return "", false
	}
	mid := strings.TrimSpace(messageID)
	if mid == "" {
		return "", false
	}
	// Short timeout: falling back to the header heuristics is much better than
	// holding up delivery behind a slow query.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Built through the same constructor the inbound and outbound paths store
	// under, so the lookup key cannot drift away from the stored one. The bare
	// ID is tried second for rows written before that prefix existed.
	for _, rid := range []networkid.MessageID{common.EmailToMessageID(mid), networkid.MessageID(mid)} {
		msg, err := r.Bridge.DB.Message.GetFirstPartByID(ctx, networkid.UserLoginID(receiver), rid)
		if err != nil {
			// A real failure, not a miss. Reported rather than swallowed: the
			// consequence is silent (threads quietly start landing in new
			// rooms), so the log line is the only way anyone finds out.
			if r.Log != nil {
				r.Log.Warn().Err(err).
					Str("receiver", receiver).
					Msg("thread resolver query failed; this thread may open a second room instead of continuing in its own")
			}
			return "", false
		}
		if msg != nil {
			tid := normalizeThreadID(string(msg.Room.ID))
			if tid == "" {
				continue
			}
			if r.Log != nil {
				r.Log.Debug().
					Str("receiver", receiver).
					Str("remote_id", string(rid)).
					Str("thread_id", tid).
					Msg("thread resolver: matched a previously bridged message")
			}
			return tid, true
		}
	}
	return "", false
}

func normalizeThreadID(portalOrThreadID string) string {
	return strings.TrimPrefix(strings.TrimSpace(portalOrThreadID), "thread:")
}
