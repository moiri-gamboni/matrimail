package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Leicas/matrimail/pkg/email"
	"github.com/Leicas/matrimail/pkg/imap"
)

// Login flow IDs registered with bridgev2.
const (
	LoginFlowIDPassword   = "email-password"
	LoginFlowIDOAuthGmail = "email-oauth-gmail"
)

// EmailLoginProcess represents the email login flow.
//
// Two flows share this struct:
//
//   - LoginFlowIDPassword: classic IMAP-app-password flow. Steps:
//     credentials → folder_selection → confirmation → complete.
//   - LoginFlowIDOAuthGmail: Authorization-Code + PKCE + RFC 8252 loopback
//     flow. Steps: oauth_email → oauth_wait → folder_selection →
//     confirmation → complete. The browser redirect lands on a transient
//     127.0.0.1 listener owned by EmailLoginProcess; the framework's
//     LoginProcessDisplayAndWait.Wait() blocks until the listener delivers a
//     code (or the listener times out / errors).
//
// flowID is set by EmailConnector.CreateLogin from the user's flow pick.
type EmailLoginProcess struct {
	user      *bridgev2.User
	connector *EmailConnector
	flowID    string // see LoginFlowID* constants
	email     string
	username  string
	password  string

	// Multi-step flow state
	currentStep      string            // "credentials", "folder_selection", "confirmation"
	availableFolders []imap.FolderInfo // Folders enumerated after credential validation
	selectedFolders  []imap.FolderInfo // User's folder selection
	selectedNames    []string          // Raw IMAP folder names for storage
	providerName     string            // Detected provider name for display
	testClient       *imap.Client      // Validated IMAP client (reused for folder listing)

	// OAuth-flow state — only populated for LoginFlowIDOAuthGmail.
	//
	// listener is the loopback HTTP listener serving Google's redirect; the
	// framework calls Wait() while it's running, and we close it from
	// Cancel() and from the success path. pkceVerifier and state are
	// generated once at oauth_email-step entry and consumed when exchanging
	// the code. scopeMode controls which scope set the auth URL requested.
	listener     *OAuthListener
	pkceVerifier string
	state        string
	scopeMode    string // ScopeModeModify (default) or ScopeModeFull
}

var (
	_ bridgev2.LoginProcess               = (*EmailLoginProcess)(nil)
	_ bridgev2.LoginProcessUserInput      = (*EmailLoginProcess)(nil)
	_ bridgev2.LoginProcessDisplayAndWait = (*EmailLoginProcess)(nil)
)

// EmailLoginMetadata contains email-specific login metadata
type EmailLoginMetadata struct {
	Email    string `json:"email"`
	Username string `json:"username"`
}

// Start begins the login process. The first prompt depends on flowID:
// for LoginFlowIDOAuthGmail we ask for the email up front (so we know which
// account the OAuth code authorizes), then build the auth URL and spin up the
// loopback callback listener.
func (elp *EmailLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	if elp.flowID == LoginFlowIDOAuthGmail {
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       "oauth_email",
			Instructions: elp.buildOAuthEmailPrompt(),
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{
					{
						Type:        bridgev2.LoginInputFieldTypeEmail,
						ID:          "email",
						Name:        "Email Address",
						Description: "Your Gmail / Workspace address",
					},
					{
						// Free-text optional override — empty string means "use the configured default".
						Type:        bridgev2.LoginInputFieldTypeUsername,
						ID:          "scope_mode",
						Name:        "Scope mode (optional)",
						Description: "Leave blank for default. Type 'modify' for Gmail-API-only (recommended) or 'full' for IMAP/SMTP XOAUTH2 (advanced).",
					},
				},
			},
		}, nil
	}
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "credentials",
		Instructions: elp.buildLoginInstructions(),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeEmail,
					ID:          "email",
					Name:        "Email Address",
					Description: "Your full email address",
				},
				{
					Type:        bridgev2.LoginInputFieldTypePassword,
					ID:          "password",
					Name:        "Password",
					Description: "Your email password or App Password",
				},
			},
		},
	}, nil
}

// buildOAuthEmailPrompt is the first-step UI for LoginFlowIDOAuthGmail. We
// only need the email here; the OAuth dance happens after.
func (elp *EmailLoginProcess) buildOAuthEmailPrompt() string {
	defaultMode := elp.connector.Config.GmailOAuth.EffectiveDefaultScopeMode()
	return `**Matrimail — Gmail OAuth Login**

🔐 Sign in with your Google account, no app password required.

**Step 1 of 3** — enter the Gmail / Google Workspace address you want to bridge.

When you submit, the bridge will hand back a single URL. Open it in any browser
(your phone, laptop, anywhere), sign in to Google, and authorize matrimail. The
bridge will detect the authorization and continue automatically.

**Scope modes:**
- ` + "`modify`" + ` (recommended): uses the Gmail API for both reading and sending. Sensitive
  scope, no CASA assessment required, supports long-lived refresh tokens.
- ` + "`full`" + ` (advanced): uses IMAP/SMTP XOAUTH2 via the ` + "`mail.google.com`" + ` scope.
  Restricted scope — your OAuth project will be locked into Google's "Testing"
  publishing status, which means refresh tokens **expire every 7 days**. Pick this
  only if you need IMAP semantics (e.g. your Workspace admin disabled the Gmail API).

Default mode on this bridge: **` + defaultMode + `**.

**Running matrimail on a remote server?** Set up an SSH local port-forward
**before** opening the URL:

` + "```\nssh -L 8888:127.0.0.1:8888 user@your-bridge-host\n```" + `

…and configure ` + "`gmail_oauth.listener_address: 127.0.0.1:8888`" + ` in your
matrimail config.

If you cannot expose a browser-reachable port at all, ` + "`!matrimail oauth paste-token`" + `
is a last resort: it works, but the refresh token you type is long-lived,
grants your whole mailbox, and ends up in that room's history.

*Need help?* ` + "`!matrimail help`"
}

// buildLoginInstructions creates helpful login instructions based on common email providers
func (elp *EmailLoginProcess) buildLoginInstructions() string {
	return `**Matrimail Login**

📧 **Please enter your email credentials using the form fields below.**

**Important Notes:**
• For Gmail/Yahoo/Outlook: Use an **App Password** (not your regular password)
• The bridge will automatically detect your email provider settings
• Your password will be encrypted and stored securely

**App Password Setup Guide:**
📱 **Gmail:** Settings → Security → 2-Step Verification → App passwords
📱 **Yahoo:** Account Info → Account security → Generate app password  
📱 **Outlook:** Security → Sign-in options → App passwords
📱 **iCloud:** Sign-In and Security → App-Specific Passwords

**Popular Providers Supported:**
Gmail, Yahoo, Outlook, iCloud, FastMail - Auto-configured
Custom IMAP servers - Auto-detected

*The bridge will test your IMAP connection automatically after you submit your credentials.*

**Need help?** Use ` + "`!matrimail help`" + ` for more information or ` + "`!matrimail status`" + ` to check connection status.`
}

// Cancel cancels the login process and tears down any in-flight OAuth
// loopback listener so a re-issued !matrimail login doesn't leak a port or a
// goroutine.
func (elp *EmailLoginProcess) Cancel() {
	if elp.listener != nil {
		elp.connector.unregisterOAuthListener(elp.user.MXID.String(), elp.listener)
		elp.listener.Close()
	}
}

// SubmitUserInput handles a login step submission. Routing depends on the
// currentStep, which is advanced explicitly by each handler. The OAuth flow
// shares folder_selection / confirmation with the password flow once the
// token is in hand.
func (elp *EmailLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	switch elp.currentStep {
	case "oauth_wait":
		return elp.handleOAuthWait(ctx, input)
	case "folder_selection":
		return elp.handleFolderSelection(ctx, input)
	case "confirmation":
		return elp.handleConfirmation(ctx, input)
	default:
		if elp.flowID == LoginFlowIDOAuthGmail {
			return elp.handleOAuthEmail(ctx, input)
		}
		return elp.handleCredentials(ctx, input)
	}
}

// handleCredentials validates credentials and proceeds to folder selection
func (elp *EmailLoginProcess) handleCredentials(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Extract credentials from user input data
	for key, value := range input {
		switch key {
		case "email":
			elp.email = strings.TrimSpace(value)
		case "password":
			elp.password = strings.TrimSpace(value)
		}
	}

	// Set username to email if not provided separately
	if elp.username == "" {
		elp.username = elp.email
	}

	// Validate required fields
	if elp.email == "" {
		return nil, fmt.Errorf("email address is required")
	}
	if elp.password == "" {
		return nil, fmt.Errorf("password is required")
	}

	// Detect email provider and give specific guidance
	providerInfo := elp.detectEmailProvider()
	if providerInfo != nil {
		elp.providerName = providerInfo.Name
	} else {
		elp.providerName = "Email"
	}

	// Show provider detection results to user
	if providerInfo != nil && providerInfo.Name != "Custom Provider" {
		// Known provider detected
		elp.connector.Bridge.Log.Info().
			Str("provider", providerInfo.Name).
			Str("domain", providerInfo.Domain).
			Msg("Known provider detected, using optimized settings")
	} else {
		// Unknown provider - using auto-detection fallback
		parts := strings.Split(elp.email, "@")
		if len(parts) == 2 {
			domain := parts[1]
			fallbackHost := fmt.Sprintf("imap.%s", domain)
			elp.connector.Bridge.Log.Info().
				Str("domain", domain).
				Str("fallback_host", fallbackHost).
				Msg("Unknown provider detected, attempting auto-detection")
		}
	}

	// Test IMAP connection and keep client for folder listing
	client, err := elp.testIMAPConnectionAndKeep(ctx)
	if err != nil {
		// Provide provider-specific troubleshooting
		errorMsg := elp.buildConnectionErrorMessage(err, providerInfo)
		return nil, fmt.Errorf("%s", errorMsg)
	}
	elp.testClient = client

	// Enumerate available folders
	folders, err := client.ListFolders()
	if err != nil {
		client.Disconnect()
		elp.connector.Bridge.Log.Warn().Err(err).Msg("Failed to list folders, falling back to INBOX only")
		// Fall back to INBOX-only if folder listing fails
		elp.selectedNames = []string{"INBOX"}
		elp.selectedFolders = []imap.FolderInfo{{
			Name:    "INBOX",
			Display: "INBOX",
			Icon:    "📥",
			Type:    imap.FolderTypeStandard,
		}}
		// Skip folder selection, go directly to save
		return elp.completeLogin(ctx)
	}

	elp.availableFolders = folders
	client.Disconnect() // Close connection, will reconnect when starting monitoring

	// If no folders found, fall back to INBOX
	if len(folders) == 0 {
		elp.selectedNames = []string{"INBOX"}
		elp.selectedFolders = []imap.FolderInfo{{
			Name:    "INBOX",
			Display: "INBOX",
			Icon:    "📥",
			Type:    imap.FolderTypeStandard,
		}}
		return elp.completeLogin(ctx)
	}

	// Move to folder selection step
	elp.currentStep = "folder_selection"

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "folder_selection",
		Instructions: BuildFolderSelectionPrompt(folders, elp.providerName),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeUsername,
					ID:          "folder_selection",
					Name:        "Folder Selection",
					Description: "Enter folder number(s), 'default' for INBOX, or 'cancel'",
				},
			},
		},
	}, nil
}

// handleFolderSelection validates folder selection and proceeds to confirmation
func (elp *EmailLoginProcess) handleFolderSelection(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	_ = ctx // ctx reserved for future use
	selectionInput := ""
	for key, value := range input {
		if key == "folder_selection" {
			selectionInput = value
			break
		}
	}

	result := ValidateFolderSelection(selectionInput, elp.availableFolders)

	if result.IsCancel {
		return nil, fmt.Errorf("login cancelled by user")
	}

	if !result.Valid {
		// Show error and re-prompt
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       "folder_selection",
			Instructions: result.ErrorMessage + "\n\n" + BuildFolderSelectionPrompt(elp.availableFolders, elp.providerName),
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{
					{
						Type:        bridgev2.LoginInputFieldTypeUsername,
						ID:          "folder_selection",
						Name:        "Folder Selection",
						Description: "Enter folder number(s), 'default' for INBOX, or 'cancel'",
					},
				},
			},
		}, nil
	}

	// Store selection
	elp.selectedNames = result.SelectedNames
	elp.selectedFolders = result.SelectedInfos

	// Move to confirmation step
	elp.currentStep = "confirmation"

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "confirmation",
		Instructions: BuildConfirmationPrompt(elp.selectedFolders),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeUsername,
					ID:          "confirmation",
					Name:        "Confirmation",
					Description: "Type 'yes' to confirm, 'no' to go back, or 'cancel'",
				},
			},
		},
	}, nil
}

// handleConfirmation validates confirmation and completes login
func (elp *EmailLoginProcess) handleConfirmation(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	confirmInput := ""
	for key, value := range input {
		if key == "confirmation" {
			confirmInput = value
			break
		}
	}

	confirmed, goBack, errorMsg := ValidateConfirmation(confirmInput)

	if goBack {
		// Go back to folder selection
		elp.currentStep = "folder_selection"
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       "folder_selection",
			Instructions: BuildFolderSelectionPrompt(elp.availableFolders, elp.providerName),
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{
					{
						Type:        bridgev2.LoginInputFieldTypeUsername,
						ID:          "folder_selection",
						Name:        "Folder Selection",
						Description: "Enter folder number(s) or 'default' for INBOX",
					},
				},
			},
		}, nil
	}

	if !confirmed {
		// Show error and re-prompt
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeUserInput,
			StepID:       "confirmation",
			Instructions: errorMsg + "\n\n" + BuildConfirmationPrompt(elp.selectedFolders),
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{
					{
						Type:        bridgev2.LoginInputFieldTypeUsername,
						ID:          "confirmation",
						Name:        "Confirmation",
						Description: "Type 'yes' or 'no'",
					},
				},
			},
		}, nil
	}

	// User confirmed, complete the login
	return elp.completeLogin(ctx)
}

// completeLogin saves the account and completes the login process.
//
// For the OAuth flow we skip the full UpsertAccount (which would clobber the
// auth_type / oauth_* columns under SQLite's INSERT-OR-REPLACE semantics) and
// instead just update the monitored_folders column on the row pre-created at
// the start of the OAuth dance. The OAuth tokens were persisted by
// SaveOAuthToken in handleOAuthWait.
func (elp *EmailLoginProcess) completeLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	if elp.flowID == LoginFlowIDOAuthGmail {
		if err := elp.connector.DB.UpdateMonitoredFolders(ctx, elp.user.MXID.String(), elp.email, elp.selectedNames); err != nil {
			return nil, fmt.Errorf("failed to save folder selection: %w", err)
		}
	} else {
		// Save credentials to database FIRST (before creating user login)
		// This ensures LoadUserLogin can find the account when it's called
		if err := elp.saveAccount(ctx); err != nil {
			return nil, fmt.Errorf("failed to save account: %w", err)
		}
	}

	// Create new user login using the bridgev2 pattern
	// This will trigger LoadUserLogin which needs the account to exist in DB
	userLoginID := networkid.UserLoginID(fmt.Sprintf("email:%s", elp.email))
	userLogin, err := elp.user.NewLogin(ctx, &database.UserLogin{
		ID:         userLoginID,
		RemoteName: elp.email,
		Metadata: &EmailLoginMetadata{
			Email:    elp.email,
			Username: elp.username,
		},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// Build success message with folder info
	var folderList strings.Builder
	for _, f := range elp.selectedFolders {
		folderList.WriteString(fmt.Sprintf("\n  • %s %s %s", f.Icon, f.Display, f.TypeBracket()))
	}

	successMsg := fmt.Sprintf(`✅ **Account configured successfully!**

📧 **Email:** %s
📁 **Monitoring:**%s

Emails from the selected folder(s) will now appear in Beeper.
To change folders later, use `+"`!matrimail config folders`"+``, elp.email, folderList.String())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: successMsg,
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: userLogin.ID,
			UserLogin:   userLogin,
		},
	}, nil
}

// EmailUserLogin represents a logged-in email account
type EmailUserLogin struct {
	UserLogin *bridgev2.UserLogin
	connector *EmailConnector
	Email     string
	Password  string
	IMAPHost  string
	IMAPPort  int
	TLS       bool
}

func (eul *EmailUserLogin) Connect(ctx context.Context) error {
	// Connection is handled by the IMAP manager
	return eul.connector.IMAPManager.AddAccount(eul.UserLogin, eul.Email, eul.Email, eul.Password)
}

func (eul *EmailUserLogin) Disconnect() {
	// Disconnection is handled by the IMAP manager
	eul.connector.IMAPManager.RemoveAccount(eul.UserLogin.UserMXID.String(), eul.Email)
}

func (eul *EmailUserLogin) IsLoggedIn() bool {
	// Check if account is active in IMAP manager
	statuses := eul.connector.IMAPManager.GetAccountStatus(eul.UserLogin.UserMXID.String())
	for _, status := range statuses {
		if status.Email == eul.Email {
			return status.Connected
		}
	}
	return false
}

func (eul *EmailUserLogin) GetRemoteID() networkid.UserLoginID {
	return networkid.UserLoginID(fmt.Sprintf("email:%s", eul.Email))
}

func (eul *EmailUserLogin) GetRemoteName() string {
	return eul.Email
}

// testIMAPConnection tests the IMAP connection with provided credentials
// testIMAPConnectionAndKeep tests the IMAP connection and returns the connected client
// for subsequent operations like folder listing
func (elp *EmailLoginProcess) testIMAPConnectionAndKeep(ctx context.Context) (*imap.Client, error) {
	// Keep ctx parameter used to satisfy linters even if not currently leveraged here.
	_ = ctx
	// Create a temporary logger for testing (NO PASSWORDS LOGGED)
	logger := elp.connector.Bridge.Log.With().
		Str("component", "login_test").
		Str("email", elp.email)

	// Safely extract domain for logging
	if parts := strings.Split(elp.email, "@"); len(parts) == 2 {
		logger = logger.Str("host_detected", parts[1])
	}

	finalLogger := logger.Logger()

	// Create test IMAP client without UserLogin (just for testing connection)
	client, err := imap.NewClient(elp.email, elp.username, elp.password, nil, &finalLogger, elp.connector.Config.Logging.Sanitized, elp.connector.Config.Logging.PseudonymSecret, elp.connector.Config.Network.IMAP.StartupBackfillSeconds, elp.connector.Config.Network.IMAP.StartupBackfillMax, elp.connector.Config.Network.IMAP.InitialIdleTimeoutSeconds, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create IMAP client: %w", err)
	}

	// Test connection
	err = client.Connect()
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}

	finalLogger.Info().Msg("IMAP connection test successful")
	return client, nil
}

// saveAccount saves the email account credentials to database
func (elp *EmailLoginProcess) saveAccount(ctx context.Context) error {
	logger := elp.connector.Bridge.Log.With().
		Str("component", "login_save").
		Str("email", elp.email).
		Logger()

	logger.Info().Msg("Saving account credentials to database")

	// Auto-detect provider settings for saving
	parts := strings.Split(elp.email, "@")
	if len(parts) != 2 {
		return fmt.Errorf("invalid email format: %s", elp.email)
	}
	domain := strings.ToLower(parts[1])
	var host string
	var port int
	var tls bool

	if provider, ok := imap.CommonProviders[domain]; ok {
		host = provider.Host
		port = provider.Port
		tls = provider.TLS
	} else {
		// Default IMAP settings
		host = fmt.Sprintf("imap.%s", domain)
		port = 993
		tls = true
	}

	account := &EmailAccount{
		UserMXID:         elp.user.MXID.String(),
		Email:            elp.email,
		Username:         elp.username,
		Password:         elp.password,
		Host:             host,
		Port:             port,
		TLS:              tls,
		CreatedAt:        time.Now(),
		LastSyncTime:     time.Now(),
		MonitoredFolders: elp.selectedNames,
	}

	logger.Debug().Str("host", host).Int("port", port).Bool("tls", tls).Msg("Attempting to save account to database")

	if err := elp.connector.DB.UpsertAccount(ctx, account); err != nil {
		logger.Error().Err(err).Msg("Failed to save account credentials to database")
		return err
	}

	logger.Info().Msg("Successfully saved account credentials to database")
	return nil
}

// ProviderInfo contains information about an email provider
type ProviderInfo struct {
	Name        string
	Domain      string
	NeedsAppPwd bool
	HelpURL     string
}

// detectEmailProvider detects the email provider from the email address
func (elp *EmailLoginProcess) detectEmailProvider() *ProviderInfo {
	if elp.email == "" {
		return nil
	}

	parts := strings.Split(elp.email, "@")
	if len(parts) != 2 {
		return nil
	}

	domain := strings.ToLower(parts[1])

	switch domain {
	case "gmail.com", "googlemail.com":
		return &ProviderInfo{
			Name:        "Gmail",
			Domain:      domain,
			NeedsAppPwd: true,
			HelpURL:     "https://support.google.com/accounts/answer/185833",
		}
	case "yahoo.com", "yahoo.co.uk", "yahoo.fr", "yahoo.de":
		return &ProviderInfo{
			Name:        "Yahoo",
			Domain:      domain,
			NeedsAppPwd: true,
			HelpURL:     "https://help.yahoo.com/kb/generate-third-party-passwords-sln15241.html",
		}
	case "outlook.com", "hotmail.com", "live.com", "msn.com":
		return &ProviderInfo{
			Name:        "Outlook/Hotmail",
			Domain:      domain,
			NeedsAppPwd: true,
			HelpURL:     "https://support.microsoft.com/en-us/account-billing/using-app-passwords-with-apps-that-don-t-support-two-step-verification-5896ed9b-4263-e681-128a-a6f2979a7944",
		}
	case "icloud.com", "me.com", "mac.com":
		return &ProviderInfo{
			Name:        "iCloud",
			Domain:      domain,
			NeedsAppPwd: true,
			HelpURL:     "https://support.apple.com/en-us/HT204397",
		}
	default:
		return &ProviderInfo{
			Name:        "Custom Provider",
			Domain:      domain,
			NeedsAppPwd: false,
			HelpURL:     "",
		}
	}
}

// buildConnectionErrorMessage creates a helpful error message based on the provider
func (elp *EmailLoginProcess) buildConnectionErrorMessage(err error, provider *ProviderInfo) string {
	baseError := fmt.Sprintf("Connection failed: %v", err)

	if provider == nil {
		return baseError
	}

	// Check for common authentication errors
	errorStr := strings.ToLower(err.Error())
	isAuthError := strings.Contains(errorStr, "authentication") ||
		strings.Contains(errorStr, "login") ||
		strings.Contains(errorStr, "password") ||
		strings.Contains(errorStr, "credentials")

	if isAuthError && provider.NeedsAppPwd {
		return fmt.Sprintf(`❌ **%s Login Failed**

**Most likely cause:** You need to use an **App Password** instead of your regular password.

**How to fix:**
1. Go to your %s security settings
2. Enable 2-Factor Authentication (if not already enabled)
3. Generate an App Password specifically for this email bridge
4. Use the App Password instead of your regular password

**Help Link:** %s

**Original Error:** %v`,
			provider.Name, provider.Name, provider.HelpURL, err)
	}

	if isAuthError {
		return fmt.Sprintf(`❌ **%s Login Failed**

**Possible causes:**
• Incorrect email address or password
• Two-factor authentication enabled (you may need an App Password)
• IMAP access disabled in your email settings
• Account temporarily locked

**Please double-check:**
✓ Email address is correct
✓ Password is correct (or use App Password if 2FA is enabled)
✓ IMAP is enabled in your %s settings

**Original Error:** %v`,
			provider.Name, provider.Name, err)
	}

	// Handle custom providers (auto-detection fallback) differently
	if provider.Name == "Custom Provider" {
		parts := strings.Split(elp.email, "@")
		domain := "your email provider"
		fallbackHost := "imap.domain.com"
		if len(parts) == 2 {
			domain = parts[1]
			fallbackHost = fmt.Sprintf("imap.%s", domain)
		}

		return fmt.Sprintf(`❌ **Connection Failed - Auto-Detection Used**

🔍 **We attempted to connect using:** %s:993

**This is an unknown email provider, so we used auto-detection.**

**Possible solutions:**

**1. Check with %s for correct IMAP settings:**
• IMAP server address (might not be %s)
• Port number (usually 993 or 143)
• Security settings (SSL/TLS)
• IMAP access needs to be enabled

**2. Common IMAP settings to try:**
• mail.%s:993 (SSL)
• %s:993 (SSL)  
• %s:143 (STARTTLS)

**3. If your provider uses non-standard settings:**
Contact your email administrator or check your provider's documentation

**Original Error:** %v`,
			fallbackHost, domain, fallbackHost, domain, fallbackHost, fallbackHost, err)
	}

	// Generic connection error for known providers
	return fmt.Sprintf(`❌ **Connection to %s Failed**

**Possible causes:**
• Network connectivity issues
• Firewall blocking IMAP connections
• Email provider server temporarily unavailable
• Account settings may need adjustment

**Please try:**
✓ Check your internet connection
✓ Verify IMAP is enabled in your %s account settings
✓ Try again in a few minutes
✓ Contact your email provider if the issue persists

**Original Error:** %v`,
		provider.Name, provider.Name, err)
}

// IMAP monitoring is now handled by the EmailClient in client.go

// ----------------------------------------------------------------------------
// OAuth (Gmail) login flow — Authorization Code + PKCE + RFC 8252 loopback
// ----------------------------------------------------------------------------

// handleOAuthEmail captures the user's email + scope-mode choice, generates
// PKCE+state, spins up a transient loopback HTTP listener on 127.0.0.1, builds
// the Google authorization URL, and returns it as a DisplayAndWait step. The
// bridgev2 framework then calls Wait() (below), which blocks until the
// listener delivers a code (success) or an error (state mismatch, user
// cancelled, timeout).
func (elp *EmailLoginProcess) handleOAuthEmail(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	for k, v := range input {
		switch k {
		case "email":
			elp.email = strings.TrimSpace(v)
		case "scope_mode":
			elp.scopeMode = strings.ToLower(strings.TrimSpace(v))
		}
	}
	if elp.email == "" {
		return nil, fmt.Errorf("email address is required")
	}
	if elp.username == "" {
		elp.username = elp.email
	}

	// Resolve scope mode: empty input → bridge default; otherwise validate.
	switch elp.scopeMode {
	case "":
		elp.scopeMode = elp.connector.Config.GmailOAuth.EffectiveDefaultScopeMode()
	case ScopeModeFull, ScopeModeModify:
		// ok
	default:
		return nil, fmt.Errorf("scope_mode must be empty, 'modify', or 'full' (got %q)", elp.scopeMode)
	}

	cfg := elp.connector.Config.GmailOAuth
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("gmail_oauth.client_id / client_secret not configured on this bridge — see docs/gmail-oauth-setup.md, or use the email-password flow")
	}

	// Pre-create the account row so SaveOAuthTokenWithScope (which UPDATEs in
	// place) finds something to write into. The row will be filled in with
	// the real OAuth token after the callback fires; if the user abandons
	// the flow, the row stays in password mode with an empty password —
	// harmless, and !matrimail logout cleans it up.
	parts := strings.Split(elp.email, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid email format: %s", elp.email)
	}
	domain := strings.ToLower(parts[1])
	host := fmt.Sprintf("imap.%s", domain)
	port := 993
	tls := true
	if provider, ok := imap.CommonProviders[domain]; ok {
		host = provider.Host
		port = provider.Port
		tls = provider.TLS
	}
	account := &EmailAccount{
		UserMXID:         elp.user.MXID.String(),
		Email:            elp.email,
		Username:         elp.username,
		Password:         "", // unused under OAuth; encryptString accepts empty
		Host:             host,
		Port:             port,
		TLS:              tls,
		CreatedAt:        time.Now(),
		LastSyncTime:     time.Now(),
		MonitoredFolders: []string{"INBOX"},
	}
	if err := elp.connector.DB.UpsertAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("failed to pre-create account row: %w", err)
	}

	// Generate PKCE verifier + state and stash both on the login process so
	// finishOAuthWait can pass them to ExchangeCode.
	verifier, challenge, err := email.GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := email.GenerateState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	elp.pkceVerifier = verifier
	elp.state = state

	// Spin up the loopback listener BEFORE building the auth URL so we have
	// the actual bound port for the redirect_uri.
	listener, err := StartOAuthListener(
		cfg.EffectiveListenerAddress(),
		state,
		cfg.EffectiveCallbackTimeout(),
	)
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback listener: %w", err)
	}
	elp.listener = listener
	// Register so `!matrimail oauth paste-code` can find this listener.
	elp.connector.registerOAuthListener(elp.user.MXID.String(), listener)

	scopes := email.ScopesForMode(elp.scopeMode)
	authURL, err := email.BuildAuthURL(
		email.GmailOAuthConfig{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret},
		listener.RedirectURI(),
		state,
		challenge,
		elp.email, // login_hint pre-fills Google's account chooser
		scopes,
	)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("build auth URL: %w", err)
	}

	elp.currentStep = "oauth_wait"
	scopeNote := ""
	if elp.scopeMode == ScopeModeFull {
		scopeNote = "\n⚠️ **Full mode**: refresh tokens issued by an unverified Google project in Testing status expire after 7 days. You'll need to re-run `!matrimail login` weekly unless you publish your OAuth project (which for `mail.google.com` requires Google verification + CASA assessment)."
	}
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeDisplayAndWait,
		StepID: "oauth_wait",
		Instructions: fmt.Sprintf(`🔐 **Step 2 of 3 — Authorize matrimail with Google**

Open this URL in any browser (phone, laptop, doesn't matter — it just needs to
reach **%s** on this host):

%s

Sign in as **%s** and authorize matrimail. The bridge will detect the redirect
automatically and continue. This URL expires in %s.

Scope mode: **%s**.%s

🚧 *Headless server tip:* if your matrimail host has no browser, set up an SSH
local port-forward from your workstation **before** opening the URL — see the
prompt from Step 1.

🆘 *No SSH access either?* Open the URL anyway. After you click "Allow", your
browser will fail to load the redirect ("can't connect to %s") — but the
URL bar will contain the authorization code. Copy the **full URL** from the
address bar and run:

    !matrimail oauth paste-code <paste-the-url-here>

The bridge has the matching state and PKCE verifier in memory and will finish
the exchange.`,
			listener.RedirectURI(),
			authURL,
			elp.email,
			cfg.EffectiveCallbackTimeout().String(),
			elp.scopeMode,
			scopeNote,
			listener.RedirectURI(),
		),
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type: bridgev2.LoginDisplayTypeCode,
			Data: authURL,
		},
	}, nil
}

// Wait satisfies bridgev2.LoginProcessDisplayAndWait. The framework calls
// this after presenting the auth URL to the user; we block until the loopback
// listener delivers a code (or fails) and then transition into folder
// selection without further user input.
//
// Context cancellation aborts the wait — typically only happens if the user
// cancels the login from their client.
func (elp *EmailLoginProcess) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	if elp.listener == nil {
		return nil, errors.New("OAuth wait called before listener start; this is a bridge bug")
	}
	code, err := elp.listener.Wait(ctx)
	// Always release the port and drop the registry entry — listener has
	// already shut down on success but Close() is idempotent and the registry
	// entry would otherwise dangle until the next login.
	defer func() {
		elp.connector.unregisterOAuthListener(elp.user.MXID.String(), elp.listener)
		elp.listener.Close()
	}()
	if err != nil {
		return nil, fmt.Errorf("OAuth authorization failed: %w", err)
	}
	return elp.finishOAuthExchange(ctx, code)
}

// handleOAuthWait is a defensive fallback for clients that submit input
// against the oauth_wait step ID instead of waiting on the listener. We
// just check whether the listener has produced a code and either advance or
// re-display the auth URL.
func (elp *EmailLoginProcess) handleOAuthWait(ctx context.Context, _ map[string]string) (*bridgev2.LoginStep, error) {
	if elp.listener == nil {
		return nil, errors.New("OAuth wait called before listener start; this is a bridge bug")
	}
	// Non-blocking peek: the listener delivers exactly once on success.
	cleanup := func() {
		elp.connector.unregisterOAuthListener(elp.user.MXID.String(), elp.listener)
		elp.listener.Close()
	}
	select {
	case code := <-elp.listenerCodeCh():
		defer cleanup()
		return elp.finishOAuthExchange(ctx, code)
	case err := <-elp.listenerErrCh():
		defer cleanup()
		return nil, fmt.Errorf("OAuth authorization failed: %w", err)
	default:
		// Still waiting — re-show a "still waiting" message.
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeDisplayAndWait,
			StepID:       "oauth_wait",
			Instructions: "⏳ Still waiting for you to authorize in your browser. Open the URL from the previous message and finish the OAuth flow.",
			DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
				Type: bridgev2.LoginDisplayTypeCode,
				Data: elp.listener.RedirectURI(),
			},
		}, nil
	}
}

// listenerCodeCh / listenerErrCh expose the listener's internal channels for
// the defensive fallback peek in handleOAuthWait. The Wait() path doesn't
// need them — it uses listener.Wait() directly.
func (elp *EmailLoginProcess) listenerCodeCh() <-chan string { return elp.listener.codeCh }
func (elp *EmailLoginProcess) listenerErrCh() <-chan error   { return elp.listener.errCh }

// finishOAuthExchange exchanges the authorization code for tokens, verifies
// the authorized identity matches the user-entered email, persists the
// token, and sets up folder selection.
func (elp *EmailLoginProcess) finishOAuthExchange(ctx context.Context, code string) (*bridgev2.LoginStep, error) {
	cfg := elp.connector.Config.GmailOAuth
	emailCfg := email.GmailOAuthConfig{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret}

	tok, err := email.ExchangeCode(ctx, emailCfg, code, elp.pkceVerifier, elp.listener.RedirectURI())
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	// Verify the authorized identity matches what the user typed by hitting
	// users.getProfile. Both `gmail.modify` and `mail.google.com` cover this.
	ts := email.TokenSource(ctx, emailCfg, tok)
	svc, err := gmail.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("create gmail service: %w", err)
	}
	prof, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("verify gmail profile: %w", err)
	}
	if !strings.EqualFold(prof.EmailAddress, elp.email) {
		return nil, fmt.Errorf("authorized account %s does not match the email you entered (%s); please restart login", prof.EmailAddress, elp.email)
	}

	// Persist token + scope mode under the existing account row.
	if err := elp.connector.DB.SaveOAuthTokenWithScope(ctx, elp.user.MXID.String(), elp.email,
		OAuthProviderGoogle, elp.scopeMode, tok); err != nil {
		return nil, fmt.Errorf("save oauth token: %w", err)
	}

	// Provider name for downstream prompts.
	if pi := elp.detectEmailProvider(); pi != nil {
		elp.providerName = pi.Name
	} else {
		elp.providerName = "Gmail"
	}

	// Folder enumeration: IMAP for full-mode, Gmail labels for modify-mode.
	folders, ferr := elp.enumerateFoldersForOAuth(ctx, svc, tok.AccessToken)
	if ferr != nil || len(folders) == 0 {
		if ferr != nil {
			elp.connector.Bridge.Log.Warn().Err(ferr).Msg("OAuth folder enumeration failed, falling back to INBOX only")
		}
		elp.selectedNames = []string{"INBOX"}
		elp.selectedFolders = []imap.FolderInfo{{
			Name: "INBOX", Display: "INBOX", Icon: "📥", Type: imap.FolderTypeStandard,
		}}
		return elp.completeLogin(ctx)
	}
	elp.availableFolders = folders
	elp.currentStep = "folder_selection"
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "folder_selection",
		Instructions: BuildFolderSelectionPrompt(folders, elp.providerName),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:        bridgev2.LoginInputFieldTypeUsername,
				ID:          "folder_selection",
				Name:        "Folder Selection",
				Description: "Enter folder number(s), 'default' for INBOX, or 'cancel'",
			}},
		},
	}, nil
}

// enumerateFoldersForOAuth picks the right folder/label discovery path based
// on scope mode. Full-mode uses IMAP (which is the only thing that scope
// authorizes that we can list folders with cheaply). Modify-mode uses the
// Gmail labels API since IMAP isn't authorized.
func (elp *EmailLoginProcess) enumerateFoldersForOAuth(ctx context.Context, svc *gmail.Service, accessToken string) ([]imap.FolderInfo, error) {
	if elp.scopeMode == ScopeModeFull {
		return elp.listFoldersOAuthIMAP(ctx, accessToken)
	}
	return ListGmailLabelsAsFolders(ctx, svc)
}

// listFoldersOAuthIMAP opens a one-shot IMAP connection authenticated via
// XOAUTH2 for folder enumeration in full-scope mode.
func (elp *EmailLoginProcess) listFoldersOAuthIMAP(ctx context.Context, accessToken string) ([]imap.FolderInfo, error) {
	_ = ctx // Connect uses its own internal timeouts
	logger := elp.connector.Bridge.Log.With().
		Str("component", "login_test_oauth").
		Str("email", elp.email).
		Logger()

	client, err := imap.NewClientOAuth(elp.email, elp.email, accessToken, nil, &logger,
		elp.connector.Config.Logging.Sanitized,
		elp.connector.Config.Logging.PseudonymSecret,
		elp.connector.Config.Network.IMAP.StartupBackfillSeconds,
		elp.connector.Config.Network.IMAP.StartupBackfillMax,
		elp.connector.Config.Network.IMAP.InitialIdleTimeoutSeconds,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create oauth IMAP client: %w", err)
	}
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("oauth IMAP connect: %w", err)
	}
	defer client.Disconnect()
	return client.ListFolders()
}
