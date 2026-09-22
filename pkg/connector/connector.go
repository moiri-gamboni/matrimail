package connector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Leicas/matrimail/pkg/common"
	"github.com/Leicas/matrimail/pkg/email"
	"github.com/Leicas/matrimail/pkg/imap"
	"github.com/Leicas/matrimail/pkg/matrix"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

type EmailConnector struct {
	Bridge        *bridgev2.Bridge
	Config        Config
	IMAPManager   *imap.Manager
	RoomManager   *matrix.RoomManager
	ThreadManager *email.ThreadManager
	Processor     *email.Processor
	DB            *EmailAccountQuery

	// GmailInbound owns Gmail-API-mode inbound pollers, one per modify-mode
	// account. IMAP-mode (full scope) accounts go through IMAPManager
	// instead. Populated in Init.
	GmailInbound *GmailInboundManager

	// SentDedup records the messages we ourselves sent, keyed by Message-ID,
	// so the inbound IMAP processor can suppress the IDLE echo from the Sent
	// folder. Populated in Init; the sender (Phase C) will call Record from
	// HandleMatrixMessage.
	SentDedup *SentDedupQuery

	// initCancel cancels the long-lived context used for the dedup cleanup
	// goroutine. Set in Init, called from Stop.
	initCancel context.CancelFunc

	// oauthListeners maps a user's MXID to their currently-active OAuth
	// loopback listener so the `!matrimail oauth paste-code` admin command can
	// inject a code/state pair into the in-progress login (used on headless
	// hosts where the loopback callback can't be reached from the user's
	// browser even via SSH tunneling). Registered by handleOAuthEmail right
	// after StartOAuthListener; unregistered by the Wait/handleOAuthWait
	// success/error paths and by Cancel().
	oauthListenersMu sync.Mutex
	oauthListeners   map[string]*OAuthListener

	// pendingDMReplies tracks portals whose next outbound should be sent in
	// DM mode (reply-only-to-sender) instead of reply-all. Set by
	// MarkPortalNextReplyDM via the !matrimail reply-only command, consumed
	// once by handleMatrixMessageOutbound.
	pendingDMRepliesMu sync.Mutex
	pendingDMReplies   map[networkid.PortalID]struct{}
}

// MarkPortalNextReplyDM flags the next outbound in the given portal as a
// DM-mode reply.
func (ec *EmailConnector) MarkPortalNextReplyDM(portalID networkid.PortalID) {
	ec.pendingDMRepliesMu.Lock()
	defer ec.pendingDMRepliesMu.Unlock()
	if ec.pendingDMReplies == nil {
		ec.pendingDMReplies = map[networkid.PortalID]struct{}{}
	}
	ec.pendingDMReplies[portalID] = struct{}{}
}

// consumePortalNextReplyDM atomically tests-and-clears the DM toggle for a
// portal. Returns true exactly once after each MarkPortalNextReplyDM.
func (ec *EmailConnector) consumePortalNextReplyDM(portalID networkid.PortalID) bool {
	ec.pendingDMRepliesMu.Lock()
	defer ec.pendingDMRepliesMu.Unlock()
	if _, ok := ec.pendingDMReplies[portalID]; ok {
		delete(ec.pendingDMReplies, portalID)
		return true
	}
	return false
}

var (
	_ bridgev2.NetworkConnector = (*EmailConnector)(nil)
	_ bridgev2.StoppableNetwork = (*EmailConnector)(nil)
)

func (ec *EmailConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Matrimail",
		NetworkURL:           "https://en.wikipedia.org/wiki/Email",
		NetworkIcon:          "mxc://maunium.net/YgtkucQxWlKJxwMBJR6Ggz5w", // Email icon
		NetworkID:            "email",
		BeeperBridgeType:     "email",
		DefaultPort:          29319, // Different from WhatsApp's 29318
		DefaultCommandPrefix: "!matrimail",
	}
}

func (ec *EmailConnector) Init(bridge *bridgev2.Bridge) {
	ec.Bridge = bridge

	// Initialize config with default values
	imapConfig := IMAPConfig{
		DefaultTimeout:            30,
		StartupBackfillSeconds:    180,
		StartupBackfillMax:        25,
		InitialIdleTimeoutSeconds: 3,
	}

	// CRITICAL: do NOT do `ec.Config = Config{...}` here — that wipes whatever
	// the framework's LoadConfig decoded out of the user's network: block in
	// config.yaml (including GmailOAuth.{ClientID,ClientSecret}, which would
	// break the OAuth login flow picker).
	//
	// Instead, fill in zero-value defaults only for fields the user didn't set.
	// The example-config.yaml already documents the defaults; this is a safety
	// net for old configs that predate a field's introduction.
	if ec.Config.IMAP.DefaultTimeout == 0 {
		ec.Config.IMAP.DefaultTimeout = imapConfig.DefaultTimeout
	}
	if ec.Config.IMAP.StartupBackfillSeconds == 0 {
		ec.Config.IMAP.StartupBackfillSeconds = imapConfig.StartupBackfillSeconds
	}
	if ec.Config.IMAP.StartupBackfillMax == 0 {
		ec.Config.IMAP.StartupBackfillMax = imapConfig.StartupBackfillMax
	}
	if ec.Config.IMAP.InitialIdleTimeoutSeconds == 0 {
		ec.Config.IMAP.InitialIdleTimeoutSeconds = imapConfig.InitialIdleTimeoutSeconds
	}
	// Keep Network.IMAP populated for backward compatibility (some legacy paths
	// read from ec.Config.Network.IMAP rather than ec.Config.IMAP).
	if ec.Config.Network.IMAP.DefaultTimeout == 0 {
		ec.Config.Network.IMAP = ec.Config.IMAP
	}
	if ec.Config.Processing.MaxUploadBytes == 0 {
		ec.Config.Processing.MaxUploadBytes = DefaultMaxUploadBytes
	}
	// Logging.Sanitized defaults to true; can't distinguish "user set false"
	// from "zero value", so we trust whatever was decoded. Old configs without
	// the field present will get zero (false) — acceptable behavior change.

	// Allow environment overrides for verbose logging
	// MATRIMAIL_LOG_LEVEL: trace|debug|info|warn|error
	if lvl := strings.ToLower(os.Getenv("MATRIMAIL_LOG_LEVEL")); lvl != "" {
		switch lvl {
		case "trace":
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
		case "debug":
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		case "info":
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		case "warn":
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		case "error":
			zerolog.SetGlobalLevel(zerolog.ErrorLevel)
		}
	} else {
		// Default to maximum verbosity for analysis
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	}
	if san := strings.ToLower(os.Getenv("MATRIMAIL_LOG_SANITIZED")); san == "false" || san == "0" || san == "no" {
		ec.Config.Logging.Sanitized = false
	}

	// Ensure ./data directory exists for local SQLite files and sidecar WAL/SHM files
	dataDir := filepath.Join(".", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		bridge.Log.Warn().Err(err).Str("path", dataDir).Msg("Failed to ensure data directory exists")
	}
	if wd, err := os.Getwd(); err == nil {
		bridge.Log.Info().Str("cwd", wd).Str("data_dir", dataDir).Msg("Startup environment")
	}

	// Initialize database
	ec.DB = &EmailAccountQuery{
		DB: bridge.DB,
	}

	// Create database tables
	ctx := context.Background()
	if err := ec.DB.CreateTable(ctx); err != nil {
		bridge.Log.Error().Err(err).Msg("Failed to create email_accounts table - bridge initialization failed")
		panic(fmt.Errorf("database initialization failed: %w", err))
	}
	// Database health check: ensure we can write to the DB directory to avoid runtime I/O errors
	if err := ec.checkDBWritable(ctx); err != nil {
		bridge.Log.Error().Err(err).Msg("Database is not writable. Fix filesystem permissions or remove stale DB files, then restart the bridge.")
		panic(fmt.Errorf("database not writable: %w", err))
	}
	// Best-effort: add index for faster message lookups by (network, remote_id) if schema matches.
	if _, err := bridge.DB.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_message_network_remote ON message(network, remote_id)`); err == nil {
		bridge.Log.Trace().Msg("Ensured index idx_message_network_remote on message(network, remote_id)")
	}
	if _, err := bridge.DB.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_network_remote ON messages(network, remote_id)`); err == nil {
		bridge.Log.Trace().Msg("Ensured index idx_messages_network_remote on messages(network, remote_id)")
	}

	// Initialize managers
	logger := bridge.Log.With().Str("component", "imap").Logger()
	ec.IMAPManager = imap.NewManager(bridge, &logger, ec.Config.Logging.Sanitized, ec.Config.Logging.PseudonymSecret)

	roomLogger := bridge.Log.With().Str("component", "matrix").Logger()
	ec.RoomManager = matrix.NewRoomManager(&roomLogger)

	// Prefer a DB-backed resolver that can find existing portals by prior bridged messages
	resolver := &DBThreadMetadataResolver{Bridge: bridge, Log: &roomLogger, Network: "email"}
	ec.ThreadManager = email.NewThreadManager(resolver)

	// Initialize email processor and wire it to the IMAP manager
	processorLogger := bridge.Log.With().Str("component", "email_processor").Logger()
	ec.Processor = email.NewProcessor(&processorLogger, ec.ThreadManager, ec.Config.Logging.Sanitized, ec.Config.Logging.PseudonymSecret)
	// Apply processing config
	if ec.Config.Processing.MaxUploadBytes > 0 {
		ec.Processor.MaxUploadBytes = ec.Config.Processing.MaxUploadBytes
	}
	ec.Processor.GzipLargeBodies = ec.Config.Processing.GzipLargeBodies
	ec.IMAPManager.SetProcessor(ec.Processor)

	// Phase B: Sent-folder dedup. Schema first, then wire the checker into
	// the inbound processor. The bridge_id is the BeeperBridgeType so that a
	// shared DB across multiple bridge instances stays scoped per-bridge.
	ec.SentDedup = &SentDedupQuery{
		DB:       bridge.DB,
		BridgeID: ec.GetName().BeeperBridgeType,
	}
	if err := ec.SentDedup.CreateTable(ctx); err != nil {
		bridge.Log.Error().Err(err).Msg("Failed to create matrimail_sent_dedup table")
		panic(fmt.Errorf("sent dedup table init failed: %w", err))
	}
	ec.Processor.SetDedupChecker(&dedupAdapter{store: ec.SentDedup})

	// Alias resolver: when an inbound email lands, pickDeliveredTo needs the
	// list of addresses that count as "this user" (primary + send-as
	// aliases) so it can detect that mail addressed to e.g.
	// alice@example.com was delivered to the Gmail account billing@biz.com.
	ec.Processor.SetAliasResolver(func(receiver string) []string {
		login := ec.Bridge.GetCachedUserLoginByID(networkid.UserLoginID(receiver))
		if login == nil {
			return nil
		}
		client, _ := login.Client.(*EmailClient)
		if client == nil {
			return nil
		}
		out := []string{strings.ToLower(client.Email)}
		for _, sa := range client.SendAsAliases {
			if sa.Email != "" {
				out = append(out, strings.ToLower(sa.Email))
			}
		}
		return out
	})

	// Error notifier: surface user-visible processing errors into the
	// affected Matrix room (best-effort).
	ec.Processor.SetErrorNotifier(&processorErrorNotifier{ec: ec})

	// Gmail-API inbound pollers — one per modify-mode account, spawned in
	// LoadUserLogin. The manager itself is just bookkeeping.
	ec.GmailInbound = NewGmailInboundManager(ec)

	// Background cleanup goroutine: prune entries older than 30 days, every
	// 24h. The IMAP echo of an outbound send arrives within seconds, so
	// 30 days is more than enough headroom for slow Sent folders or
	// backfills. Cancellation is wired through ec.initCancel (called from Stop).
	cleanupCtx, cancel := context.WithCancel(context.Background())
	ec.initCancel = cancel
	go func() {
		// Run an initial cleanup at startup so the table doesn't bloat
		// indefinitely if the bridge is restarted frequently.
		if n, cerr := ec.SentDedup.Cleanup(cleanupCtx, time.Now().Add(-30*24*time.Hour)); cerr != nil {
			bridge.Log.Warn().Err(cerr).Msg("sent_dedup startup cleanup failed")
		} else if n > 0 {
			bridge.Log.Info().Int64("rows", n).Msg("sent_dedup startup cleanup deleted old entries")
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				if n, cerr := ec.SentDedup.Cleanup(cleanupCtx, time.Now().Add(-30*24*time.Hour)); cerr != nil {
					bridge.Log.Warn().Err(cerr).Msg("sent_dedup periodic cleanup failed")
				} else if n > 0 {
					bridge.Log.Debug().Int64("rows", n).Msg("sent_dedup periodic cleanup")
				}
			}
		}
	}()

	// Add commands with connector context
	ec.Bridge.Commands.(*commands.Processor).AddHandlers(
		ec.createCommands()...,
	)
}

// createCommands creates command handlers with access to this connector instance
func (ec *EmailConnector) createCommands() []commands.CommandHandler {
	return []commands.CommandHandler{
		&commands.FullHandler{
			Func: fnPing,
			Name: "ping",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Check if the bridge is alive",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnStatus(ce, ec) },
			Name: "status",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Show connection status for your email accounts",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnLogin(ce, ec) },
			Name: "login",
			Help: commands.HelpMeta{
				Section:     HelpSectionAuth,
				Description: "Login to an email account",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnLogout(ce, ec) },
			Name: "logout",
			Help: commands.HelpMeta{
				Section:     HelpSectionAuth,
				Description: "Logout from email accounts",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnList(ce, ec) },
			Name: "list",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "List configured email accounts",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnSync(ce, ec) },
			Name: "sync",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Manually trigger email synchronization",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnReconnect(ce, ec) },
			Name: "reconnect",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Reconnect to email servers",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnNuke(ce, ec) },
			Name: "nuke",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Remove all email accounts and reset database",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnPassphrase(ce, ec) },
			Name: "passphrase",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Show where the database encryption passphrase is stored (never prints it)",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnConfig(ce, ec) },
			Name: "config",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Configure email bridge settings (e.g., config folders)",
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnCompose(ce, ec) },
			Name: "compose",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Start a new email thread.",
				Args:        `to:<email> [cc:...] [subject:"..."]`,
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnOAuth(ce, ec) },
			Name: "oauth",
			Help: commands.HelpMeta{
				Section:     HelpSectionAuth,
				Description: "Manage OAuth tokens (paste-code / paste-token / revoke).",
				Args:        `paste-code <redirect-url> | paste-token <email> <refresh_token> | revoke <email>`,
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnBacklog(ce, ec) },
			Name: "backlog",
			Help: commands.HelpMeta{
				Section:     HelpSectionAdmin,
				Description: "Scan the past N days for messages with monitored labels and forward any that haven't been bridged (modify-scope Gmail only).",
				Args:        `[days] [email]`,
			},
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnDraft(ce, ec) },
			Name: "draft",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Ask the configured external workflow (e.g. n8n) to generate a Gmail draft reply for this thread. Run from inside the thread's portal room. Extra words are forwarded as an instruction.",
				Args:        `[instruction…]`,
			},
			RequiresPortal: true,
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnReplyOnly(ce, ec) },
			Name: "reply-only",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Make your next message in this room reply ONLY to the original sender (DM mode), not reply-all.",
			},
			RequiresPortal: true,
		},
		&commands.FullHandler{
			Func: func(ce *commands.Event) { fnReplyOnly(ce, ec) },
			Name: "dm",
			Help: commands.HelpMeta{
				Section:     HelpSectionInfo,
				Description: "Alias of !matrimail reply-only.",
			},
			RequiresPortal: true,
		},
	}
}

func (ec *EmailConnector) Start(ctx context.Context) error {
	ec.Bridge.Log.Info().Msg("Email connector starting...")
	return nil
}

// Stop gracefully shuts down the EmailConnector and all IMAP connections
func (ec *EmailConnector) Stop() {
	ec.Bridge.Log.Info().Msg("Email connector stopping...")

	// Stop all IMAP clients
	if ec.IMAPManager != nil {
		ec.IMAPManager.StopAll()
	}

	// Stop all Gmail-API pollers
	if ec.GmailInbound != nil {
		ec.GmailInbound.StopAll()
	}

	// Cancel the dedup cleanup goroutine.
	if ec.initCancel != nil {
		ec.initCancel()
	}

	ec.Bridge.Log.Info().Msg("Email connector stopped")
}

// registerOAuthListener stores the listener for the given user's MXID so the
// paste-code command can find it. Replaces any previous entry — bridgev2
// only runs one login process per user at a time, so a fresh `!matrimail
// login` invalidates the previous listener anyway.
func (ec *EmailConnector) registerOAuthListener(mxid string, l *OAuthListener) {
	ec.oauthListenersMu.Lock()
	defer ec.oauthListenersMu.Unlock()
	if ec.oauthListeners == nil {
		ec.oauthListeners = map[string]*OAuthListener{}
	}
	ec.oauthListeners[mxid] = l
}

// unregisterOAuthListener removes the stored listener iff it's still the same
// instance — protects against a race where a fresh login replaced it before
// the old one's defer ran.
func (ec *EmailConnector) unregisterOAuthListener(mxid string, l *OAuthListener) {
	ec.oauthListenersMu.Lock()
	defer ec.oauthListenersMu.Unlock()
	if ec.oauthListeners[mxid] == l {
		delete(ec.oauthListeners, mxid)
	}
}

// lookupOAuthListener returns the listener registered for the user, or nil if
// none. Caller must not retain the pointer past the response — Wait/Cancel
// can deregister concurrently.
func (ec *EmailConnector) lookupOAuthListener(mxid string) *OAuthListener {
	ec.oauthListenersMu.Lock()
	defer ec.oauthListenersMu.Unlock()
	return ec.oauthListeners[mxid]
}

// dedupAdapter is the connector-side glue that exposes SentDedupQuery.Exists
// as the email.DedupChecker interface. Lives in this file (vs. a separate
// shim file) because the type is too small to deserve its own and inverting
// the import direction (email package importing connector) would create a
// cycle.
type dedupAdapter struct {
	store *SentDedupQuery
}

// IsOurMessage reports whether the given Message-ID was previously recorded
// by the outbound sender for this receiver. Returns (false, nil) when the
// store is nil so a half-initialized connector still serves inbound traffic.
func (d *dedupAdapter) IsOurMessage(ctx context.Context, receiver, messageID string) (bool, error) {
	if d == nil || d.store == nil {
		return false, nil
	}
	return d.store.Exists(ctx, receiver, messageID)
}

// LoadUserLogin is now implemented in client.go

func (ec *EmailConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		DisappearingMessages: false,
		AggressiveUpdateInfo: false,
	}
}

func (ec *EmailConnector) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	// Extract email address from ghost ID
	userIDStr := string(ghost.ID)
	// Remove "email:" prefix if present
	if len(userIDStr) > 6 && userIDStr[:6] == "email:" {
		userIDStr = userIDStr[6:]
	}
	return &bridgev2.UserInfo{
		Name:        &userIDStr,
		Avatar:      nil,
		IsBot:       nil,
		Identifiers: []string{userIDStr},
	}, nil
}

// GetChatInfo implements the bridgev2 interface for portal/room creation
func (ec *EmailConnector) GetChatInfo(ctx context.Context, portal *bridgev2.Portal, userLogin *bridgev2.UserLogin, portalID networkid.PortalKey) (*bridgev2.ChatInfo, error) {
	// Extract thread ID from portal ID
	threadID := string(portalID.ID)
	if len(threadID) > 7 && threadID[:7] == "thread:" {
		threadID = threadID[7:] // Remove "thread:" prefix
	}

	// If we have richer thread info, build the room using RoomManager
	if ec.ThreadManager != nil && ec.RoomManager != nil {
		thread := ec.ThreadManager.GetThreadByID(string(userLogin.ID), threadID)

		// Phase D: synthetic compose threads can fall out of the in-memory
		// cache after 24h. Reconstruct from Portal.Metadata so the room is
		// still functional after a bridge restart or long idle.
		if thread == nil && portal != nil {
			if pm, ok := portal.Metadata.(*PortalMetadata); ok && pm != nil && pm.ThreadID == threadID {
				thread = ThreadFromPortalMetadata(pm)
				ec.ThreadManager.CacheForReceiver(string(userLogin.ID), thread)
				ec.Bridge.Log.Debug().
					Str("thread_id", threadID).
					Bool("is_draft", thread.IsDraft).
					Int("participants", len(thread.Participants)).
					Msg("Reconstructed thread from Portal.Metadata after ThreadManager cache miss")
			}
		}

		if thread != nil {
			return ec.RoomManager.GetChatInfoForThread(ctx, thread, userLogin)
		}
	}

	// Fallback: basic room configuration when thread is not yet known
	roomName := fmt.Sprintf("Email Thread: %s", threadID)
	roomTopic := "Email thread - messages will appear here when emails are received"

	// Start with empty member list - participants will be added when emails are processed
	chatMembers := make([]bridgev2.ChatMember, 0)

	chatInfo := &bridgev2.ChatInfo{
		Name:   &roomName,
		Topic:  &roomTopic,
		Avatar: nil,
		Type:   ptr.Ptr(database.RoomTypeDefault),
		Members: &bridgev2.ChatMemberList{
			Members: chatMembers,
			IsFull:  true,
		},
		// Match the room_manager fallback's tagging — see comment there.
		UserLocal: &bridgev2.UserLocalPortalInfo{
			Tag: ptr.Ptr(event.RoomTagLowPriority),
		},
	}

	ec.Bridge.Log.Debug().
		Str("thread_id", threadID).
		Str("room_name", roomName).
		Msg("Created fallback ChatInfo for email thread")

	return chatInfo, nil
}

// Required methods for NetworkConnector interface
func (ec *EmailConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID == "" {
		flowID = LoginFlowIDPassword
	}
	if flowID == LoginFlowIDOAuthGmail {
		// Reject early when the bridge has no Gmail OAuth credentials
		// configured — better than letting the flow start and fail when we
		// try to spin up the loopback listener.
		if ec.Config.GmailOAuth.ClientID == "" || ec.Config.GmailOAuth.ClientSecret == "" {
			return nil, fmt.Errorf("gmail_oauth not configured on this bridge; see docs/gmail-oauth-setup.md, or use the email-password flow")
		}
	}
	return &EmailLoginProcess{user: user, connector: ec, flowID: flowID}, nil
}

func (ec *EmailConnector) GetDBMetaTables() []any {
	return []any{
		&EmailAccount{},
	}
}

func (ec *EmailConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		// Portal metadata is used to persist synthetic compose threads (and
		// last-known thread state for normal threads) across the 24h
		// ThreadManager cache TTL. See pkg/connector/portal_metadata.go.
		Portal: func() any { return &PortalMetadata{} },
	}
}

func (ec *EmailConnector) GetBridgeInfoVersion() (int, int) {

	return 1, 1 // Version 1.1
}

func (ec *EmailConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{{
		Name:        "Email + App Password",
		Description: "IMAP login with an email address and an app password (Gmail, Outlook, iCloud, FastMail, custom IMAP, etc.)",
		ID:          LoginFlowIDPassword,
	}}
	// Only advertise the Gmail OAuth flow if the bridge admin has configured
	// the Desktop client credentials. Otherwise Beeper would offer a flow
	// that immediately errors in CreateLogin.
	if ec.Config.GmailOAuth.ClientID != "" && ec.Config.GmailOAuth.ClientSecret != "" {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Gmail (OAuth, recommended)",
			Description: "Sign in via Google's authorization-code + PKCE flow with a loopback redirect. Default uses the Gmail API (sensitive scope, long-lived refresh tokens). Optional 'full' mode falls back to IMAP/SMTP XOAUTH2 via mail.google.com (restricted scope, weekly re-auth in Testing mode).",
			ID:          LoginFlowIDOAuthGmail,
		})
	}
	return flows
}

// Helper functions for creating network IDs
func MakeUserID(email string) networkid.UserID {
	return common.EmailToGhostID(email)
}

func MakePortalID(threadID string) networkid.PortalID {
	return networkid.PortalID(fmt.Sprintf("thread:%s", threadID))
}

// checkDBWritable attempts a few write operations to ensure the underlying DB is writable
// and the directory allows journaling/WAL files. This prevents silent runtime failures later.
func (ec *EmailConnector) checkDBWritable(ctx context.Context) error {
	// Create a tiny health table and write a row, then delete it.
	_, err := ec.Bridge.DB.Exec(ctx, `CREATE TABLE IF NOT EXISTS email_health_check (ts INTEGER NOT NULL)`)
	if err != nil {
		return fmt.Errorf("failed to create health check table: %w", err)
	}
	// Inline the value to avoid placeholder dialect differences (SQLite uses ?, Postgres uses $1).
	// The row is deleted immediately below; the actual value doesn't matter — this only verifies write access.
	_, err = ec.Bridge.DB.Exec(ctx, `INSERT INTO email_health_check (ts) VALUES (0)`)
	if err != nil {
		return fmt.Errorf("failed to insert into health check table: %w", err)
	}
	_, err = ec.Bridge.DB.Exec(ctx, `DELETE FROM email_health_check`)
	if err != nil {
		return fmt.Errorf("failed to delete from health check table: %w", err)
	}
	return nil
}
