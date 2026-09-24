package connector

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
	gmail "google.golang.org/api/gmail/v1"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/Leicas/matrimail/pkg/beeper"
	"github.com/Leicas/matrimail/pkg/email"
)

// GmailInboundManager owns the per-account GmailHistoryPoller lifecycle for
// scope-mode='modify' accounts. The IMAP path doesn't need this: IMAP IDLE
// connections are managed by pkg/imap. For Gmail-API mode there's no
// long-lived TCP connection to drive — we poll users.history.list every
// PollInterval and feed the results through the same processor pipeline.
//
// One manager instance per EmailConnector; one poller per (UserMXID, Email)
// account pair.
type GmailInboundManager struct {
	connector *EmailConnector

	mu      sync.Mutex
	runners map[string]*gmailRunner // key: UserMXID + "|" + Email
}

type gmailRunner struct {
	cancel context.CancelFunc
	poller *email.GmailHistoryPoller
}

// NewGmailInboundManager constructs the manager. Wired up from
// EmailConnector.Init.
func NewGmailInboundManager(ec *EmailConnector) *GmailInboundManager {
	return &GmailInboundManager{
		connector: ec,
		runners:   map[string]*gmailRunner{},
	}
}

// Start spawns a poller for the given login if one doesn't already exist.
// Idempotent — calling it twice for the same account is a no-op (the second
// call returns nil).
//
// labels are the Gmail label IDs the user picked at login time (the
// monitored_folders column for modify-mode accounts).
func (m *GmailInboundManager) Start(ctx context.Context, login *bridgev2.UserLogin, emailAddr string, labels []string, ts oauth2.TokenSource) error {
	if m == nil {
		return fmt.Errorf("GmailInboundManager: nil manager")
	}
	key := login.UserMXID.String() + "|" + emailAddr

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.runners[key]; exists {
		return nil // already running
	}

	logger := m.connector.Bridge.Log.With().Str("component", "gmail_inbound").Str("email", emailAddr).Logger()
	userMXID := login.UserMXID.String()
	attachments := email.NewGmailAttachments(ts, &logger)

	poller := &email.GmailHistoryPoller{
		UserMXID:          userMXID,
		Email:             emailAddr,
		MonitoredLabelIDs: labels,
		TokenSource:       ts,
		CursorLoad: func(ctx context.Context) (uint64, error) {
			return m.connector.DB.GetGmailHistoryID(ctx, userMXID, emailAddr)
		},
		CursorSave: func(ctx context.Context, historyID uint64) error {
			return m.connector.DB.SetGmailHistoryID(ctx, userMXID, emailAddr, historyID)
		},
		OnMessage: func(ctx context.Context, msg *gmail.Message, mailbox string) error {
			return m.handleMessage(ctx, login, msg, mailbox, attachments.Fetch)
		},
		Log: &logger,
	}

	runnerCtx, cancel := context.WithCancel(context.Background())
	m.runners[key] = &gmailRunner{cancel: cancel, poller: poller}

	if cfg := m.connector.Config.BeeperSync; cfg.APIURL != "" {
		syncLog := m.connector.Bridge.Log.With().Str("component", "beeper_sync").Str("email", emailAddr).Logger()
		syncer := &beeperSyncer{
			loginID:       string(login.ID),
			beeper:        &beeper.Client{BaseURL: cfg.APIURL, TokenFile: cfg.TokenFile, HTTP: &http.Client{Timeout: 30 * time.Second}, Log: &syncLog},
			gmail:         email.NewGmailThreads(ts, &syncLog),
			store:         m.connector.BeeperSync,
			threadForChat: gmailThreadForChat(m.connector.Bridge.DB, string(login.ID)),
			log:           &syncLog,
		}
		poller.OnThreadsChanged = syncer.gmailThreadsChanged
		go syncer.run(runnerCtx, cfg.Interval())
	}

	go func() {
		if err := poller.Run(runnerCtx); err != nil && runnerCtx.Err() == nil {
			logger.Error().Err(err).Msg("Gmail history poller exited with error")
			// Stops the Beeper state sync too, which without the poller would
			// no longer hear of changes made in Gmail.
			cancel()
			// Surface to the user via the management room — they won't see
			// inbound mail until the bridge is restarted or the account is
			// re-loaded. Best-effort: a delivery failure is non-critical (the
			// log + bridge state still catch it).
			if login != nil && login.User != nil {
				notice := fmt.Sprintf("Gmail inbox polling stopped: %s", err.Error())
				if nerr := sendBridgeNotice(context.Background(), login.User, notice); nerr != nil {
					logger.Warn().Err(nerr).Msg("Failed to deliver Gmail poller stop notice")
				}
			}
		}
	}()

	logger.Info().Strs("labels", labels).Msg("Gmail inbound poller started")
	return nil
}

// Stop tears down the poller for the given account. Idempotent.
func (m *GmailInboundManager) Stop(userMXID, emailAddr string) {
	if m == nil {
		return
	}
	key := userMXID + "|" + emailAddr
	m.mu.Lock()
	r := m.runners[key]
	delete(m.runners, key)
	m.mu.Unlock()
	if r != nil {
		r.cancel()
		r.poller.Stop()
	}
}

// Backlog asks the poller for (userMXID, emailAddr) to scan the past
// lookbackDays for messages with monitored labels, feeding each through the
// usual handleMessage path. Returns the number of messages fed (after dedup).
// Errors if no poller is registered for the account or lookbackDays <= 0.
//
// This is the recovery path for the messageAdded-only history regression: the
// old poller advanced its cursor past labelAdded events without surfacing
// them, so any tagged messages from before the fix can't be picked up by the
// normal history loop. Backlog re-fetches via messages.list, which sees
// current label state regardless of history cursor.
func (m *GmailInboundManager) Backlog(ctx context.Context, userMXID, emailAddr string, lookbackDays int) (int, error) {
	if m == nil {
		return 0, fmt.Errorf("GmailInboundManager: nil manager")
	}
	key := userMXID + "|" + emailAddr
	m.mu.Lock()
	r := m.runners[key]
	m.mu.Unlock()
	if r == nil {
		return 0, fmt.Errorf("no Gmail-API poller registered for %s (account may be password-mode, full-scope, or needs-reauth)", emailAddr)
	}
	logger := m.connector.Bridge.Log.With().Str("component", "gmail_backlog").Str("email", emailAddr).Logger()
	return r.poller.Backlog(ctx, lookbackDays, &logger)
}

// StopAll tears down every registered poller. Called from EmailConnector.Stop.
func (m *GmailInboundManager) StopAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	runners := m.runners
	m.runners = map[string]*gmailRunner{}
	m.mu.Unlock()
	for _, r := range runners {
		r.cancel()
		r.poller.Stop()
	}
}

// handleMessage is the OnMessage callback for the poller. Builds a
// ParsedEmail, runs the processor's shared post-parse pipeline, then queues
// the resulting Matrix event through the bridgev2 portal/event machinery.
//
// Mirrors pkg/imap/client.go's post-process path: portal lookup → portal
// creation if missing → ensure room → QueueRemoteEvent.
func (m *GmailInboundManager) handleMessage(ctx context.Context, login *bridgev2.UserLogin, msg *gmail.Message, mailbox string, fetch email.GmailAttachmentFetch) error {
	parsed, err := email.ParseGmailAPIMessage(msg, fetch)
	if err != nil {
		return fmt.Errorf("parse gmail message %s: %w", msg.Id, err)
	}
	emailMsg, err := m.connector.Processor.ProcessParsedEmail(ctx, parsed, login, mailbox)
	if err != nil {
		return fmt.Errorf("process parsed email: %w", err)
	}
	if emailMsg == nil {
		// dedup hit — nothing to forward
		return nil
	}

	matrixEvent := m.connector.Processor.ToMatrixEvent(ctx, emailMsg, login)

	portal, err := login.Bridge.GetExistingPortalByKey(ctx, emailMsg.PortalKey)
	if err != nil {
		return fmt.Errorf("portal lookup: %w", err)
	}
	if portal == nil {
		portal, err = login.Bridge.GetPortalByKey(ctx, emailMsg.PortalKey)
		if err != nil {
			return fmt.Errorf("create portal: %w", err)
		}
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ctx, login, nil); err != nil {
			return fmt.Errorf("create matrix room: %w", err)
		}
	}

	// Persist the reply context now, not only when the user sends. Otherwise
	// the stored copy describes the world as of their last reply, and after a
	// restart an ordinary typed reply is addressed from recipients this very
	// message may have dropped.
	if err := PersistThreadState(ctx, portal, emailMsg.Thread); err != nil {
		m.connector.Bridge.Log.Warn().Err(err).
			Str("portal_key", string(emailMsg.PortalKey.ID)).
			Msg("could not persist thread state on inbound; a restart before the next send will reply from stale recipients")
	}

	if !login.QueueRemoteEvent(matrixEvent).Success {
		return fmt.Errorf("queue remote event failed for message %s", emailMsg.MessageID)
	}
	return nil
}

