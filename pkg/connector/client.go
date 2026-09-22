package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/Leicas/matrimail/pkg/coordinator"
	"github.com/Leicas/matrimail/pkg/email"
	"github.com/Leicas/matrimail/pkg/imap"
	"github.com/Leicas/matrimail/pkg/reliability"
)

// ClientConfig holds configuration for EmailClient timing and behavior
type ClientConfig struct {
	// Registration wait time before checking if client connected
	RegistrationWaitTime time.Duration

	// Reconnection sleep time to allow server-side cleanup
	ReconnectionSleepTime time.Duration

	// Periodic sync interval
	PeriodicSyncInterval time.Duration

	// Minimum time between sync operations (throttling)
	SyncThrottleTime time.Duration
}

// DefaultClientConfig returns sensible defaults for client configuration
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		RegistrationWaitTime:  2 * time.Second,
		ReconnectionSleepTime: 2 * time.Second,
		PeriodicSyncInterval:  5 * time.Minute,
		SyncThrottleTime:      time.Minute,
	}
}

// EmailClient implements bridgev2.NetworkAPI for email accounts
type EmailClient struct {
	Main      *EmailConnector
	UserLogin *bridgev2.UserLogin

	// Email account information
	Email    string
	Username string
	Password string

	// Folders to monitor (from user selection during login)
	MonitoredFolders []string

	// IMAP client
	IMAPClient *imap.Client

	// GmailAPIInbound flags accounts that are served by the Gmail-API inbound
	// poller (modify-scope OAuth) instead of IMAP. When true, IMAPClient is
	// intentionally nil and Connect() must not treat that as auth_failure;
	// the bridge state for the inbox is "established" once the poller is
	// running (the poller's own ticker handles ongoing health).
	GmailAPIInbound bool

	// Sender drives outbound email (SMTP or Gmail API). Populated by
	// LoadUserLogin via email.PickSender; consulted by HandleMatrixMessage.
	Sender email.Sender

	// Signature is the user's Gmail-configured HTML signature, fetched once
	// at LoadUserLogin time via users.settings.sendAs.list. Empty for
	// non-Gmail accounts and for Gmail users who haven't set a signature.
	// Appended to outbound messages on NEW threads only (replies go without
	// it, matching Gmail's "include only on first message" toggle).
	Signature string

	// SendAsAliases is the user's full send-as identity set (primary +
	// aliases) as reported by Gmail. Populated for OAuth-Gmail accounts at
	// LoadUserLogin time; empty for password / non-Gmail accounts. Used so
	// inbound mail addressed to an alias gets a reply From that alias, not
	// from the primary address.
	SendAsAliases []email.GmailSendAs

	// Configuration
	config ClientConfig

	// State management
	stateCoordinator *coordinator.StateCoordinator
	isConnected      atomic.Bool
	stopLoops        atomic.Pointer[context.CancelFunc]

	// Client lifecycle context management
	ctx    context.Context
	cancel context.CancelFunc

	// Synchronization
	historySyncMutex sync.Mutex
	lastSyncTime     time.Time
}

var (
	_ bridgev2.NetworkAPI = (*EmailClient)(nil)
)

// EmailClientErrors - Core connector-level errors
var (
	EmailNotLoggedIn      = status.BridgeStateErrorCode("E-EMAIL-001")
	EmailConnectionFailed = status.BridgeStateErrorCode("E-EMAIL-002")
	EmailAuthFailure      = status.BridgeStateErrorCode("E-EMAIL-005")
	// EmailReauthRequired ("E-EMAIL-006") is defined in reauth.go.
)

// extractEmailCredentials extracts email and username from login metadata or login ID
func (ec *EmailConnector) extractEmailCredentials(login *bridgev2.UserLogin) (string, string, error) {
	var email, username string

	// Extract login metadata with proper error handling
	if login.Metadata != nil {
		if loginMetadata, ok := login.Metadata.(*EmailLoginMetadata); ok && loginMetadata.Email != "" {
			email = loginMetadata.Email
			username = loginMetadata.Username
			ec.Bridge.Log.Debug().Str("email", email).Msg("Loaded credentials from login metadata")
		}
	}

	// If metadata is missing or invalid, try to extract email from login ID
	if email == "" {
		// Login ID format should be "email:user@domain.com"
		loginIDStr := string(login.ID)
		if len(loginIDStr) > 6 && loginIDStr[:6] == "email:" {
			email = loginIDStr[6:]
			username = email // Use email as username by default
			ec.Bridge.Log.Debug().Str("email", email).Msg("Extracted email from login ID")
		}
	}

	// If we still don't have an email, this login is invalid
	if email == "" {
		ec.Bridge.Log.Warn().Str("login_id", string(login.ID)).Msg("No email found in login metadata or ID")
		return "", "", fmt.Errorf("invalid login: no email found in metadata or login ID %s", login.ID)
	}

	return email, username, nil
}

// loadAccountCredentials loads account credentials from database
func (ec *EmailConnector) loadAccountCredentials(ctx context.Context, userMXID, email string) (*EmailAccount, error) {
	account, err := ec.DB.GetAccount(ctx, userMXID, email)
	if err != nil {
		return nil, fmt.Errorf("failed to load email account: %w", err)
	}

	if account == nil {
		ec.Bridge.Log.Debug().Str("email", email).Msg("No account credentials found")
		return nil, fmt.Errorf("no account credentials found for email %s", email)
	}

	return account, nil
}

// createIMAPClient creates and configures the IMAP client.
//
// When the account's saved auth_type is "oauth-gmail" we build an OAuth
// client and wire a refresh-aware TokenProvider so each (re)connect mints a
// fresh access token via the saved refresh token. Refresh failures that look
// permanent (invalid_grant, revoked, 7-day Testing-mode expiry) trigger the
// re-auth DM via EmailConnector.HandleRefreshError; the IMAP layer then
// fails fast instead of looping reconnects against a dead token.
//
// Accounts flagged with auth_type='oauth-gmail-needs-reauth' are skipped at
// connection time — they stay resident but inert until the user runs
// !matrimail login. Same applies to scope-mode 'modify' accounts: those
// don't have IMAP authorization and are served by the Gmail API inbound
// poller in inbound_gmail_api.go, not by this client.
func (ec *EmailConnector) createIMAPClient(emailClient *EmailClient, login *bridgev2.UserLogin) error {
	logger := login.Log.With().Str("component", "imap").Logger()

	info, err := ec.DB.GetOAuthAccount(context.Background(), login.UserMXID.String(), emailClient.Email)
	if err != nil {
		ec.Bridge.Log.Warn().Err(err).Msg("GetOAuthAccount failed; falling back to password auth")
	}
	if info != nil {
		// Skip connection setup if the account is flagged for re-auth — keep
		// the row, surface the bridge state, and wait for the user.
		if info.AuthType == AuthTypeOAuthGmailNeedsReauth {
			ec.Bridge.Log.Info().
				Str("email", emailClient.Email).
				Msg("OAuth account is flagged for re-auth; skipping IMAP setup until user runs !matrimail login")
			ReportReauthBridgeState(emailClient.stateCoordinator, emailClient.Email)
			// Leave IMAPClient nil; the EmailClient.Connect() path treats
			// nil IMAPClient as "no inbound transport configured" and reports
			// EmailNotLoggedIn, which is acceptable.
			return nil
		}

		// Modify-mode accounts use the Gmail API inbound poller (Phase 2),
		// not IMAP. Skip IMAP setup; the poller is wired up separately.
		if info.ScopeMode == ScopeModeModify {
			ec.Bridge.Log.Debug().
				Str("email", emailClient.Email).
				Msg("OAuth account is in scope_mode=modify; using Gmail API inbound poller, skipping IMAP setup")
			return nil
		}

		// Full-mode OAuth: IMAP via XOAUTH2.
		if info.Token == nil {
			return fmt.Errorf("account %s is OAuth-full but has no stored token", emailClient.Email)
		}
		oauthCfg := email.GmailOAuthConfig{
			ClientID:     ec.Config.GmailOAuth.ClientID,
			ClientSecret: ec.Config.GmailOAuth.ClientSecret,
		}
		if oauthCfg.ClientID == "" || oauthCfg.ClientSecret == "" {
			return fmt.Errorf("account %s is OAuth but bridge has no gmail_oauth config", emailClient.Email)
		}
		// Use a long-lived background context for the TokenSource so token
		// refreshes survive any per-request context cancellation.
		ts := email.TokenSource(context.Background(), oauthCfg, info.Token)

		imapClient, ierr := imap.NewClientOAuth(
			emailClient.Email,
			emailClient.Username,
			info.Token.AccessToken,
			login,
			&logger,
			ec.Config.Logging.Sanitized,
			ec.Config.Logging.PseudonymSecret,
			ec.Config.Network.IMAP.StartupBackfillSeconds,
			ec.Config.Network.IMAP.StartupBackfillMax,
			ec.Config.Network.IMAP.InitialIdleTimeoutSeconds,
			emailClient.stateCoordinator,
		)
		if ierr != nil {
			return fmt.Errorf("failed to create OAuth IMAP client: %w", ierr)
		}
		// Refresh-on-reconnect provider with permanent-failure detection.
		// Persist new tokens whenever the underlying TokenSource hands us a
		// freshly minted one so a bridge restart doesn't lose the latest
		// access token; on a permanent refresh failure flip the account to
		// needs-reauth and fire the DM.
		userMXID := login.UserMXID.String()
		emailAddr := emailClient.Email
		provider := info.Provider
		scopeMode := info.ScopeMode
		imapClient.SetTokenProvider(func(ctx context.Context) (string, error) {
			fresh, terr := ts.Token()
			if terr != nil {
				if ec.HandleRefreshError(ctx, login, userMXID, emailAddr, scopeMode, terr) {
					ReportReauthBridgeState(emailClient.stateCoordinator, emailAddr)
				}
				return "", terr
			}
			// Best-effort persistence. Failures are non-fatal — the in-memory
			// TokenSource still has the new token for subsequent reconnects.
			if err := ec.DB.SaveOAuthToken(ctx, userMXID, emailAddr, provider, fresh); err != nil {
				ec.Bridge.Log.Warn().Err(err).Msg("Failed to persist refreshed OAuth token")
			}
			return fresh.AccessToken, nil
		})
		imapClient.PersistThreadState = PersistThreadState
		emailClient.IMAPClient = imapClient
		if ec.Processor != nil {
			emailClient.IMAPClient.SetProcessor(ec.Processor)
		}
		return nil
	}

	imapClient, err := imap.NewClient(
		emailClient.Email,
		emailClient.Username,
		emailClient.Password,
		login,
		&logger,
		ec.Config.Logging.Sanitized,
		ec.Config.Logging.PseudonymSecret,
		ec.Config.Network.IMAP.StartupBackfillSeconds,
		ec.Config.Network.IMAP.StartupBackfillMax,
		ec.Config.Network.IMAP.InitialIdleTimeoutSeconds,
		emailClient.stateCoordinator,
	)
	if err != nil {
		return fmt.Errorf("failed to create IMAP client: %w", err)
	}

	imapClient.PersistThreadState = PersistThreadState
	emailClient.IMAPClient = imapClient

	// Set the email processor on the IMAP client
	if ec.Processor != nil {
		emailClient.IMAPClient.SetProcessor(ec.Processor)
	}

	return nil
}

// startGmailInboundIfApplicable spins up the Gmail-API inbound poller for
// scope-mode='modify' OAuth accounts. No-op for password-mode accounts,
// for full-mode OAuth (handled by IMAP), and for accounts flagged for
// re-auth.
func (ec *EmailConnector) startGmailInboundIfApplicable(ctx context.Context, emailClient *EmailClient, login *bridgev2.UserLogin, account *EmailAccount) error {
	if account.AuthType != AuthTypeOAuthGmail {
		return nil // password mode or needs-reauth — no Gmail-API poller
	}
	info, err := ec.DB.GetOAuthAccount(ctx, login.UserMXID.String(), emailClient.Email)
	if err != nil {
		return fmt.Errorf("get oauth account: %w", err)
	}
	if info == nil || info.Token == nil {
		return nil
	}
	if info.ScopeMode != ScopeModeModify {
		return nil
	}
	oauthCfg := email.GmailOAuthConfig{
		ClientID:     ec.Config.GmailOAuth.ClientID,
		ClientSecret: ec.Config.GmailOAuth.ClientSecret,
	}
	if oauthCfg.ClientID == "" || oauthCfg.ClientSecret == "" {
		return fmt.Errorf("gmail_oauth not configured but account %s is OAuth", emailClient.Email)
	}
	base := email.TokenSource(context.Background(), oauthCfg, info.Token)
	ts := newReauthAwareTokenSource(base, ec, login,
		login.UserMXID.String(), emailClient.Email, info.Provider, info.ScopeMode)

	if err := ec.GmailInbound.Start(ctx, login, emailClient.Email, account.MonitoredFolders, ts); err != nil {
		return err
	}
	emailClient.GmailAPIInbound = true
	return nil
}

// startClientConnections starts the client connection and registration processes
func (ec *EmailConnector) startClientConnections(emailClient *EmailClient, login *bridgev2.UserLogin) {
	// Automatically connect the client after loading
	// Use client's lifecycle context for long-running connection
	go emailClient.Connect(emailClient.ctx)

	// Register client with IMAP manager for status reporting
	go func() {
		// Wait a moment for connection to establish, respecting context cancellation
		select {
		case <-time.After(emailClient.config.RegistrationWaitTime):
			if emailClient.IsConnected() {
				// Register the connected client with the manager for status reporting
				ec.IMAPManager.RegisterClient(login.UserMXID.String(), emailClient.Email, emailClient.IMAPClient)
			}
		case <-emailClient.ctx.Done():
			// Client cancelled, exit goroutine
			return
		}
	}()
}

func (ec *EmailConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	// Create client lifecycle context that survives beyond this function
	clientCtx, clientCancel := context.WithCancel(context.Background())

	// Create email client for this login
	emailClient := &EmailClient{
		Main:      ec,
		UserLogin: login,
		ctx:       clientCtx,
		cancel:    clientCancel,
		config:    DefaultClientConfig(),
	}

	// Initialize state coordinator
	emailClient.stateCoordinator = coordinator.NewStateCoordinator(login, &ec.Bridge.Log)
	login.Client = emailClient

	// Extract email credentials
	email, username, err := ec.extractEmailCredentials(login)
	if err != nil {
		return err
	}
	emailClient.Email = email
	emailClient.Username = username

	// Load account credentials from database
	account, err := ec.loadAccountCredentials(ctx, login.UserMXID.String(), emailClient.Email)
	if err != nil {
		return err
	}
	emailClient.Password = account.Password
	emailClient.MonitoredFolders = account.MonitoredFolders

	// Log configured folders
	if len(account.MonitoredFolders) > 0 {
		ec.Bridge.Log.Info().
			Strs("folders", account.MonitoredFolders).
			Str("email", emailClient.Email).
			Msg("Loaded monitored folders configuration")
	} else {
		ec.Bridge.Log.Debug().Str("email", emailClient.Email).Msg("No monitored folders configured, will use INBOX")
		emailClient.MonitoredFolders = []string{"INBOX"}
	}

	// Create IMAP client (skipped for modify-mode and needs-reauth accounts)
	if err := ec.createIMAPClient(emailClient, login); err != nil {
		return err
	}

	// Build the outbound Sender. Gmail OAuth accounts get the Gmail API path
	// (which echoes a server-assigned Message-ID we use for dedup); everything
	// else falls through to provider-specific SMTP submission with PLAIN.
	if err := ec.buildSender(ctx, emailClient, login, account); err != nil {
		// Outbound failure shouldn't block inbound — log and continue with a
		// nil Sender. HandleMatrixMessage will reject sends with a clear error.
		ec.Bridge.Log.Warn().Err(err).Str("email", emailClient.Email).Msg("Failed to build outbound Sender; inbound still functional")
	}

	// Best-effort fetch of the Gmail-side signature so new outbound threads
	// carry it (matches the Gmail web UI's "include signature on first
	// message" behavior). Cached on EmailClient.Signature for the bridge's
	// lifetime; bridge restart re-fetches.
	if account.AuthType == AuthTypeOAuthGmail {
		if err := ec.loadGmailSignature(ctx, emailClient, login); err != nil {
			ec.Bridge.Log.Debug().Err(err).Str("email", emailClient.Email).Msg("Failed to load Gmail signature; outbound new-thread emails will go without one")
		}
	}

	// Spawn the Gmail-API inbound poller for modify-mode accounts. The poller
	// is independent of the IMAP client (which is skipped for modify-mode in
	// createIMAPClient) and runs on its own goroutine. needs-reauth accounts
	// also skip the poller — it would just hammer Google with a dead refresh
	// token.
	if err := ec.startGmailInboundIfApplicable(ctx, emailClient, login, account); err != nil {
		ec.Bridge.Log.Warn().Err(err).Str("email", emailClient.Email).Msg("Failed to start Gmail inbound poller")
	}

	// Start client connections
	ec.startClientConnections(emailClient, login)

	return nil
}

// buildSender constructs the outbound email.Sender for an account. For OAuth
// accounts it loads the persisted refresh token and wraps it in an
// oauth2.TokenSource so subsequent sends mint a fresh access token; for
// password accounts it just hands off the app password to PickSender.
func (ec *EmailConnector) buildSender(ctx context.Context, emailClient *EmailClient, login *bridgev2.UserLogin, account *EmailAccount) error {
	senderAccount := email.SenderAccount{
		Email:       emailClient.Email,
		IsGmail:     email.IsGmailDomain(emailClient.Email),
		AppPassword: emailClient.Password,
	}
	// Detect Google Workspace custom domains (e.g. user@company.com hosted on
	// Google) via MX lookup so the SMTP path uses smtp.gmail.com instead of
	// smtp.<custom-domain> which doesn't exist.
	if !senderAccount.IsGmail {
		if at := strings.LastIndex(emailClient.Email, "@"); at >= 0 {
			if email.IsGoogleWorkspaceDomain(emailClient.Email[at+1:]) {
				senderAccount.IsGoogleWorkspace = true
			}
		}
	}

	if account.AuthType == AuthTypeOAuthGmail || account.AuthType == AuthTypeOAuthGmailNeedsReauth {
		// OAuth path — load token and build a refresh-aware TokenSource.
		oauthCfg := email.GmailOAuthConfig{
			ClientID:     ec.Config.GmailOAuth.ClientID,
			ClientSecret: ec.Config.GmailOAuth.ClientSecret,
		}
		if oauthCfg.ClientID == "" || oauthCfg.ClientSecret == "" {
			return fmt.Errorf("account %s is OAuth but bridge has no gmail_oauth config", emailClient.Email)
		}
		info, err := ec.DB.GetOAuthAccount(context.Background(), login.UserMXID.String(), emailClient.Email)
		if err != nil {
			return fmt.Errorf("load oauth token: %w", err)
		}
		if info == nil || info.Token == nil {
			return fmt.Errorf("account %s is OAuth but has no stored token", emailClient.Email)
		}
		// If the account has been flagged for re-auth, don't build a working
		// sender — outbound sends would just hammer Google with a dead
		// refresh token and re-fire the re-auth DM. Surface a clear error.
		if info.AuthType == AuthTypeOAuthGmailNeedsReauth {
			return fmt.Errorf("account %s needs re-auth; run !matrimail login", emailClient.Email)
		}
		// Long-lived background context so token refreshes survive any
		// per-request context cancellation. Wrap with reauthAwareTokenSource
		// so refresh failures during outbound sends fire the re-auth DM
		// (the IMAP path has its own SetTokenProvider hook).
		base := email.TokenSource(context.Background(), oauthCfg, info.Token)
		senderAccount.OAuthTokenSource = newReauthAwareTokenSource(
			base, ec, login,
			login.UserMXID.String(), emailClient.Email, info.Provider, info.ScopeMode,
		)
		// Workspace custom domains aren't gmail.com; OAuth presence is the
		// real signal that this account speaks the Gmail API.
		senderAccount.IsGmail = true
	}

	logger := ec.Bridge.Log.With().Str("component", "sender").Str("email", emailClient.Email).Logger()
	sender, err := email.PickSender(ctx, senderAccount, &logger)
	if err != nil {
		return fmt.Errorf("build sender: %w", err)
	}
	emailClient.Sender = sender
	ec.Bridge.Log.Info().
		Str("email", emailClient.Email).
		Str("provider", sender.Provider()).
		Msg("Outbound Sender ready")
	return nil
}

// loadGmailSignature fetches the user's Gmail-side signature via
// users.settings.sendAs.list and caches it on the EmailClient. Best-effort:
// errors are surfaced as the returned error and the caller logs at Debug.
// Empty signature (user hasn't configured one in Gmail) is not an error.
func (ec *EmailConnector) loadGmailSignature(ctx context.Context, emailClient *EmailClient, login *bridgev2.UserLogin) error {
	cfg := ec.Config.GmailOAuth
	emailCfg := email.GmailOAuthConfig{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret}
	info, err := ec.DB.GetOAuthAccount(ctx, login.UserMXID.String(), emailClient.Email)
	if err != nil {
		return fmt.Errorf("get oauth account: %w", err)
	}
	if info == nil || info.Token == nil {
		return nil
	}
	ts := email.TokenSource(context.Background(), emailCfg, info.Token)
	svc, err := gmail.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return fmt.Errorf("create gmail service: %w", err)
	}
	sig, err := email.FetchGmailSignature(ctx, svc, emailClient.Email)
	if err != nil {
		return fmt.Errorf("fetch signature: %w", err)
	}
	emailClient.Signature = sig
	if sig != "" {
		ec.Bridge.Log.Info().Str("email", emailClient.Email).Int("signature_html_len", len(sig)).Msg("Loaded Gmail signature for outbound new-thread emails")
	}
	// Best-effort: load the user's full send-as identity list (primary +
	// aliases). Failures are non-fatal — the user just won't get
	// alias-preserving From on replies addressed to an alias.
	aliases, aerr := email.FetchGmailSendAsList(ctx, svc)
	if aerr != nil {
		ec.Bridge.Log.Debug().Err(aerr).Str("email", emailClient.Email).Msg("Failed to load Gmail send-as aliases; replies will always go from the primary address")
		emailClient.SendAsAliases = nil
	} else {
		emailClient.SendAsAliases = aliases
		ec.Bridge.Log.Info().Str("email", emailClient.Email).Int("count", len(aliases)).Msg("Loaded Gmail send-as identities")
	}
	return nil
}

func (ec *EmailClient) Connect(ctx context.Context) {
	if ec.IMAPClient == nil {
		if ec.GmailAPIInbound {
			// Modify-scope Gmail OAuth: inbound is served by the Gmail-API
			// history poller (its own goroutine, started in
			// startGmailInboundIfApplicable). There's no IMAP socket to keep
			// open, so report both connection_established and idle_started so
			// the coordinator's "Connected && IdleRunning => StateConnected"
			// rule is satisfied (the poller's ticker is the moral equivalent
			// of IMAP IDLE for this transport). Subsequent transient failures
			// inside the poller don't currently flow back to the coordinator —
			// token revocation flips the account to needs-reauth via
			// reauthAwareTokenSource which fires its own bridge state.
			ec.stateCoordinator.ReportSimpleEvent("inbox", "connection_established", true, "", nil)
			ec.stateCoordinator.ReportSimpleEvent("inbox", "idle_started", true, "", nil)
			return
		}
		ec.stateCoordinator.ReportSimpleEvent("inbox", "auth_failure", false, EmailNotLoggedIn, nil)
		return
	}

	// Emit STARTING before beginning connection work
	ec.stateCoordinator.ReportSimpleEvent("inbox", "connection_started", false, "", nil)

	// Connect to IMAP server
	if err := ec.IMAPClient.Connect(); err != nil {
		ec.UserLogin.Log.Error().Err(err).Msg("Failed to connect to IMAP server")
		ec.stateCoordinator.ReportSimpleEvent("inbox", "connection_lost", false, EmailConnectionFailed, map[string]any{"go_error": err.Error()})
		return
	}

	// We're connected to the server; emit RUNNING (service up but not yet fully ready)
	// Note: the actual connection_established event will be reported by the IMAP client

	// Start IMAP IDLE monitoring with retry logic (includes baseline/backfill)
	if err := ec.startIDLEWithRetry(); err != nil {
		ec.UserLogin.Log.Error().Err(err).Msg("Failed to start IMAP IDLE after retries")
		// Disconnect and reset state since IDLE failed
		ec.isConnected.Store(false)
		if ec.IMAPClient != nil {
			ec.IMAPClient.Disconnect()
		}
		// Report IDLE startup failure - this will be handled by the state coordinator
		ec.stateCoordinator.ReportSimpleEvent("inbox", string(coordinator.EventIdleFailed), false, imap.EmailIdleFailed, map[string]any{"go_error": err.Error(), "error_type": "IDLE_startup_failed"})
		return
	}

	// Only mark as connected after IDLE successfully starts
	ec.isConnected.Store(true)

	// Start background loops now that IDLE is running and baseline/backfill are done
	ec.startLoops()

	// Report successful IDLE startup - the coordinator will promote to CONNECTED
	ec.stateCoordinator.ReportSimpleEvent("inbox", "idle_started", true, "", nil)

	ec.UserLogin.Log.Info().Msg("Email client connected successfully and ready (baseline/backfill complete)")
}

func (ec *EmailClient) Disconnect() {
	ec.isConnected.Store(false)

	// Stop background loops
	if cancel := ec.stopLoops.Swap(nil); cancel != nil {
		(*cancel)()
	}

	// Stop IMAP monitoring and disconnect
	ec.disconnectIMAPClient()

	// Cancel client context to clean up any remaining goroutines
	if ec.cancel != nil {
		ec.cancel()
	}

	ec.UserLogin.Log.Info().Msg("Email client disconnected")
}

func (ec *EmailClient) LogoutRemote(ctx context.Context) {
	ec.UserLogin.Log.Info().Msg("Logging out from email account")

	// Remove account from database
	if err := ec.Main.DB.DeleteAccount(ctx, ec.UserLogin.UserMXID.String(), ec.Email); err != nil {
		ec.UserLogin.Log.Error().Err(err).Msg("Failed to delete account from database")
	}

	// Disconnect gracefully - this handles context cancellation and cleanup
	ec.Disconnect()

	// Remove from IMAP manager
	if err := ec.Main.IMAPManager.RemoveAccount(ec.UserLogin.UserMXID.String(), ec.Email); err != nil {
		ec.UserLogin.Log.Error().Err(err).Msg("Failed to remove account from IMAP manager")
	}

	// Clear credentials - safe after Disconnect() has cancelled all operations
	ec.Password = ""
	ec.IMAPClient = nil

	// CRITICAL: Delete the UserLogin record from bridgev2 database
	// This is what actually removes the login from the bridge framework
	logoutState := status.BridgeState{
		StateEvent: status.StateLoggedOut,
		Source:     "bridge",
	}
	ec.UserLogin.Delete(ctx, logoutState, bridgev2.DeleteOpts{})
	ec.UserLogin.Log.Debug().Msg("Successfully deleted UserLogin from bridge database")

	ec.UserLogin.Log.Info().Msg("Successfully logged out from email account")
}

// IsLoggedIn reports whether this account has valid credentials, per
// bridgev2's contract (see networkinterface.go: "should not do any IO
// operations"). It's a credential-state check, not a connection-state check.
//
// Two valid transports indicate logged-in credentials:
//   - IMAPClient (IMAP IDLE for password and full-mode OAuth — built only
//     after credentials are accepted by the IMAP server)
//   - Sender (Gmail API for modify-mode OAuth — built only when an OAuth
//     token is present and the account isn't flagged for re-auth, see
//     buildSender)
//
// Without this OR, modify-mode Gmail accounts (no IMAP, Gmail-API-only) would
// always look "not logged in" because IMAPClient is nil by design — and
// FindPreferredLogin would refuse to route outbound Matrix events through
// the portal, surfacing "You're not logged in" on every compose-room send.
func (ec *EmailClient) IsLoggedIn() bool {
	if ec.IMAPClient != nil {
		return true
	}
	return ec.Sender != nil
}

func (ec *EmailClient) IsConnected() bool {
	return ec.isConnected.Load()
}

// disconnectIMAPClient safely stops IDLE and disconnects the IMAP client
func (ec *EmailClient) disconnectIMAPClient() {
	if ec.IMAPClient != nil {
		ec.IMAPClient.StopIDLE()
		ec.IMAPClient.Disconnect()
	}
}

// Stop gracefully stops all client operations and cleans up resources
func (ec *EmailClient) Stop(ctx context.Context) {
	ec.UserLogin.Log.Info().Msg("Stopping email client")

	// Mark as disconnected first to prevent new items being queued
	ec.isConnected.Store(false)

	// Stop all background loops
	if cancel := ec.stopLoops.Swap(nil); cancel != nil {
		(*cancel)()
		// Background operations will terminate cleanly via context cancellation
	}

	// Disconnect from IMAP server
	ec.disconnectIMAPClient()

	// Cancel client context to clean up any remaining goroutines
	if ec.cancel != nil {
		ec.cancel()
	}

	ec.UserLogin.Log.Info().Msg("Email client stopped")
}

// startIDLEWithRetry attempts to start IDLE with robust retry logic and timeout protection
func (ec *EmailClient) startIDLEWithRetry() error {
	// Get IMAP timeout configuration for timeout protection
	timeoutConfig := reliability.IMAPTimeouts()

	// Add timeout for the entire IDLE startup sequence
	timeoutCtx, cancel := context.WithTimeout(ec.ctx, timeoutConfig.Command)
	defer cancel()

	return reliability.RetryWithBackoff(timeoutCtx, reliability.IDLEStartupRetryConfig(), func() error {
		// Test connection health first
		if err := ec.IMAPClient.TestConnection(); err != nil {
			ec.UserLogin.Log.Warn().Err(err).Msg("Connection test failed, will trigger reconnection via retry system")
			// Let the retry system handle connection recovery - this will trigger reconnectIMAPClient
			return err
		}

		// Attempt to start IDLE
		if err := ec.IMAPClient.StartIDLE(); err != nil {
			// Let the retry system categorize and decide whether to retry
			// This includes IDLE conflicts, server errors, and network issues
			ec.UserLogin.Log.Debug().Err(err).Msg("IDLE startup failed, letting retry system handle recovery")

			// For IDLE conflicts, force reconnection before next retry
			if strings.Contains(strings.ToLower(err.Error()), "idle already") ||
				strings.Contains(strings.ToLower(err.Error()), "already running") {
				if reconErr := ec.reconnectIMAPClient(); reconErr != nil {
					ec.UserLogin.Log.Warn().Err(reconErr).Msg("Failed to reconnect during IDLE conflict recovery")
					// Return original error, not reconnection error, for retry system categorization
				} else {
					ec.UserLogin.Log.Debug().Msg("Reconnected successfully to clear IDLE conflict")
				}
			}

			return err
		}

		ec.UserLogin.Log.Info().Msg("IDLE started successfully")
		return nil
	})
}

// reconnectIMAPClient performs a full disconnect and reconnect
func (ec *EmailClient) reconnectIMAPClient() error {
	ec.UserLogin.Log.Info().Msg("Reconnecting IMAP client to clear server-side state")

	// Stop IDLE first if it's running
	ec.disconnectIMAPClient()

	// Wait for server-side cleanup
	time.Sleep(ec.config.ReconnectionSleepTime)

	// Reconnect
	if err := ec.IMAPClient.Connect(); err != nil {
		return fmt.Errorf("failed to reconnect IMAP: %w", err)
	}

	ec.UserLogin.Log.Info().Msg("IMAP reconnection successful")
	return nil
}

func (ec *EmailClient) startLoops() {
	ctx, cancel := context.WithCancel(ec.ctx)
	ec.stopLoops.Store(&cancel)

	// Start periodic sync checker
	go ec.periodicSyncLoop(ctx)
}

func (ec *EmailClient) periodicSyncLoop(ctx context.Context) {
	ticker := time.NewTicker(ec.config.PeriodicSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ec.performPeriodicSync(ctx)
		}
	}
}

func (ec *EmailClient) performPeriodicSync(ctx context.Context) {
	// Check if context is cancelled before proceeding
	select {
	case <-ctx.Done():
		ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] Context cancelled, skipping sync")
		return
	default:
	}

	ec.historySyncMutex.Lock()
	defer ec.historySyncMutex.Unlock()

	// Check if we need to perform a sync
	if time.Since(ec.lastSyncTime) < ec.config.SyncThrottleTime {
		ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] Skipping sync - too soon since last sync")
		return
	}

	ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] Starting periodic sync")

	// Add timeout protection for periodic sync operations
	timeoutConfig := reliability.IMAPTimeouts()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeoutConfig.Command)
	defer cancel()

	var err error
	func() {
		// Capture IMAP client reference safely to prevent nil pointer panic
		imapClient := ec.IMAPClient
		if imapClient != nil && imapClient.IsConnected() {
			if imapClient.IsIDLERunning() {
				ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] IDLE is running - triggering manual check (will interrupt IDLE)")
			} else {
				ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] IDLE not running - safe to check messages")
			}

			// Check if timeout context is cancelled
			select {
			case <-timeoutCtx.Done():
				err = timeoutCtx.Err()
				return
			default:
			}

			// Perform message check with timeout protection
			if syncErr := imapClient.CheckNewMessages(); syncErr != nil {
				err = fmt.Errorf("message check failed: %w", syncErr)
			} else {
				ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] Message check completed successfully")
			}
		} else {
			ec.UserLogin.Log.Warn().Msg("[PERIODIC SYNC] IMAP client not available or not connected")
		}
	}()

	if err != nil {
		ec.UserLogin.Log.Warn().Err(err).Msg("[PERIODIC SYNC] Failed to check for new messages during periodic sync with timeout protection")
	}

	// Update sync time regardless of success/failure to maintain sync interval
	ec.lastSyncTime = time.Now()

	ec.UserLogin.Log.Debug().Msg("[PERIODIC SYNC] Periodic sync completed")
}

// GetCapabilities returns the room-level feature map exposed by matrimail to
// the bridgev2 framework. The framework's Portal.checkMessageContentCaps
// consults File BEFORE invoking HandleMatrixMessage and hard-rejects media
// msgtypes that aren't declared here, so all four (image/file/video/audio)
// must appear or media simply won't reach the connector.
//
// Edits, deletes, reactions, polls, and locations are all rejected — email
// has no equivalent semantics. Threads are unsupported because matrimail
// already maps each conversation to its own portal/room. Replies are fully
// supported (and produce In-Reply-To/References headers).
func (ec *EmailClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	const maxAttachmentBytes int64 = 25 * 1024 * 1024 // Gmail's per-message attachment ceiling

	return &event.RoomFeatures{
		ID: "matrimail-email-v1",
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:           event.CapLevelFullySupported,
			event.FmtItalic:         event.CapLevelFullySupported,
			event.FmtUnderline:      event.CapLevelFullySupported,
			event.FmtStrikethrough:  event.CapLevelFullySupported,
			event.FmtInlineCode:     event.CapLevelFullySupported,
			event.FmtCodeBlock:      event.CapLevelFullySupported,
			event.FmtBlockquote:     event.CapLevelFullySupported,
			event.FmtInlineLink:     event.CapLevelFullySupported,
			event.FmtUnorderedList:  event.CapLevelFullySupported,
			event.FmtOrderedList:    event.CapLevelFullySupported,
			event.FmtHeaders:        event.CapLevelFullySupported,
			event.FmtHorizontalLine: event.CapLevelFullySupported,
			event.FmtTable:          event.CapLevelPartialSupport,
			event.FmtCustomEmoji:    event.CapLevelDropped,
			event.FmtSpoiler:        event.CapLevelDropped,
		},
		File: event.FileFeatureMap{
			event.MsgImage: {
				MimeTypes: map[string]event.CapabilitySupportLevel{"image/*": event.CapLevelFullySupported},
				MaxSize:   maxAttachmentBytes,
				Caption:   event.CapLevelFullySupported,
			},
			event.MsgFile: {
				MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
				MaxSize:   maxAttachmentBytes,
				Caption:   event.CapLevelFullySupported,
			},
			event.MsgVideo: {
				MimeTypes: map[string]event.CapabilitySupportLevel{"video/*": event.CapLevelFullySupported},
				MaxSize:   maxAttachmentBytes,
			},
			event.MsgAudio: {
				MimeTypes: map[string]event.CapabilitySupportLevel{"audio/*": event.CapLevelFullySupported},
				MaxSize:   maxAttachmentBytes,
			},
		},
		Reply:           event.CapLevelFullySupported,
		Edit:            event.CapLevelRejected,
		Delete:          event.CapLevelRejected,
		Reaction:        event.CapLevelRejected,
		Thread:          event.CapLevelUnsupported,
		LocationMessage: event.CapLevelRejected,
		Poll:            event.CapLevelRejected,
	}
}

func (ec *EmailClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	// Check if this user ID corresponds to our email account
	expectedUserID := MakeUserID(ec.Email)
	return userID == expectedUserID
}

// GetChatInfo implements the NetworkAPI interface - delegates to connector
func (ec *EmailClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return ec.Main.GetChatInfo(ctx, portal, ec.UserLogin, networkid.PortalKey{ID: portal.ID})
}

// GetUserInfo implements the NetworkAPI interface - delegates to connector
func (ec *EmailClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	return ec.Main.GetUserInfo(ctx, ghost)
}

// HandleMatrixMessage implements the NetworkAPI interface. The full outbound
// flow lives in client_send.go for review hygiene; this is a thin delegator.
func (ec *EmailClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return ec.handleMatrixMessageOutbound(ctx, msg)
}
