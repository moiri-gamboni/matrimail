package connector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/Leicas/matrimail/pkg/beeper"
	"github.com/Leicas/matrimail/pkg/email"
)

// beeperSyncer mirrors archived and unread state between one Gmail login's
// threads and their chats in Beeper.
//
// Beeper keeps a chat's archived state in room account data and its unread
// state in read receipts and a marked-unread flag, none of which a bridge is
// sent, so the Beeper side is read from the Beeper Desktop API that Beeper
// Server (or Beeper Desktop) serves. The Gmail side is read from the thread's
// labels, fetched when the history poller reports the thread's INBOX or
// UNREAD labels moved, when the chat changed, or on a thread's first sync.
//
// Each thread's last agreed state is stored (BeeperSyncQuery). On a tick,
// every field one side changed is copied to the other, and Gmail's value wins
// a field both changed. For a yes/no field that tie-break never decides
// anything: two sides that both moved away from the same stored value agree.
// Where the preference does matter is a thread's first sync, with no stored
// state to say which side moved: Gmail's state is taken, because the chats of
// an imported mailbox start unread and in the inbox whatever the mail's state.
type beeperSyncer struct {
	loginID string
	beeper  *beeper.Client
	gmail   *email.GmailThreads
	store   *BeeperSyncQuery
	// threadForChat returns the Gmail thread a chat mirrors, or "" when it
	// mirrors none of this login's.
	threadForChat func(ctx context.Context, chatID string) (string, error)
	log           *zerolog.Logger

	// mu serialises syncing one thread with recording Gmail changes, so a
	// change reported while a thread is being synced is not cleared by the
	// sync's write.
	mu sync.Mutex

	// accountID is the login's Beeper account, looked up on the first tick.
	accountID string
	// listFailing and chatsFailing record whether the last tick could not
	// list the chats, or could list them but not sync them all. Each kind of
	// failure is logged when it starts and when it ends, not on every tick,
	// and apart, so a chat that keeps failing does not hide an outage.
	listFailing  bool
	chatsFailing bool
}

// run syncs every interval until ctx ends, first straight away.
func (s *beeperSyncer) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.runTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runTick runs one tick and logs each kind of failure as it starts and ends.
// A failure is retried on the next tick rather than stopping the sync: the
// likely causes (Beeper Server restarting, a replaced token, a Gmail hiccup)
// pass on their own, and mail bridging does not depend on this.
func (s *beeperSyncer) runTick(ctx context.Context) {
	chats, err := s.listChats(ctx)
	s.listFailing = s.logEpisode(s.listFailing, err, "Beeper state sync cannot list chats")
	if err != nil {
		return
	}
	s.chatsFailing = s.logEpisode(s.chatsFailing, s.syncChats(ctx, chats), "Beeper state sync failed for some chats")
}

// logEpisode logs err when a failure starts and a line when it ends, and
// returns whether it is failing now.
func (s *beeperSyncer) logEpisode(wasFailing bool, err error, what string) bool {
	switch {
	case err != nil && !wasFailing:
		s.log.Warn().Err(err).Msg(what + "; retrying every interval, logging again when it recovers")
	case err == nil && wasFailing:
		s.log.Info().Msg(what + ": recovered")
	}
	return err != nil
}

func (s *beeperSyncer) listChats(ctx context.Context) ([]beeper.Chat, error) {
	if s.accountID == "" {
		accountID, err := s.beeper.AccountID(ctx, s.loginID)
		if err != nil {
			return nil, err
		}
		s.accountID = accountID
	}
	return s.beeper.ListChats(ctx, s.accountID)
}

// syncChats syncs each chat. A chat that fails is skipped so the rest still
// sync; the first failure is returned.
func (s *beeperSyncer) syncChats(ctx context.Context, chats []beeper.Chat) error {
	var failed int
	var firstErr error
	for _, chat := range chats {
		if err := s.syncChat(ctx, chat); err != nil {
			s.log.Debug().Err(err).Str("chat_id", chat.ID).Msg("Beeper state sync: chat not synced")
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("chat %s: %w", chat.ID, err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d chats not synced, first: %w", failed, len(chats), firstErr)
	}
	return nil
}

func (s *beeperSyncer) syncChat(ctx context.Context, chat beeper.Chat) error {
	threadID, err := s.threadForChat(ctx, chat.ID)
	if err != nil || threadID == "" {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := s.store.Get(ctx, s.loginID, threadID)
	if err != nil {
		return err
	}
	inBeeper := email.ThreadState{Archived: chat.IsArchived, Unread: chat.Unread()}
	if stored != nil && !stored.GmailChanged && inBeeper == stored.State {
		return nil
	}

	// Read Gmail before writing anything, so a Gmail change the poller has
	// not reported yet is seen rather than overwritten.
	thread, err := s.gmail.Get(ctx, threadID)
	if email.IsGmailNotFound(err) {
		// Deleted from Gmail: there is nothing left to mirror. Recording the
		// chat as it stands stops it being fetched again until it changes.
		return s.store.Put(ctx, s.loginID, threadID, inBeeper)
	}
	if err != nil {
		return err
	}
	target := thread.State
	if stored != nil {
		target = mergeThreadState(stored.State, inBeeper, thread.State)
	}

	if err := s.gmail.Apply(ctx, thread, target); err != nil {
		return err
	}
	if err := s.applyToBeeper(ctx, chat.ID, inBeeper, target); err != nil {
		return err
	}
	return s.store.Put(ctx, s.loginID, threadID, target)
}

// mergeThreadState takes, field by field, the side that moved away from the
// stored state, Gmail's if both did.
func mergeThreadState(stored, inBeeper, inGmail email.ThreadState) email.ThreadState {
	pick := func(stored, beeper, gmail bool) bool {
		if gmail != stored {
			return gmail
		}
		return beeper
	}
	return email.ThreadState{
		Archived: pick(stored.Archived, inBeeper.Archived, inGmail.Archived),
		Unread:   pick(stored.Unread, inBeeper.Unread, inGmail.Unread),
	}
}

// applyToBeeper changes the chat to the target state and reads it back: a
// write's response describes the chat before the write, and the state must
// not be recorded as synced unless Beeper holds it.
func (s *beeperSyncer) applyToBeeper(ctx context.Context, chatID string, current, target email.ThreadState) error {
	if current == target {
		return nil
	}
	if current.Archived != target.Archived {
		if err := s.beeper.SetArchived(ctx, chatID, target.Archived); err != nil {
			return err
		}
	}
	if current.Unread != target.Unread {
		if err := s.beeper.SetUnread(ctx, chatID, target.Unread); err != nil {
			return err
		}
	}
	chat, err := s.beeper.GetChat(ctx, chatID)
	if err != nil {
		return err
	}
	if got := (email.ThreadState{Archived: chat.IsArchived, Unread: chat.Unread()}); got != target {
		return fmt.Errorf("beeper shows %+v after the change, want %+v", got, target)
	}
	return nil
}

// gmailThreadsChanged takes the history poller's report of threads whose
// INBOX or UNREAD labels moved; the next tick reads them from Gmail.
func (s *beeperSyncer) gmailThreadsChanged(ctx context.Context, threadIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, threadID := range threadIDs {
		if err := s.store.MarkGmailChanged(ctx, s.loginID, threadID); err != nil {
			return err
		}
	}
	return nil
}

// gmailThreadForChat maps a chat to the Gmail thread its portal holds. The
// chat ID is the portal's Matrix room ID. Chats of another login, and rooms
// with no Gmail thread yet (a compose draft before its first send), map to "".
func gmailThreadForChat(db *database.Database, loginID string) func(ctx context.Context, chatID string) (string, error) {
	return func(ctx context.Context, chatID string) (string, error) {
		portal, err := db.Portal.GetByMXID(ctx, id.RoomID(chatID))
		if err != nil || portal == nil || portal.Receiver != networkid.UserLoginID(loginID) {
			return "", err
		}
		pm, _ := portal.Metadata.(*PortalMetadata)
		if pm == nil {
			return "", nil
		}
		return pm.GmailThreadID, nil
	}
}
