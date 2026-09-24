package email

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	gmail "google.golang.org/api/gmail/v1"
)

// GmailHistoryPoller polls a single Gmail account via the Gmail API for new
// messages. Runs as a long-lived goroutine spawned from the connector's
// LoadUserLogin; one poller per modify-mode account.
//
// Strategy:
//
//   - On first run with no persisted historyId, call users.getProfile to get
//     the current historyId and persist it as the cursor — we only ingest
//     forward from "now", not the entire mailbox history.
//   - Every PollInterval, call users.history.list(startHistoryId=cursor,
//     historyTypes=[messageAdded,labelAdded], labelId=L) once per monitored
//     label L -- the parameter takes a single label. For each new messageId,
//     fetch via users.messages.get(format=full) and hand the gmail.Message to
//     the per-message callback (which builds a ParsedEmail and feeds the
//     processor). labelAdded is required so that post-arrival tagging (e.g. a
//     separate Gmail filter or n8n workflow that applies the monitored label
//     after delivery) is still surfaced to Matrix.
//   - Persist the new historyId after each successful tick.
//
// Errors during a tick are logged and the cursor stays put — the next tick
// retries from the same cursor. Refresh-token failures (invalid_grant) bubble
// out of the TokenSource and are caught by the wrapping reauthAwareTokenSource
// in the connector layer; the poller just sees the error, logs, and the next
// poll either succeeds (transient) or hits the same error (the connector's
// re-auth path will handle it).
type GmailHistoryPoller struct {
	// Identity / persistence keys. Used by the cursor save/load callbacks.
	UserMXID string
	Email    string

	// MonitoredLabelIDs are the Gmail label IDs the user picked at login time
	// (e.g. "INBOX", "SENT", "Label_5"), not display names. Each is listed
	// through users.history.list separately. Required.
	MonitoredLabelIDs []string

	// PollInterval is how long to wait between polls. Default 30s if unset.
	// Below 5s the server may rate-limit and the bridge wastes API quota.
	PollInterval time.Duration

	// TokenSource is the refresh-aware OAuth source. The connector wraps it
	// with reauthAwareTokenSource so refresh failures fire the re-auth UX.
	TokenSource oauth2.TokenSource

	// CursorLoad / CursorSave persist the lastHistoryId across restarts. The
	// connector wires these to its DB.GetGmailHistoryID / SetGmailHistoryID.
	// Both are required.
	CursorLoad func(ctx context.Context) (uint64, error)
	CursorSave func(ctx context.Context, historyID uint64) error

	// OnMessage is called for each newly-discovered message, after a
	// users.messages.get(format=full) round-trip. Errors are logged but don't
	// stop the poller. Required.
	OnMessage func(ctx context.Context, msg *gmail.Message, mailbox string) error

	Log *zerolog.Logger

	// clientOptions are appended when building the Gmail service; tests use
	// them to point the poller at a fake server.
	clientOptions []option.ClientOption

	// Internal state.
	stopOnce sync.Once
	stopCh   chan struct{}
}

// Run blocks until ctx is cancelled or Stop() is called. Spawn it in a
// goroutine from the connector. Returns the first non-recoverable error
// encountered (e.g. ctx cancellation), or nil on graceful shutdown.
func (g *GmailHistoryPoller) Run(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	g.stopCh = make(chan struct{})

	if g.PollInterval <= 0 {
		g.PollInterval = 30 * time.Second
	}

	logger := g.Log.With().Str("component", "gmail_history_poller").Str("email", g.Email).Logger()

	// Bootstrap: ensure we have a historyId cursor.
	cursor, err := g.bootstrapCursor(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap cursor: %w", err)
	}
	logger.Info().Uint64("history_id", cursor).Msg("Gmail history poller starting")

	ticker := time.NewTicker(g.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Gmail history poller stopping (context cancelled)")
			return ctx.Err()
		case <-g.stopCh:
			logger.Info().Msg("Gmail history poller stopping (Stop called)")
			return nil
		case <-ticker.C:
			newCursor, perr := g.pollOnce(ctx, cursor, &logger)
			if perr != nil {
				// invalid_grant style: bubble up so the caller's wrapping
				// TokenSource can fire the re-auth UX. The connector wraps
				// our TokenSource with reauthAwareTokenSource, so the
				// bridge-level reauth path is wired automatically.
				logger.Warn().Err(perr).Msg("Gmail history poll tick failed; will retry next interval")
				continue
			}
			if newCursor != cursor {
				if err := g.CursorSave(ctx, newCursor); err != nil {
					logger.Warn().Err(err).Uint64("history_id", newCursor).Msg("Failed to persist Gmail historyId cursor")
				} else {
					cursor = newCursor
				}
			}
		}
	}
}

// Stop signals the poller to exit on its next iteration. Safe to call from
// any goroutine and idempotent.
func (g *GmailHistoryPoller) Stop() {
	g.stopOnce.Do(func() {
		if g.stopCh != nil {
			close(g.stopCh)
		}
	})
}

// validate returns an error for any missing required field.
func (g *GmailHistoryPoller) validate() error {
	if g.Email == "" {
		return errors.New("GmailHistoryPoller: empty Email")
	}
	if len(g.MonitoredLabelIDs) == 0 {
		return errors.New("GmailHistoryPoller: no monitored labels")
	}
	if g.TokenSource == nil {
		return errors.New("GmailHistoryPoller: nil TokenSource")
	}
	if g.CursorLoad == nil || g.CursorSave == nil {
		return errors.New("GmailHistoryPoller: missing CursorLoad/CursorSave callbacks")
	}
	if g.OnMessage == nil {
		return errors.New("GmailHistoryPoller: nil OnMessage callback")
	}
	if g.Log == nil {
		return errors.New("GmailHistoryPoller: nil Log")
	}
	return nil
}

// bootstrapCursor returns the persisted historyId, or — if there isn't one —
// fetches the current historyId from users.getProfile and persists it before
// returning. This makes a fresh login start ingesting from "now" rather than
// trying to backfill the entire mailbox.
func (g *GmailHistoryPoller) bootstrapCursor(ctx context.Context) (uint64, error) {
	cursor, err := g.CursorLoad(ctx)
	if err != nil {
		return 0, fmt.Errorf("load cursor: %w", err)
	}
	if cursor != 0 {
		return cursor, nil
	}
	svc, err := g.gmailService(ctx)
	if err != nil {
		return 0, err
	}
	prof, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("getProfile: %w", err)
	}
	cursor = prof.HistoryId
	if err := g.CursorSave(ctx, cursor); err != nil {
		return 0, fmt.Errorf("save initial cursor: %w", err)
	}
	return cursor, nil
}

// pollOnce lists history since cursor for every monitored label, feeds each
// newly surfaced message to OnMessage once, in history order, and returns the
// cursor to persist.
//
// users.history.list filters on a single labelId, so each monitored label is
// listed separately and a message under two of them (mail to yourself carries
// both INBOX and SENT) is reported twice; it is fetched and fed once. The
// calls run one after another while the mailbox keeps moving, so the returned
// cursor is the lowest of the per-label ones: a message arriving between two
// calls is read again on the next tick, where the bridge drops a message ID it
// has already bridged, rather than skipped.
func (g *GmailHistoryPoller) pollOnce(ctx context.Context, cursor uint64, logger *zerolog.Logger) (uint64, error) {
	svc, err := g.gmailService(ctx)
	if err != nil {
		return cursor, err
	}

	var records []*gmail.History
	var newCursor uint64
	for i, lblID := range g.MonitoredLabelIDs {
		labelRecords, labelCursor, err := listHistory(ctx, svc, cursor, lblID)
		if err != nil {
			// historyId expired (Gmail keeps history for 7-30 days). When that
			// happens, refresh the cursor to current via getProfile and skip the
			// gap — we can't recover the missed messages without a full scan.
			// Retention is mailbox-wide, so expiry fails the first label; a
			// 404 after an earlier label succeeded from the same cursor has
			// another cause, and resetting on it would skip mail every tick.
			if i == 0 && isHistoryExpiredError(err) {
				logger.Warn().Err(err).Uint64("cursor", cursor).
					Msg("Gmail historyId cursor expired; resetting to current and skipping gap")
				prof, perr := svc.Users.GetProfile("me").Context(ctx).Do()
				if perr != nil {
					return cursor, fmt.Errorf("getProfile after history-expired: %w", perr)
				}
				return prof.HistoryId, nil
			}
			return cursor, err
		}
		records = append(records, labelRecords...)
		if newCursor == 0 || labelCursor < newCursor {
			newCursor = labelCursor
		}
	}

	msgIDs := newMessageIDs(records, g.MonitoredLabelIDs)
	if len(msgIDs) == 0 {
		return newCursor, nil
	}
	logger.Debug().Int("new_messages", len(msgIDs)).Uint64("from", cursor).Uint64("to", newCursor).
		Msg("Gmail history poll: fetching new messages")

	for _, msgID := range msgIDs {
		full, err := svc.Users.Messages.Get("me", msgID).Format("full").Context(ctx).Do()
		if err != nil {
			logger.Warn().Err(err).Str("gmail_msg_id", msgID).Msg("Failed to fetch Gmail message; skipping")
			continue
		}
		g.feed(ctx, full, logger)
	}
	return newCursor, nil
}

// listHistory drains users.history.list for one label, returning its records
// and the highest historyId the responses reported, which is where this
// label's history can resume.
func listHistory(ctx context.Context, svc *gmail.Service, cursor uint64, labelID string) ([]*gmail.History, uint64, error) {
	var records []*gmail.History
	labelCursor := cursor
	pageToken := ""
	for {
		call := svc.Users.History.List("me").
			StartHistoryId(cursor).
			HistoryTypes("messageAdded", "labelAdded").
			LabelId(labelID).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, cursor, fmt.Errorf("history.list label=%s: %w", labelID, err)
		}
		records = append(records, resp.History...)
		if resp.HistoryId > labelCursor {
			labelCursor = resp.HistoryId
		}
		if resp.NextPageToken == "" {
			return records, labelCursor, nil
		}
		pageToken = resp.NextPageToken
	}
}

// newMessageIDs returns the messages the history records surface, each once,
// in the order the records happened. The API returns each label's history in
// chronological order of record ID; records from several labels are merged by
// that ID.
//
// Two record types count:
//
//   - messagesAdded: the message arrived with a monitored label already on it
//     (delivery to INBOX, a send filed in SENT, a filter applied on arrival).
//   - labelsAdded: a monitored label was applied after delivery (a filter or
//     workflow tagging it later, a manual move). Only when the added labels
//     include a monitored one: the labelId filter matches any message that
//     currently has a monitored label, so it also reports unrelated labels
//     added to a message already bridged.
func newMessageIDs(records []*gmail.History, monitored []string) []string {
	sorted := make([]*gmail.History, 0, len(records))
	for _, h := range records {
		if h != nil {
			sorted = append(sorted, h)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Id < sorted[j].Id })

	var ids []string
	seen := map[string]bool{}
	add := func(m *gmail.Message) {
		if m == nil || m.Id == "" || seen[m.Id] {
			return
		}
		seen[m.Id] = true
		ids = append(ids, m.Id)
	}
	for _, h := range sorted {
		for _, ma := range h.MessagesAdded {
			if ma != nil {
				add(ma.Message)
			}
		}
		for _, la := range h.LabelsAdded {
			if la != nil && anyLabelMatches(la.LabelIds, monitored) {
				add(la.Message)
			}
		}
	}
	return ids
}

// feed hands a fetched message to OnMessage under the mailbox its own labels
// put it in, and reports whether it was accepted. The labels come from the
// fetched message because the history.list reference says the messages in
// history records typically carry only an id and a threadId.
//
// Drafts are skipped: they have no stable Message-ID and would appear in
// Matrix as messages from a third-party ghost. They become visible once sent,
// when Gmail removes DRAFT.
func (g *GmailHistoryPoller) feed(ctx context.Context, msg *gmail.Message, logger *zerolog.Logger) bool {
	if hasLabel(msg.LabelIds, "DRAFT") {
		logger.Debug().Str("gmail_msg_id", msg.Id).Msg("Skipping draft message (DRAFT label)")
		return false
	}
	if err := g.OnMessage(ctx, msg, primaryLabel(msg.LabelIds, g.MonitoredLabelIDs)); err != nil {
		logger.Warn().Err(err).Str("gmail_msg_id", msg.Id).Msg("OnMessage callback failed; continuing")
		return false
	}
	return true
}

// Backlog scans the past lookback window for messages bearing any of the
// monitored labels and feeds them through OnMessage, bypassing the history
// cursor entirely. Useful when the history cursor has advanced past
// labelAdded events that the old (pre-fix) poller dropped — those messages
// are otherwise unrecoverable without a manual scan.
//
// Messages are fed oldest first by Gmail's internalDate, because the last one
// fed becomes its room's newest event; messages.list documents no order. A
// message under several monitored labels is fed once.
//
// Returns the number of messages fed to OnMessage. Errors from individual
// message fetches are logged and counted as skipped, not returned.
// The caller controls the lookback range; Gmail's `newer_than:` query operator
// accepts d/m/y units — we use days. A lookback of 0 is rejected (callers
// should pass an explicit window so an accidental empty value doesn't trigger
// a full-mailbox scan).
func (g *GmailHistoryPoller) Backlog(ctx context.Context, lookbackDays int, logger *zerolog.Logger) (int, error) {
	if lookbackDays <= 0 {
		return 0, fmt.Errorf("backlog: lookbackDays must be positive, got %d", lookbackDays)
	}
	if len(g.MonitoredLabelIDs) == 0 {
		return 0, fmt.Errorf("backlog: no monitored labels configured")
	}
	svc, err := g.gmailService(ctx)
	if err != nil {
		return 0, err
	}

	query := fmt.Sprintf("newer_than:%dd", lookbackDays)
	// Gmail's messages.list `labelIds` parameter is an AND filter, so we
	// must scan once per monitored label and dedup client-side.
	var ids []string
	seen := map[string]bool{}
	for _, lblID := range g.MonitoredLabelIDs {
		pageToken := ""
		for {
			list := svc.Users.Messages.List("me").
				LabelIds(lblID).
				Q(query).
				MaxResults(100).
				Context(ctx)
			if pageToken != "" {
				list = list.PageToken(pageToken)
			}
			resp, lerr := list.Do()
			if lerr != nil {
				return 0, fmt.Errorf("backlog: messages.list label=%s: %w", lblID, lerr)
			}
			for _, m := range resp.Messages {
				if m == nil || m.Id == "" || seen[m.Id] {
					continue
				}
				seen[m.Id] = true
				ids = append(ids, m.Id)
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
	}

	if len(ids) == 0 {
		return 0, nil
	}
	logger.Info().Int("candidates", len(ids)).Int("lookback_days", lookbackDays).
		Strs("labels", g.MonitoredLabelIDs).Msg("Gmail backlog scan starting")

	msgs := make([]*gmail.Message, 0, len(ids))
	for _, msgID := range ids {
		full, ferr := svc.Users.Messages.Get("me", msgID).Format("full").Context(ctx).Do()
		if ferr != nil {
			logger.Warn().Err(ferr).Str("gmail_msg_id", msgID).Msg("Backlog: failed to fetch message; skipping")
			continue
		}
		msgs = append(msgs, full)
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].InternalDate < msgs[j].InternalDate })

	fed := 0
	for _, msg := range msgs {
		if g.feed(ctx, msg, logger) {
			fed++
		}
	}
	logger.Info().Int("fed", fed).Int("candidates", len(ids)).Msg("Gmail backlog scan complete")
	return fed, nil
}

// gmailService constructs a fresh gmail.Service per call. The underlying
// http client is owned by the service object; constructing each call is
// trivial vs the round-trip it's about to do.
func (g *GmailHistoryPoller) gmailService(ctx context.Context) (*gmail.Service, error) {
	opts := append([]option.ClientOption{option.WithTokenSource(g.TokenSource)}, g.clientOptions...)
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail.NewService: %w", err)
	}
	return svc, nil
}

// isHistoryExpiredError reports whether err is Gmail's "historyId is too
// old" 404. Per Google's docs the API returns 404 with reason "notFound" when
// the supplied startHistoryId is older than the server-side retention window
// (7 days for new accounts, longer for active ones).
func isHistoryExpiredError(err error) bool {
	if err == nil {
		return false
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if gerr.Code == 404 {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "historyId") && strings.Contains(strings.ToLower(msg), "not found")
}

// hasLabel reports whether labels contains target (case-insensitive). Used to
// detect Gmail system labels like DRAFT, SENT, INBOX without depending on
// label-ID order.
func hasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, target) {
			return true
		}
	}
	return false
}

// anyLabelMatches reports whether any element of a equals any element of b
// (case-insensitive). Used to test whether a labelsAdded event added a
// monitored label, vs adding some unrelated label to an already-monitored
// message.
func anyLabelMatches(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

// primaryLabel names the mailbox a message is passed to OnMessage under,
// which the processor reads to decide whether the message is the user's own.
// SENT wins over everything, whatever order Gmail lists the labels in, so a
// message carrying both INBOX and SENT (mail to yourself) counts as sent; then
// INBOX; then the first monitored label the message carries; then the first
// monitored label, of which the poller always has at least one.
func primaryLabel(messageLabels, monitored []string) string {
	for _, want := range []string{"SENT", "INBOX"} {
		if hasLabel(messageLabels, want) {
			return want
		}
	}
	for _, lbl := range messageLabels {
		for _, mon := range monitored {
			if strings.EqualFold(lbl, mon) {
				return lbl
			}
		}
	}
	return monitored[0]
}
