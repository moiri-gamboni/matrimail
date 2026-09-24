package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/Leicas/matrimail/pkg/email"
)

// BeeperSyncQuery stores, per Gmail thread of a login, the archived and
// unread state Gmail and Beeper last agreed on. A side whose state differs
// from it has changed since; after the bridge applies a change, both sides
// equal it again, which is what keeps the change from coming back.
type BeeperSyncQuery struct {
	DB *database.Database
}

// beeperSyncRow is one thread's stored state. GmailChanged is set when the
// history poller saw the thread's labels move since the last sync, which the
// next sync reads as a reason to fetch the thread from Gmail.
type beeperSyncRow struct {
	State        email.ThreadState
	GmailChanged bool
}

// CreateTable creates matrimail_beeper_sync. Idempotent; run on every start.
func (q *BeeperSyncQuery) CreateTable(ctx context.Context) error {
	_, err := q.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS matrimail_beeper_sync (
			login_id        TEXT    NOT NULL,
			gmail_thread_id TEXT    NOT NULL,
			archived        BOOLEAN NOT NULL,
			unread          BOOLEAN NOT NULL,
			gmail_changed   BOOLEAN NOT NULL DEFAULT FALSE,
			PRIMARY KEY (login_id, gmail_thread_id)
		)
	`)
	if err != nil {
		return fmt.Errorf("create matrimail_beeper_sync: %w", err)
	}
	return nil
}

// Get returns the thread's stored state, or nil if it was never synced.
func (q *BeeperSyncQuery) Get(ctx context.Context, loginID, gmailThreadID string) (*beeperSyncRow, error) {
	rows, err := q.DB.Query(ctx, dialectQuery(q.DB.Dialect, `
		SELECT archived, unread, gmail_changed FROM matrimail_beeper_sync
		WHERE login_id = ? AND gmail_thread_id = ?
	`), loginID, gmailThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var row beeperSyncRow
	if err := rows.Scan(&row.State.Archived, &row.State.Unread, &row.GmailChanged); err != nil {
		return nil, err
	}
	return &row, nil
}

// Put records the state both sides now hold, clearing GmailChanged.
func (q *BeeperSyncQuery) Put(ctx context.Context, loginID, gmailThreadID string, s email.ThreadState) error {
	_, err := q.DB.Exec(ctx, dialectQuery(q.DB.Dialect, `
		INSERT INTO matrimail_beeper_sync (login_id, gmail_thread_id, archived, unread, gmail_changed)
		VALUES (?, ?, ?, ?, FALSE)
		ON CONFLICT (login_id, gmail_thread_id) DO UPDATE SET
			archived = EXCLUDED.archived, unread = EXCLUDED.unread, gmail_changed = FALSE
	`), loginID, gmailThreadID, s.Archived, s.Unread)
	return err
}

// MarkGmailChanged flags the thread if it has a stored state; a thread never
// synced is read from Gmail on its first sync anyway.
func (q *BeeperSyncQuery) MarkGmailChanged(ctx context.Context, loginID, gmailThreadID string) error {
	_, err := q.DB.Exec(ctx, dialectQuery(q.DB.Dialect, `
		UPDATE matrimail_beeper_sync SET gmail_changed = TRUE
		WHERE login_id = ? AND gmail_thread_id = ?
	`), loginID, gmailThreadID)
	return err
}
