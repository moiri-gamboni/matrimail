package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/Leicas/matrimail/pkg/email"
	"github.com/Leicas/matrimail/pkg/imap"
)

var (
	HelpSectionAuth  = commands.HelpSection{Name: "Authentication", Order: 10}
	HelpSectionInfo  = commands.HelpSection{Name: "Information", Order: 5}
	HelpSectionAdmin = commands.HelpSection{Name: "Administration", Order: 15}
)

func fnPing(ce *commands.Event) {
	ce.Reply("🏓 **Pong!** Matrimail is alive and running.")
}

func fnStatus(ce *commands.Event, connector *EmailConnector) {
	logins := ce.User.GetUserLogins()

	if len(logins) == 0 {
		ce.Reply(`
**Matrimail Status**

**Connection Status:** Not connected
**Email Accounts:** 0
**Matrix Rooms:** 0 email rooms

**Bridge is ready!** Use ` + "`!matrimail login`" + ` to connect your first email account.
`)
		return
	}

	// Get real account status from IMAP manager
	if connector == nil || connector.IMAPManager == nil {
		ce.Reply("⚠️ **Bridge Error:** IMAP manager not initialized")
		return
	}

	accountStatuses := connector.IMAPManager.GetAccountStatus(ce.User.MXID.String())

	if len(accountStatuses) == 0 {
		ce.Reply(`
**Matrimail Status**

**Connection Status:** No active email connections
**Email Accounts:** 0 monitoring
**Matrix Rooms:** 0 email rooms

**Note:** You have bridge login(s) but no active IMAP connections. Use ` + "`!matrimail login`" + ` to add email accounts.
`)
		return
	}

	// Build status report
	statusMsg := `
**Matrimail Status**

`

	connectedCount := 0
	idleCount := 0

	for _, status := range accountStatuses {
		if status.Connected {
			connectedCount++
		}
		if status.IDLEActive {
			idleCount++
		}

		var statusIcon string
		var statusText string

		if status.Connected && status.IDLEActive {
			statusIcon = "✅"
			statusText = "Connected, monitoring"
		} else if status.Connected {
			statusIcon = "🔄"
			statusText = "Connected, starting monitoring"
		} else {
			statusIcon = "❌"
			statusText = "Disconnected"
		}

		statusMsg += fmt.Sprintf("📧 %s **%s** (%s:%d) - %s\n", statusIcon, status.Email, status.Host, status.Port, statusText)
	}

	statusMsg += fmt.Sprintf(`
**Summary:**
**Email Accounts:** %d total, %d connected
**Real-time Monitoring:** %d active IMAP IDLE sessions
**Matrix Rooms:** Calculating...

`, len(accountStatuses), connectedCount, idleCount)

	if connectedCount == len(accountStatuses) && idleCount == len(accountStatuses) {
		statusMsg += "✅ **All systems operational!** Your emails are being monitored in real-time."
	} else if connectedCount > 0 {
		statusMsg += "⚠️ **Partial connectivity** - Some accounts may need attention."
	} else {
		statusMsg += "❌ **No active connections** - Use `!matrimail login` to reconnect."
	}

	ce.Reply(statusMsg)
}

// fnNuke deletes the bridge database files immediately.
// This is intended to be called by the homeserver/bridge bot during bridge removal.
func fnNuke(ce *commands.Event, connector *EmailConnector) {
	// Require explicit confirmation to avoid accidental data loss
	if len(ce.Args) == 0 || strings.ToLower(ce.Args[0]) != "confirm" {
		ce.Reply("⚠️ This will DELETE the bridge database files and cannot be undone.\nConfirm with: `!matrimail nuke confirm`.")
		return
	}
	if connector == nil {
		ce.Reply("⚠️ Bridge not initialized.")
		return
	}
	// Stop IMAP to release DB handles
	if connector.IMAPManager != nil {
		connector.IMAPManager.StopAll()
	}

	// Get current working directory for secure path resolution
	cwd, err := os.Getwd()
	if err != nil {
		ce.Reply("❌ Failed to get current directory: %s", err.Error())
		return
	}

	// Try common DB file locations using absolute paths for security.
	// Includes legacy emaildawg.db* names so users migrating from the old
	// binary can still nuke cleanly.
	relativeCandidates := []string{
		"matrimail.db",
		"matrimail.db-wal",
		"matrimail.db-shm",
		"data/matrimail.db",
		"data/matrimail.db-wal",
		"data/matrimail.db-shm",
		"sh-matrimail.db",
		"sh-matrimail.db-wal",
		"sh-matrimail.db-shm",
		"emaildawg.db",
		"emaildawg.db-wal",
		"emaildawg.db-shm",
		"data/emaildawg.db",
		"data/emaildawg.db-wal",
		"data/emaildawg.db-shm",
		"sh-emaildawg.db",
		"sh-emaildawg.db-wal",
		"sh-emaildawg.db-shm",
	}

	var candidates []string
	for _, relPath := range relativeCandidates {
		absPath := filepath.Join(cwd, relPath)
		// Validate the path is within the working directory for security
		if cleanPath := filepath.Clean(absPath); strings.HasPrefix(cleanPath, cwd) {
			candidates = append(candidates, cleanPath)
		}
	}

	removed := 0
	for _, path := range candidates {
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	if removed == 0 {
		ce.Reply("ℹ️ No bridge DB files found to delete.")
		return
	}
	ce.Reply("🧨 Bridge database deleted (%d file(s)). Please restart the bridge service.", removed)
}

func fnLogin(ce *commands.Event, connector *EmailConnector) {
	// Multi-account: a user can attach multiple email accounts. The DB primary key
	// (user_mxid, email) prevents true duplicates, and the existing list/logout
	// commands already iterate per-account. Surface the current state so the user
	// knows they're adding rather than replacing, but don't block the new login.
	logins := ce.User.GetUserLogins()
	if len(logins) > 0 {
		ce.Reply("ℹ️ You already have %d email account(s) connected — starting a new login to add another. Use `!matrimail list` to see existing accounts, or `!matrimail logout <email>` to remove one.", len(logins))
	}

	ctx := context.Background()

	// Check if the user provided arguments in the command
	args := strings.TrimSpace(ce.RawArgs)
	if args != "" {
		// Parse text arguments: email:user@domain.com password:pass or password:"quoted pass"
		email, password, err := parseLoginArgs(args)
		if err != nil {
			ce.Reply("❌ %s\n\n**Usage:** `!matrimail login email:your@email.com password:yourpassword`\n**Or:** `!matrimail login email:your@email.com password:\"password with spaces\"`", err.Error())
			return
		}

		// Process the text-based login
		err = processTextLogin(ctx, ce, email, password, connector)
		if err != nil {
			ce.Reply("❌ Login failed: %s", err.Error())
		}
		return
	}

	// Fallback to interactive login process using bridgev2 forms.
	// If Gmail OAuth is configured at the bridge level, offer a choice;
	// otherwise stay on the historical app-password flow.
	flowID := pickInteractiveLoginFlow(connector)
	connector.Bridge.Log.Info().Str("flow_id", flowID).Msg("matrimail: starting interactive login flow")

	loginProcess, err := connector.CreateLogin(ctx, ce.User, flowID)
	if err != nil {
		ce.Reply("❌ Failed to start login process: %s", err.Error())
		return
	}

	// Start the login flow
	step, err := loginProcess.Start(ctx)
	if err != nil {
		ce.Reply("❌ Failed to start login: %s", err.Error())
		return
	}

	// Dispatch the first step. For UserInput steps, this also registers a
	// CommandState that captures the user's NEXT message as input — without
	// it, the bot's command processor treats the email/password reply as
	// "Unknown command" and the multi-step login dies after step 1.
	dispatchLoginStep(ce, loginProcess, step, flowID)
}

// dispatchLoginStep replicates bridgev2/commands/login.go's unexported
// doLoginStep so the matrimail login flow can plug into the framework's
// per-user CommandState routing. Without this, the user's reply to a
// LoginStepTypeUserInput prompt gets handled by the top-level command
// processor (which doesn't know about the in-flight login) and the bot
// replies "Unknown command, use the help command for help."
//
// Recursive: each step's submit handler calls back here for the next step.
func dispatchLoginStep(ce *commands.Event, lp bridgev2.LoginProcess, step *bridgev2.LoginStep, flowID string) {
	if step.Instructions != "" {
		// On the first prompt of the password flow, also append our
		// app-password help block (Gmail/Yahoo/iCloud setup tips). For OAuth
		// the step's own Instructions are sufficient.
		if flowID == LoginFlowIDPassword && step.Type == bridgev2.LoginStepTypeUserInput {
			ce.Reply(buildEnhancedLoginInstructions(step.Instructions))
		} else {
			ce.Reply(step.Instructions)
		}
	}

	switch step.Type {
	case bridgev2.LoginStepTypeUserInput:
		uip, ok := lp.(bridgev2.LoginProcessUserInput)
		if !ok {
			ce.Reply("❌ login flow returned a UserInput step but the login process doesn't implement LoginProcessUserInput")
			return
		}
		fields := step.UserInputParams.Fields
		data := make(map[string]string, len(fields))
		var prompt func(remaining []bridgev2.LoginInputDataField)
		prompt = func(remaining []bridgev2.LoginInputDataField) {
			if len(remaining) == 0 {
				next, err := uip.SubmitUserInput(ce.Ctx, data)
				if err != nil {
					ce.Reply("❌ Failed to submit input: %v", err)
					return
				}
				commands.StoreCommandState(ce.User, nil)
				dispatchLoginStep(ce, lp, next, flowID)
				return
			}
			field := remaining[0]
			if field.Description != "" {
				ce.Reply("Please enter your %s\n%s", field.Name, field.Description)
			} else {
				ce.Reply("Please enter your %s", field.Name)
			}
			commands.StoreCommandState(ce.User, &commands.CommandState{
				Action: "Login",
				Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
					field.FillDefaultValidate()
					if field.Type == bridgev2.LoginInputFieldTypePassword || field.Type == bridgev2.LoginInputFieldTypeToken {
						ce.Redact()
					}
					val, err := field.Validate(ce.RawArgs)
					if err != nil {
						ce.Reply("❌ Invalid value for %s: %v", field.Name, err)
						prompt(remaining)
						return
					}
					data[field.ID] = val
					prompt(remaining[1:])
				}),
				Cancel: func() {
					_, _ = uip.Cancel, uip // intentional: Cancel signature varies; rely on framework cleanup
				},
			})
		}
		// First field: collect immediately. The step's own Instructions
		// (already sent above) act as the "ask"; we only need promptNext to
		// register the CommandState.Next handler.
		commands.StoreCommandState(ce.User, &commands.CommandState{
			Action: "Login",
			Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
				field := fields[0]
				field.FillDefaultValidate()
				if field.Type == bridgev2.LoginInputFieldTypePassword || field.Type == bridgev2.LoginInputFieldTypeToken {
					ce.Redact()
				}
				val, err := field.Validate(ce.RawArgs)
				if err != nil {
					ce.Reply("❌ Invalid value for %s: %v", field.Name, err)
					return
				}
				data[field.ID] = val
				if len(fields) > 1 {
					prompt(fields[1:])
					return
				}
				next, err := uip.SubmitUserInput(ce.Ctx, data)
				if err != nil {
					ce.Reply("❌ Failed to submit input: %v", err)
					return
				}
				commands.StoreCommandState(ce.User, nil)
				dispatchLoginStep(ce, lp, next, flowID)
			}),
		})

	case bridgev2.LoginStepTypeDisplayAndWait:
		daw, ok := lp.(bridgev2.LoginProcessDisplayAndWait)
		if !ok {
			ce.Reply("❌ login flow returned a DisplayAndWait step but the login process doesn't implement LoginProcessDisplayAndWait")
			return
		}
		ctx, cancel := context.WithCancel(ce.Ctx)
		commands.StoreCommandState(ce.User, &commands.CommandState{
			Action: "Login",
			Cancel: cancel,
		})
		go func() {
			defer cancel()
			next, err := daw.Wait(ctx)
			commands.StoreCommandState(ce.User, nil)
			if err != nil {
				ce.Reply("❌ Login wait failed: %v", err)
				return
			}
			dispatchLoginStep(ce, lp, next, flowID)
		}()

	case bridgev2.LoginStepTypeComplete:
		commands.StoreCommandState(ce.User, nil)
		// Framework already records the new login; just confirm.
		ce.Reply("✅ Login complete.")

	default:
		ce.Reply("❌ Unknown login step type: %s", step.Type)
	}
}

// pickInteractiveLoginFlow returns LoginFlowIDOAuthGmail when the bridge is
// configured for Gmail OAuth, otherwise LoginFlowIDPassword. The bot command
// historically hardcoded the password flow, which left OAuth-configured
// bridges without a way to start the OAuth dance from a text command.
//
// Heuristic: prefer OAuth iff the admin has wired up gmail_oauth credentials.
// We can't sniff the user's email here (the form will collect that next),
// so this is a bridge-wide default; users on non-Gmail domains will be
// gracefully steered back to the password flow by handleOAuthEmail's email
// validation if they end up at the OAuth prompt by mistake.
func pickInteractiveLoginFlow(connector *EmailConnector) string {
	cfg := connector.Config.GmailOAuth
	if cfg.ClientID != "" && cfg.ClientSecret != "" {
		return LoginFlowIDOAuthGmail
	}
	return LoginFlowIDPassword
}

func fnLogout(ce *commands.Event, connector *EmailConnector) {
	_ = connector // parameter kept for API consistency
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` to get started.")
		return
	}

	// Check if user specified an email to logout from
	if len(ce.Args) > 0 {
		emailAddr := ce.Args[0]
		ce.Reply("🔌 Disconnecting from **%s**...", emailAddr)

		// Find the specific login for this email
		var targetLogin *bridgev2.UserLogin
		for _, login := range logins {
			if client, ok := login.Client.(*EmailClient); ok && client.Email == emailAddr {
				targetLogin = login
				break
			}
		}

		if targetLogin == nil {
			ce.Reply("❌ Email account **%s** not found in your connected accounts.", emailAddr)
			return
		}

		// Use LogoutRemote for proper cleanup
		ctx := context.Background()
		if client, ok := targetLogin.Client.(*EmailClient); ok {
			client.LogoutRemote(ctx)
			ce.Reply("✅ Successfully disconnected from **%s**", emailAddr)
		} else {
			ce.Reply("❌ Failed to disconnect from **%s**: invalid client type", emailAddr)
		}
		return
	}

	// Logout all accounts
	ce.Reply("🔌 Disconnecting from all %d email account(s)...", len(logins))

	// Use LogoutRemote for each login for proper cleanup
	ctx := context.Background()
	var failures []string

	for _, login := range logins {
		if client, ok := login.Client.(*EmailClient); ok {
			try := func() {
				client.LogoutRemote(ctx)
			}

			// Use a simple panic recovery to catch any logout failures
			func() {
				email := client.Email // Capture to avoid future capture pitfalls
				defer func() {
					if msg := recover(); msg != nil {
						failures = append(failures, fmt.Sprintf("LogoutRemote for %s: panic %v (%T)", email, msg, msg))
					}
				}()
				try()
			}()
		} else {
			failures = append(failures, fmt.Sprintf("Invalid client type for login %s", login.ID))
		}
	}

	if len(failures) == 0 {
		ce.Reply("✅ Successfully disconnected from all email accounts.")
	} else {
		ce.Reply("⚠️ Logout completed with some issues:\n• %s", strings.Join(failures, "\n• "))
	}
}

func fnList(ce *commands.Event, connector *EmailConnector) {
	// Check if user has any active logins
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply(`
📭 **No email accounts connected**

To get started:
1. Use ` + "`!matrimail login`" + ` to connect your first email account
2. The bridge supports Gmail, Outlook, Yahoo, FastMail, and custom IMAP servers
3. Once connected, new emails will automatically create Matrix rooms

Need help? Use ` + "`!matrimail help`" + ` for more information.
`)
		return
	}

	// Get real account list from database (without passwords for performance)
	ctx := context.Background()
	accounts, err := connector.DB.GetUserAccountsBasic(ctx, ce.User.MXID.String())
	if err != nil {
		ce.Reply("❌ Failed to get account list: %s", err.Error())
		return
	}

	if len(accounts) == 0 {
		ce.Reply("📭 No email accounts found in database. Use `!matrimail login` to add one.")
		return
	}

	// Get account status from IMAP manager
	statusMap := make(map[string]imap.AccountStatus)
	statuses := connector.IMAPManager.GetAccountStatus(ce.User.MXID.String())
	for _, status := range statuses {
		statusMap[status.Email] = status
	}

	// Build response
	response := fmt.Sprintf("📧 **Connected Email Accounts:** %d\n\n", len(accounts))

	for _, account := range accounts {
		status, hasStatus := statusMap[account.Email]

		var statusIcon string
		var statusText string
		var provider string

		// Determine provider name
		domain := strings.ToLower(strings.Split(account.Email, "@")[1])
		if p, ok := imap.CommonProviders[domain]; ok {
			provider = p.Name
		} else {
			provider = "Custom IMAP"
		}

		if hasStatus {
			if status.Connected && status.IDLEActive {
				statusIcon = "✅"
				statusText = "Connected, monitoring"
			} else if status.Connected {
				statusIcon = "🔄"
				statusText = "Connected, starting monitoring"
			} else {
				statusIcon = "❌"
				statusText = "Disconnected"
			}
		} else {
			statusIcon = "⚠️"
			statusText = "Status unknown"
		}

		response += fmt.Sprintf("• %s **%s** (%s) - %s\n", statusIcon, account.Email, provider, statusText)
		if hasStatus && status.Host != "" {
			response += fmt.Sprintf("    Server: %s:%d\n", status.Host, status.Port)
		}
		// Show monitored folders
		if len(account.MonitoredFolders) > 0 {
			response += fmt.Sprintf("    📁 Folders: %s\n", strings.Join(account.MonitoredFolders, ", "))
		}
		response += fmt.Sprintf("    Added: %s\n\n", account.CreatedAt.Format("Jan 2, 2006"))
	}

	response += "💡 Use `!matrimail logout <email>` to remove a specific account."
	ce.Reply(response)
}

func fnSync(ce *commands.Event, connector *EmailConnector) {
	_ = connector // parameter kept for API consistency
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` to get started.")
		return
	}

	// If specific account provided, sync only that one
	if len(ce.Args) > 0 {
		emailAddr := ce.Args[0]
		var targetLogin *bridgev2.UserLogin
		for _, login := range logins {
			if client, ok := login.Client.(*EmailClient); ok && client.Email == emailAddr {
				targetLogin = login
				break
			}
		}

		if targetLogin == nil {
			ce.Reply("❌ Email account **%s** not found in your connected accounts.", emailAddr)
			return
		}

		ce.Reply("🔄 Forcing sync for **%s**...", emailAddr)
		if client, ok := targetLogin.Client.(*EmailClient); ok {
			if client.IMAPClient != nil && client.IMAPClient.IsConnected() {
				if err := client.IMAPClient.CheckNewMessages(); err != nil {
					ce.Reply("❌ Sync failed for **%s**: %s", emailAddr, err.Error())
					return
				}
				ce.Reply("✅ Sync completed for **%s**", emailAddr)
			} else {
				ce.Reply("⚠️ **%s** is not connected to IMAP server", emailAddr)
			}
		}
		return
	}

	// Sync all accounts
	ce.Reply("🔄 Forcing sync for all %d email account(s)...", len(logins))
	var successes []string
	var failures []string

	// Create context with timeout for all sync operations, derived from command context
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, login := range logins {
		if client, ok := login.Client.(*EmailClient); ok {
			// Capture IMAP client reference safely to prevent nil pointer panic
			imapClient := client.IMAPClient
			if imapClient != nil && imapClient.IsConnected() {
				// Create individual context for each sync operation
				syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
				done := make(chan error, 1)

				go func() {
					defer syncCancel() // Ensure context is cancelled when goroutine exits
					select {
					case done <- imapClient.CheckNewMessages():
					case <-syncCtx.Done():
						// Context cancelled, exit goroutine cleanly
						return
					}
				}()

				select {
				case err := <-done:
					syncCancel() // Cancel context as operation completed
					if err != nil {
						failures = append(failures, fmt.Sprintf("%s: %s", client.Email, err.Error()))
					} else {
						successes = append(successes, client.Email)
					}
				case <-syncCtx.Done():
					// Timeout occurred, context will cancel the goroutine
					failures = append(failures, fmt.Sprintf("%s: sync timed out after 30 seconds", client.Email))
				}
			} else {
				failures = append(failures, fmt.Sprintf("%s: not connected", client.Email))
			}
		}
	}

	var result strings.Builder
	if len(successes) > 0 {
		result.WriteString(fmt.Sprintf("✅ Successfully synced: %s\n", strings.Join(successes, ", ")))
	}
	if len(failures) > 0 {
		result.WriteString(fmt.Sprintf("❌ Failed to sync: %s", strings.Join(failures, "; ")))
	}

	ce.Reply(result.String())
}

func fnReconnect(ce *commands.Event, connector *EmailConnector) {
	_ = connector // parameter kept for API consistency
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` to get started.")
		return
	}

	// If specific account provided, reconnect only that one
	if len(ce.Args) > 0 {
		emailAddr := ce.Args[0]
		var targetLogin *bridgev2.UserLogin
		for _, login := range logins {
			if client, ok := login.Client.(*EmailClient); ok && client.Email == emailAddr {
				targetLogin = login
				break
			}
		}

		if targetLogin == nil {
			ce.Reply("❌ Email account **%s** not found in your connected accounts.", emailAddr)
			return
		}

		ce.Reply("🔌 Reconnecting **%s**...", emailAddr)
		if client, ok := targetLogin.Client.(*EmailClient); ok {
			// Capture IMAP client reference safely to prevent nil pointer panic
			imapClient := client.IMAPClient
			if imapClient != nil {
				if err := imapClient.Reconnect(); err != nil {
					ce.Reply("❌ Reconnection failed for **%s**: %s", emailAddr, err.Error())
					return
				}
				// Start IDLE after successful reconnect
				if err := imapClient.StartIDLE(); err != nil {
					ce.Reply("⚠️ Reconnected **%s** but IDLE failed to start: %s", emailAddr, err.Error())
				} else {
					ce.Reply("✅ Successfully reconnected **%s**", emailAddr)
				}
			} else {
				ce.Reply("❌ No IMAP client found for **%s**", emailAddr)
			}
		}
		return
	}

	// Reconnect all accounts
	ce.Reply("🔌 Reconnecting all %d email account(s)...", len(logins))
	var successes []string
	var failures []string

	for _, login := range logins {
		if client, ok := login.Client.(*EmailClient); ok {
			// Capture IMAP client reference safely to prevent nil pointer panic
			imapClient := client.IMAPClient
			if imapClient != nil {
				if err := imapClient.Reconnect(); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %s", client.Email, err.Error()))
					continue
				}
				if err := imapClient.StartIDLE(); err != nil {
					failures = append(failures, fmt.Sprintf("%s: IDLE failed - %s", client.Email, err.Error()))
				} else {
					successes = append(successes, client.Email)
				}
			}
		}
	}

	var result strings.Builder
	if len(successes) > 0 {
		result.WriteString(fmt.Sprintf("✅ Successfully reconnected: %s\n", strings.Join(successes, ", ")))
	}
	if len(failures) > 0 {
		result.WriteString(fmt.Sprintf("❌ Failed to reconnect: %s", strings.Join(failures, "; ")))
	}

	ce.Reply(result.String())
}

// parseLoginArgs parses command arguments in the format: email:user@domain.com password:pass or password:"quoted pass"
func parseLoginArgs(args string) (email, password string, err error) {
	// Split by spaces but preserve quoted strings
	parts := parseQuotedArgs(args)

	for _, part := range parts {
		if strings.HasPrefix(part, "email:") {
			email = strings.TrimPrefix(part, "email:")
		} else if strings.HasPrefix(part, "password:") {
			password = strings.TrimPrefix(part, "password:")
		}
	}

	if email == "" {
		return "", "", fmt.Errorf("email is required")
	}
	if password == "" {
		return "", "", fmt.Errorf("password is required")
	}

	// Validate email format
	if !strings.Contains(email, "@") || !strings.Contains(email, ".") {
		return "", "", fmt.Errorf("invalid email format")
	}

	return email, password, nil
}

// parseQuotedArgs splits arguments while preserving quoted strings
func parseQuotedArgs(args string) []string {
	var result []string
	var current strings.Builder
	inQuotes := false
	escaped := false

	for i, r := range args {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}

		// Handle end of string
		if i == len(args)-1 && current.Len() > 0 {
			result = append(result, current.String())
		}
	}

	return result
}

// processTextLogin processes a text-based login using the same flow as the interactive login
func processTextLogin(ctx context.Context, ce *commands.Event, email, password string, connector *EmailConnector) error {
	// Create a login process
	loginProcess, err := connector.CreateLogin(ctx, ce.User, "email-password")
	if err != nil {
		return fmt.Errorf("failed to create login process: %w", err)
	}

	// Cast to our EmailLoginProcess to access internal methods
	emailLogin, ok := loginProcess.(*EmailLoginProcess)
	if !ok {
		return fmt.Errorf("unexpected login process type")
	}

	// Validate inputs before setting (additional validation beyond parseLoginArgs)
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)

	if len(email) > 256 {
		return fmt.Errorf("email address too long (max 256 characters)")
	}
	if len(password) > 1024 {
		return fmt.Errorf("password too long (max 1024 characters)")
	}
	if len(password) == 0 {
		return fmt.Errorf("password cannot be empty")
	}

	// Set the credentials directly
	emailLogin.email = email
	emailLogin.username = email
	emailLogin.password = password

	// Submit the credentials as if they came from form input
	inputData := map[string]string{
		"email":    email,
		"password": password,
	}

	step, err := emailLogin.SubmitUserInput(ctx, inputData)
	if err != nil {
		return err
	}

	// Send success message
	ce.Reply(step.Instructions)
	return nil
}

// buildEnhancedLoginInstructions enhances the form-based instructions with text command info
func buildEnhancedLoginInstructions(originalInstructions string) string {
	// Include original form-based instructions at the top if provided
	prefix := ""
	if strings.TrimSpace(originalInstructions) != "" {
		prefix = strings.TrimSpace(originalInstructions) + "\n\n"
	}
	return prefix + `🔐 **Email Bridge Login**

**Method 1: Quick Command**
` + "`!matrimail login email:your@email.com password:yourpassword`" + `
` + "`!matrimail login email:your@email.com password:\"password with spaces\"`" + `

**Method 2: Form Fields (if supported by your client)**
📧 **Please enter your email credentials using the form fields below.**

**Important Notes:**
• For Gmail/Yahoo/Outlook: Use an **App Password** (not your regular password)
• The bridge will automatically detect your email provider settings
• Your password will be encrypted and stored securely

**App Password Setup Guide:**
**Gmail:** Settings → Security → 2-Step Verification → App passwords
**Yahoo:** Account Info → Account security → Generate app password  
**Outlook:** Security → Sign-in options → App passwords
**iCloud:** Sign-In and Security → App-Specific Passwords

**Popular Providers Supported:**
✅ Gmail, Yahoo, Outlook, iCloud, FastMail - Auto-configured
✅ Custom IMAP servers - Auto-detected

*The bridge will test your IMAP connection automatically after you submit your credentials.*

**Need help?** Use ` + "`!matrimail help`" + ` for more information or ` + "`!matrimail status`" + ` to check connection status.`
}

func fnPassphrase(ce *commands.Event, connector *EmailConnector) {
	_ = connector // parameter kept for API consistency
	if len(ce.Args) == 0 {
		// Show current status and usage
		passphrasePath, err := getPassphraseFilePath()
		if err != nil {
			ce.Reply("❌ Failed to get passphrase file path: %s", err.Error())
			return
		}

		// Check if passphrase file exists
		exists := false
		if _, err := os.Stat(passphrasePath); err == nil {
			exists = true
		}

		// Check if environment variable is set
		envSet := strings.TrimSpace(os.Getenv("MATRIMAIL_PASSPHRASE")) != ""

		ce.Reply(`🔐 **Encryption Passphrase Status**

**Environment Variable:** %s
**Passphrase File:** %s
**File Location:** %s

**Usage:**
• `+"`!matrimail passphrase show-location`"+` - Show passphrase file path

**Security Note:** Your email passwords are encrypted using this passphrase. Matrimail automatically generates one if neither environment variable nor file exists.`,
			map[bool]string{true: "✅ Set", false: "❌ Not set"}[envSet],
			map[bool]string{true: "✅ Exists", false: "❌ Not found"}[exists],
			passphrasePath)
		return
	}

	command := strings.ToLower(ce.Args[0])

	switch command {
	case "show-location":
		passphrasePath, err := getPassphraseFilePath()
		if err != nil {
			ce.Reply("❌ Failed to get passphrase file path: %s", err.Error())
			return
		}

		exists := false
		if _, err := os.Stat(passphrasePath); err == nil {
			exists = true
		}

		ce.Reply(`📍 **Passphrase File Location**

**Path:** %s
**Status:** %s

The passphrase is read from the MATRIMAIL_PASSPHRASE environment variable if
set, otherwise from the file above. Prefer the environment variable: it keeps
the key out of the data directory that holds the database and the salt.`,
			passphrasePath,
			map[bool]string{true: "✅ File exists", false: "❌ File not found"}[exists])

	case "generate", "set":
		// Removed deliberately. Both subcommands overwrote the passphrase file
		// without re-encrypting the rows it protects, so the next restart could
		// no longer decrypt any stored credential -- while the reply claimed
		// "existing email accounts will continue to work". `generate` also
		// printed the passphrase, the sole input to key derivation, into the
		// Matrix room, where it lands in the homeserver's event store and every
		// device cache. Changing the passphrase without re-encrypting is
		// equivalent to deleting every stored credential.
		ce.Reply("❌ `%s` has been removed: it overwrote the encryption passphrase "+
			"without re-encrypting stored credentials, which silently destroyed them "+
			"at the next restart. To use a passphrase you choose, set "+
			"MATRIMAIL_PASSPHRASE in the service environment before adding accounts. "+
			"Changing it afterwards has the same effect these commands had: stored "+
			"credentials can no longer be decrypted, and each account has to be "+
			"logged in again.", command)

	default:
		ce.Reply("❌ Unknown command: %s\n\n**Available commands:**\n• `show-location` - Show passphrase file location", command)
	}
}

// fnCompose implements the `!matrimail compose` bot command. It picks a login
// (the user's first / default email account, or a `from:` override if we add
// one later), parses the to:/cc:/subject: arguments, and delegates to
// EmailClient.ResolveIdentifier with createChat=true. The resulting portal is
// materialized via Bridge.GetPortalByKey + Portal.CreateMatrixRoom — same path
// the framework's built-in `start-chat` command uses.
//
// The synthetic thread is updated in-place to attach the parsed Cc list and
// Subject so HandleMatrixMessage's draft branch can use them when the user
// types the first message.
func fnCompose(ce *commands.Event, connector *EmailConnector) {
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` first.")
		return
	}

	args, err := parseComposeArgs(ce.RawArgs)
	if err != nil {
		ce.Reply("❌ %s\n\n**Usage:** `!matrimail compose to:alice@example.com [cc:bob@example.com] [subject:\"hello\"]`", err.Error())
		return
	}

	// Pick the first login's EmailClient. Multi-account users can disambiguate
	// in v2 with a `from:` argument; for now first-match is good enough and
	// matches how !matrimail logout / sync default.
	var client *EmailClient
	for _, login := range logins {
		if c, ok := login.Client.(*EmailClient); ok {
			client = c
			break
		}
	}
	if client == nil {
		ce.Reply("❌ Internal error: no usable EmailClient found among your logins.")
		return
	}

	resp, err := client.ResolveIdentifier(ce.Ctx, args.To, true)
	if err != nil {
		ce.Reply("❌ Failed to start compose thread: %s", err.Error())
		return
	}
	if resp == nil || resp.Chat == nil {
		ce.Reply("❌ Internal error: ResolveIdentifier returned no chat.")
		return
	}

	// Attach the parsed Cc + Subject to the synthetic thread so the first
	// HandleMatrixMessage call uses them. The thread was just inserted into
	// the cache by ResolveIdentifier; GetThreadByID returns the same pointer.
	threadID := strings.TrimPrefix(string(resp.Chat.PortalKey.ID), "thread:")
	if connector.ThreadManager != nil {
		if thread := connector.ThreadManager.GetThreadByID(string(client.UserLogin.ID), threadID); thread != nil {
			thread.Subject = args.Subject
			thread.Cc = append([]string(nil), args.Cc...)
			// Fold cc into Participants so resolveRecipients picks them up
			// alongside the to: address.
			for _, c := range args.Cc {
				thread.Participants = append(thread.Participants, strings.ToLower(c))
			}
			connector.ThreadManager.CacheForReceiver(string(client.UserLogin.ID), thread)
		}
	}

	portal, err := ce.Bridge.GetPortalByKey(ce.Ctx, resp.Chat.PortalKey)
	if err != nil {
		ce.Reply("❌ Failed to get portal: %s", err.Error())
		return
	}

	// Persist the draft to Portal.Metadata immediately so a bridge restart
	// before the first send doesn't lose the thread state.
	portal.Metadata = &PortalMetadata{
		ThreadID:     threadID,
		Subject:      args.Subject,
		Participants: append([]string{strings.ToLower(args.To)}, args.Cc...),
		IsDraft:      true,
	}

	chatInfo, err := client.GetChatInfo(ce.Ctx, portal)
	if err != nil {
		ce.Reply("❌ Failed to compute chat info: %s", err.Error())
		return
	}
	if portal.MXID == "" {
		if err := portal.CreateMatrixRoom(ce.Ctx, client.UserLogin, chatInfo); err != nil {
			ce.Reply("❌ Failed to create Matrix room: %s", err.Error())
			return
		}
	}
	// CreateMatrixRoom calls Save internally; if we skipped it (room already
	// existed for some reason), persist the metadata write explicitly.
	if portal.MXID != "" {
		if err := portal.Save(ce.Ctx); err != nil {
			connector.Bridge.Log.Warn().Err(err).Msg("compose: portal.Save (post-create) failed")
		}
	}

	subjLine := args.Subject
	if subjLine == "" {
		subjLine = "(no subject)"
	}
	ccLine := ""
	if len(args.Cc) > 0 {
		ccLine = fmt.Sprintf("\n**Cc:** %s", strings.Join(args.Cc, ", "))
	}
	ce.Reply("✉️ **New email thread ready**\n\n**To:** %s%s\n**Subject:** %s\n\nOpen the new room and type your message — it will be sent as the first email in the thread.\n\nRoom: [%s](%s)", args.To, ccLine, subjLine, portal.MXID, portal.MXID.URI().MatrixToURL())
}

// fnConfig handles bridge configuration commands
func fnConfig(ce *commands.Event, connector *EmailConnector) {
	if len(ce.Args) == 0 {
		ce.Reply(`⚙️ **Bridge Configuration**

**Available subcommands:**
• ` + "`!matrimail config folders`" + ` - Change which folders to monitor

**Current status:**
Use ` + "`!matrimail list`" + ` to see your connected accounts and their settings.`)
		return
	}

	subcommand := strings.ToLower(ce.Args[0])

	switch subcommand {
	case "folders":
		fnConfigFolders(ce, connector)
	default:
		ce.Reply("❌ Unknown config subcommand: `%s`\n\n**Available subcommands:**\n• `folders` - Change which folders to monitor", subcommand)
	}
}

// fnConfigFolders handles reconfiguring monitored folders for an account
// Requires re-authentication as per implementation plan
func fnConfigFolders(ce *commands.Event, connector *EmailConnector) {
	_ = connector // parameter kept for API consistency
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` to get started.")
		return
	}

	// If multiple accounts, need to specify which one
	if len(logins) > 1 && len(ce.Args) < 2 {
		var accountList strings.Builder
		accountList.WriteString("📧 You have multiple email accounts. Please specify which account to reconfigure:\n\n")
		for _, login := range logins {
			if client, ok := login.Client.(*EmailClient); ok {
				accountList.WriteString(fmt.Sprintf("• `!matrimail config folders %s`\n", client.Email))
			}
		}
		ce.Reply(accountList.String())
		return
	}

	// Find the target account
	var targetEmail string
	if len(ce.Args) >= 2 {
		targetEmail = ce.Args[1]
	} else {
		// Single account - use it
		if client, ok := logins[0].Client.(*EmailClient); ok {
			targetEmail = client.Email
		}
	}

	// Verify the account exists
	var targetLogin *bridgev2.UserLogin
	for _, login := range logins {
		if client, ok := login.Client.(*EmailClient); ok && client.Email == targetEmail {
			targetLogin = login
			break
		}
	}

	if targetLogin == nil {
		ce.Reply("❌ Email account **%s** not found in your connected accounts.", targetEmail)
		return
	}

	ce.Reply(`🔐 **Folder Reconfiguration**

To change the monitored folders for **%s**, you'll need to re-authenticate.

**Why?** Re-authentication ensures your credentials are still valid and allows us to fetch the current folder list.

**To proceed:**
1. Use `+"`!matrimail logout %s`"+` to disconnect
2. Use `+"`!matrimail login`"+` to reconnect and choose new folders

💡 **Tip:** Your existing Matrix rooms will remain - only the folders being monitored will change.`, targetEmail, targetEmail)
}

// fnOAuth handles `!matrimail oauth <subcommand>`. Three subcommands:
//
//   - paste-code <redirect-url>: completes an in-progress OAuth login when
//     the loopback callback can't reach the bridge (truly headless host, no
//     SSH tunneling). The user authorizes in their browser, the redirect to
//     127.0.0.1:NNNN/callback fails, and they paste the URL from their
//     address bar here. The bridge already has the matching state and PKCE
//     verifier in memory and finishes the exchange normally.
//
//   - paste-token <email> <refresh_token>: bootstraps an account with a
//     user-supplied refresh token. The other "headless escape hatch" — for
//     users who can run the full OAuth flow on a separate workstation. Token
//     is validated against Google before being stored.
//
//   - revoke <email>: revokes the refresh token at Google's end (severing
//     matrimail's access from the user's Google account dashboard) and
//     clears the local OAuth state. Equivalent to logout + manual revoke
//     in one step.
func fnOAuth(ce *commands.Event, connector *EmailConnector) {
	if len(ce.Args) == 0 {
		ce.Reply(`**!matrimail oauth — subcommands**

- ` + "`!matrimail oauth paste-code <redirect-url>`" + ` — finish a headless
  OAuth login by pasting the URL your browser was redirected to (the page
  that failed to load with "can't connect to 127.0.0.1:NNNN").
- ` + "`!matrimail oauth paste-token <email> <refresh_token>`" + ` — register
  an account using a refresh token you obtained out-of-band on another
  machine. Token is validated against Google before being stored.
  **Last resort:** the token you type is long-lived and grants your whole
  mailbox, and typing it here writes it into this room's history. Prefer an
  SSH port-forward and a normal login.
- ` + "`!matrimail oauth revoke <email>`" + ` — revoke matrimail's access at
  Google and clear the local OAuth state for the account.`)
		return
	}

	sub := strings.ToLower(ce.Args[0])
	switch sub {
	case "paste-code", "code":
		fnOAuthPasteCode(ce, connector)
	case "paste-token", "paste":
		fnOAuthPasteToken(ce, connector)
	case "revoke":
		fnOAuthRevoke(ce, connector)
	default:
		ce.Reply("❌ Unknown subcommand: `%s`. Try `!matrimail oauth` for help.", sub)
	}
}

// fnOAuthPasteCode implements `!matrimail oauth paste-code <redirect-url>`.
// Parses the OAuth callback URL the user copied from their browser's address
// bar, looks up the user's in-progress OAuth login, and injects the code
// into the listener so finishOAuthExchange runs as if the loopback callback
// had fired normally.
func fnOAuthPasteCode(ce *commands.Event, connector *EmailConnector) {
	if len(ce.Args) < 2 {
		ce.Reply(`Usage: ` + "`!matrimail oauth paste-code <redirect-url>`" + `

After authorizing in your browser, the page will fail to load (because the
loopback redirect points at the bridge host, not your laptop). Copy the
**full URL** from your browser's address bar — it looks like:

    http://127.0.0.1:NNNNN/callback?code=4/0AY...&state=...

…and pass it to this command. The bridge already has the matching state and
PKCE verifier in memory and will exchange the code for a refresh token.

This is for truly headless deployments where you can't SSH-tunnel the
loopback port either. If you can SSH-tunnel, just open the URL on your
workstation and the normal flow completes itself.`)
		return
	}

	// Join everything after the subcommand — the URL itself is one arg, but
	// being defensive in case a client did weird whitespace-splitting.
	raw := strings.TrimSpace(strings.Join(ce.Args[1:], ""))
	code, state, err := parseOAuthCallbackURL(raw)
	if err != nil {
		ce.Reply("❌ Could not parse that URL: %v\n\nMake sure you copied the full `http://127.0.0.1:NNNN/callback?code=...&state=...` URL from your browser, including the `?` and everything after.", err)
		return
	}

	listener := connector.lookupOAuthListener(ce.User.MXID.String())
	if listener == nil {
		ce.Reply("❌ No in-progress OAuth login found for your account.\n\nRun `!matrimail login` first, complete Step 2 by opening the URL Google gives you, then come back and paste the redirect URL here.")
		return
	}
	if err := listener.Inject(code, state); err != nil {
		ce.Reply("❌ Code injection failed: %v\n\nIf this says \"state mismatch\", the URL is from an older login session — run `!matrimail login` again to start a fresh one and use the new URL it gives you.", err)
		return
	}
	ce.Reply("✅ **Authorization code accepted.** Watch the login DM — the bridge will continue with profile verification and folder selection.")
}

// parseOAuthCallbackURL extracts the `code` and `state` query parameters from
// a Google OAuth redirect URL. Accepts a full URL (`http://127.0.0.1:.../callback?...`)
// or a bare query string (`?code=...&state=...` or `code=...&state=...`) so
// users can paste in whatever format their browser gave them. Surfaces a
// clear error if Google returned `?error=...` instead of a code.
func parseOAuthCallbackURL(raw string) (code, state string, err error) {
	q, err := extractCallbackQuery(raw)
	if err != nil {
		return "", "", err
	}
	if errCode := q.Get("error"); errCode != "" {
		desc := q.Get("error_description")
		if desc == "" {
			return "", "", fmt.Errorf("Google reported error %q", errCode)
		}
		return "", "", fmt.Errorf("Google reported error %q: %s", errCode, desc)
	}
	code = q.Get("code")
	state = q.Get("state")
	if code == "" {
		return "", "", errors.New("missing 'code' parameter in URL")
	}
	if state == "" {
		return "", "", errors.New("missing 'state' parameter in URL")
	}
	return code, state, nil
}

// extractCallbackQuery turns either a full URL or a query-string fragment
// into url.Values. Tolerant of leading `?` and of URLs with no scheme.
func extractCallbackQuery(raw string) (url.Values, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty input")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		return u.Query(), nil
	}
	if strings.HasPrefix(raw, "?") {
		raw = raw[1:]
	}
	// If the user pasted "/callback?code=...&state=..." (path + query)
	// without scheme, strip the path prefix.
	if i := strings.Index(raw, "?"); i >= 0 {
		raw = raw[i+1:]
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	return q, nil
}

// fnOAuthPasteToken implements `!matrimail oauth paste-token <email> <refresh_token>`.
// Validates the refresh token by exchanging it against Google's /token
// endpoint, fetches the authorized account's profile to confirm identity,
// then persists the token under an account row.
func fnOAuthPasteToken(ce *commands.Event, connector *EmailConnector) {
	if len(ce.Args) < 3 {
		ce.Reply(`Usage: ` + "`!matrimail oauth paste-token <email> <refresh_token>`" + `

⚠️ **This writes a long-lived full-mailbox credential into this room.** A refresh
token can mint access tokens to your whole mailbox until it expires or is
revoked. Sending it here stores it in the homeserver's event store and
on every device that syncs this room; deleting the message afterwards does not
reliably remove those copies. Use an SSH port-forward and a normal login
instead wherever that is possible.

Get a refresh token by running the OAuth authorization flow on a machine that
has a browser and can reach a loopback port. matrimail uses the standard
Google "Desktop app" client; any tool that does authorization-code + PKCE
against your gmail_oauth.client_id will produce a compatible refresh token
(e.g. ` + "`oauth2l`" + `, ` + "`mutt_oauth2.py`" + `, etc.).

The token will be validated against Google before being stored. If you have
already sent one, revoke it with ` + "`!matrimail oauth revoke <email>`" + ` and
authorize again.`)
		return
	}

	emailAddr := strings.TrimSpace(ce.Args[1])
	refreshToken := strings.TrimSpace(ce.Args[2])
	if emailAddr == "" || refreshToken == "" {
		ce.Reply("❌ Email and refresh token are both required.")
		return
	}
	if connector.Config.GmailOAuth.ClientID == "" || connector.Config.GmailOAuth.ClientSecret == "" {
		ce.Reply("❌ `gmail_oauth.client_id` / `client_secret` are not configured on this bridge. Ask your admin to set them in `config.yaml`.")
		return
	}

	ctx := context.Background()
	cfg := email.GmailOAuthConfig{
		ClientID:     connector.Config.GmailOAuth.ClientID,
		ClientSecret: connector.Config.GmailOAuth.ClientSecret,
	}

	// Validate by exchanging for an access token.
	tok, err := email.ExchangeRefreshToken(ctx, cfg, refreshToken)
	if err != nil {
		ce.Reply("❌ Refresh token validation failed: %v\n\nDouble-check that the token was issued by the same `client_id` configured on this bridge, and that the user hasn't revoked matrimail's access.", err)
		return
	}

	// Confirm identity matches the email argument.
	ts := email.TokenSource(ctx, cfg, tok)
	svc, err := gmail.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		ce.Reply("❌ Could not construct Gmail client: %v", err)
		return
	}
	prof, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		ce.Reply("❌ Gmail profile lookup failed: %v", err)
		return
	}
	if !strings.EqualFold(prof.EmailAddress, emailAddr) {
		ce.Reply("❌ Authorized account is **%s**, not the email you specified (**%s**). Aborting.", prof.EmailAddress, emailAddr)
		return
	}

	// Persist. Pre-create the account row if missing, then save the token
	// with explicit scope_mode (default modify; user can override later by
	// re-logging in interactively if they need full mode).
	account := &EmailAccount{
		UserMXID:         ce.User.MXID.String(),
		Email:            emailAddr,
		Username:         emailAddr,
		Password:         "", // unused for OAuth
		Host:             "imap.gmail.com",
		Port:             993,
		TLS:              true,
		CreatedAt:        time.Now(),
		LastSyncTime:     time.Now(),
		MonitoredFolders: []string{"INBOX"},
	}
	if err := connector.DB.UpsertAccount(ctx, account); err != nil {
		ce.Reply("❌ Failed to create account row: %v", err)
		return
	}
	scopeMode := connector.Config.GmailOAuth.EffectiveDefaultScopeMode()
	if err := connector.DB.SaveOAuthTokenWithScope(ctx, ce.User.MXID.String(), emailAddr,
		OAuthProviderGoogle, scopeMode, tok); err != nil {
		ce.Reply("❌ Failed to persist OAuth token: %v", err)
		return
	}

	ce.Reply(`✅ **Account registered:** %s (scope_mode=%s)

Run `+"`!matrimail status`"+` to confirm the bridge picked up the new account.`, emailAddr, scopeMode)
}

// fnOAuthRevoke implements `!matrimail oauth revoke <email>`. Revokes the
// stored refresh token at Google, then flips the account to needs-reauth so
// it stops trying to refresh. The user can then run `!matrimail logout` to
// fully delete it, or `!matrimail login` to re-authorize.
func fnOAuthRevoke(ce *commands.Event, connector *EmailConnector) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!matrimail oauth revoke <email>`")
		return
	}
	emailAddr := strings.TrimSpace(ce.Args[1])
	if emailAddr == "" {
		ce.Reply("❌ Email is required.")
		return
	}

	ctx := context.Background()
	info, err := connector.DB.GetOAuthAccount(ctx, ce.User.MXID.String(), emailAddr)
	if err != nil {
		ce.Reply("❌ Failed to load account: %v", err)
		return
	}
	if info == nil || info.Token == nil {
		ce.Reply("❌ No OAuth account found for **%s**.", emailAddr)
		return
	}

	// Best-effort revoke; don't fail the command if Google's endpoint is
	// flaky (the local state flip is the most important thing).
	if err := email.RevokeToken(ctx, info.Token.RefreshToken); err != nil {
		connector.Bridge.Log.Warn().Err(err).Str("email", emailAddr).Msg("Token revocation at Google failed")
	}
	if err := connector.MarkAccountNeedsReauth(ctx, ce.User.MXID.String(), emailAddr); err != nil {
		ce.Reply("⚠️ Token revoked at Google but local flag failed: %v", err)
		return
	}

	// Stop the inbound poller for modify-mode accounts so it doesn't keep
	// hitting Google with a dead token before the next bridge restart.
	if connector.GmailInbound != nil {
		connector.GmailInbound.Stop(ce.User.MXID.String(), emailAddr)
	}

	ce.Reply(`🔐 **OAuth access revoked for %s.**

The refresh token has been invalidated at Google and the account is paused
locally. To use this account again, run `+"`!matrimail login`"+`. To delete it
entirely, run `+"`!matrimail logout %s`"+`.`, emailAddr, emailAddr)
}

// fnReplyOnly toggles the next outbound in this portal to DM mode (reply
// only to thread.LastFrom). One-shot: consumed on the next send, then resets.
func fnReplyOnly(ce *commands.Event, connector *EmailConnector) {
	if ce.Portal == nil {
		ce.Reply("❌ `!matrimail reply-only` must be run inside an email portal room.")
		return
	}
	connector.MarkPortalNextReplyDM(ce.Portal.ID)
	target := ""
	if connector.ThreadManager != nil {
		threadID := strings.TrimPrefix(string(ce.Portal.ID), "thread:")
		// Best-effort: we don't have the receiver here; iterate the user's
		// logins to find the matching thread.
		for _, login := range ce.User.GetUserLogins() {
			if t := connector.ThreadManager.GetThreadByID(string(login.ID), threadID); t != nil {
				target = t.LastFrom
				break
			}
		}
	}
	if target == "" {
		ce.Reply("✅ Next reply in this room will go to the original sender only (DM mode).")
	} else {
		ce.Reply(fmt.Sprintf("✅ Next reply in this room will go to **%s** only (DM mode).", target))
	}
}

// fnDraft fires the configured draft webhook (typically an n8n flow) for the
// thread that owns the room the command was run in. Any extra args after
// `draft` are joined and forwarded as a free-form Instruction the workflow
// can interpret (e.g. "ask if Friday works", "decline politely").
//
// Requires: command must be run from a portal room (not the management room),
// and draft_webhook.url must be configured.
func fnDraft(ce *commands.Event, connector *EmailConnector) {
	if ce.Portal == nil {
		ce.Reply("❌ `!matrimail draft` must be run inside an email portal room (the thread you want a draft for), not the management room.")
		return
	}
	if strings.TrimSpace(connector.Config.DraftWebhook.URL) == "" {
		ce.Reply("❌ Draft webhook is not configured. Ask the bridge admin to set `draft_webhook.url` in `config.yaml`.")
		return
	}

	// Find which account this portal belongs to. The portal's Receiver is the
	// UserLogin ID — same shape as login.ID — so we match against the user's
	// existing logins to recover the EmailClient.
	var client *EmailClient
	for _, login := range ce.User.GetUserLogins() {
		if login.ID == ce.Portal.Receiver {
			if c, ok := login.Client.(*EmailClient); ok {
				client = c
				break
			}
		}
	}
	if client == nil {
		ce.Reply("❌ Couldn't resolve which email account owns this room (portal receiver: `%s`). Are you logged in to that account?", ce.Portal.Receiver)
		return
	}

	req := DraftRequest{
		Account:  client.Email,
		UserMXID: ce.User.MXID.String(),
		Source:   "command",
		RoomID:   string(ce.Portal.MXID),
	}

	// Layer 1: persisted Portal.Metadata. Only populated for compose threads
	// (`!matrimail compose`) and for portals that have produced at least one
	// outbound send (`client_send.go` writes it post-send). Pure inbound-only
	// rooms have nil metadata.
	if meta, ok := ce.Portal.Metadata.(*PortalMetadata); ok && meta != nil {
		req.ThreadID = meta.ThreadID
		req.MessageID = meta.LastMessageID
		req.Subject = meta.Subject
		req.Participants = meta.Participants
	}

	// Layer 2: in-memory ThreadManager. Has fresh data for any thread that
	// received a message in the last 24h. Use it to backfill anything the
	// portal metadata didn't have (or wasn't there at all).
	if connector.ThreadManager != nil {
		threadID := strings.TrimPrefix(string(ce.Portal.ID), "thread:")
		if threadID != "" {
			if thread := connector.ThreadManager.GetThreadByID(string(ce.Portal.Receiver), threadID); thread != nil {
				if req.ThreadID == "" {
					req.ThreadID = thread.ThreadID
				}
				if req.MessageID == "" {
					req.MessageID = thread.MessageID
				}
				if req.Subject == "" {
					req.Subject = thread.Subject
				}
				if len(req.Participants) == 0 && len(thread.Participants) > 0 {
					req.Participants = append([]string(nil), thread.Participants...)
				}
			}
		}
	}

	// Layer 3: at minimum, derive ThreadID from the portal ID so n8n always
	// has something to query Gmail with (the bridge encodes the RFC822 root
	// Message-Id as `thread:<id>` in MakePortalID).
	if req.ThreadID == "" {
		req.ThreadID = strings.TrimPrefix(string(ce.Portal.ID), "thread:")
	}
	// If we still don't have a per-message id, fall back to the thread id —
	// downstream rfc822msgid lookup will find at least the root message and
	// Gmail's thread API picks up the rest.
	if req.MessageID == "" {
		req.MessageID = req.ThreadID
	}

	if len(ce.Args) > 0 {
		req.Instruction = strings.TrimSpace(strings.Join(ce.Args, " "))
	}

	logger := connector.Bridge.Log.With().Str("component", "draft_webhook").Str("email", client.Email).Logger()
	ctx, cancel := context.WithTimeout(context.Background(), connector.Config.DraftWebhook.EffectiveTimeout()+5*time.Second)
	defer cancel()

	resp, err := triggerDraftWebhook(ctx, connector.Config.DraftWebhook, req, &logger)
	if err != nil {
		ce.Reply("❌ Draft request failed: %s", err.Error())
		return
	}

	// Status line — short ack that the workflow was queued.
	status := "✏️ Draft requested — agent is researching context; the draft will appear here when ready (~30-90s)."
	if resp != nil && strings.TrimSpace(resp.Message) != "" {
		status = fmt.Sprintf("✏️ %s", strings.TrimSpace(resp.Message))
	}
	if req.Instruction != "" {
		status += fmt.Sprintf("\n_Instruction:_ %s", req.Instruction)
	}
	ce.Reply(status)

	// Background poll: the n8n workflow ack'd fast (fire-and-forget) but the
	// actual draft creation happens after that — typically 30-90s for the AI
	// agent. Once it lands in Gmail, fetch the body and post it into THIS
	// portal so the user can review it in Element without leaving the room.
	go pollAndPostGmailDraft(connector, ce, client.Email, req.ThreadID, req.MessageID)
}

// pollAndPostGmailDraft watches Gmail for a draft on the given thread for
// up to ~3 minutes, then posts its body as a bot message in the portal
// where the !draft command was run. Best-effort — errors are logged but
// never surface to the user.
func pollAndPostGmailDraft(connector *EmailConnector, ce *commands.Event, emailAddr, threadHint, messageHint string) {
	logger := connector.Bridge.Log.With().
		Str("component", "draft_poll").
		Str("email", emailAddr).
		Str("room", string(ce.Portal.MXID)).
		Logger()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Get OAuth account info for this email.
	info, err := connector.DB.GetOAuthAccount(ctx, ce.User.MXID.String(), emailAddr)
	if err != nil || info == nil || info.Token == nil {
		logger.Warn().Err(err).Msg("Draft poll: no OAuth token; can't query Gmail")
		return
	}
	oauthCfg := email.GmailOAuthConfig{
		ClientID:     connector.Config.GmailOAuth.ClientID,
		ClientSecret: connector.Config.GmailOAuth.ClientSecret,
	}
	ts := email.TokenSource(context.Background(), oauthCfg, info.Token)
	svc, err := gmail.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		logger.Warn().Err(err).Msg("Draft poll: gmail.NewService failed")
		return
	}

	// Resolve the RFC822 message-id we sent to n8n into a Gmail internal
	// thread id. messages.list with q=rfc822msgid:<id> is the canonical way.
	resolveTry := func(msgID string) (string, error) {
		q := "rfc822msgid:" + strings.Trim(msgID, "<> \t")
		resp, lerr := svc.Users.Messages.List("me").Q(q).MaxResults(1).Context(ctx).Do()
		if lerr != nil {
			return "", lerr
		}
		if len(resp.Messages) == 0 {
			return "", nil
		}
		return resp.Messages[0].ThreadId, nil
	}
	gmailThreadID, _ := resolveTry(messageHint)
	if gmailThreadID == "" && threadHint != "" && threadHint != messageHint {
		gmailThreadID, _ = resolveTry(threadHint)
	}
	if gmailThreadID == "" {
		logger.Warn().Str("hint", messageHint).Msg("Draft poll: could not resolve Gmail thread id; aborting")
		return
	}
	logger.Debug().Str("gmail_thread_id", gmailThreadID).Msg("Draft poll: resolved thread id, watching for draft")

	// Poll every 15s, up to 12 times (~3 minutes total). The AI agent
	// typically completes in 30-90s; this gives a generous buffer.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	// Initial short delay before first poll — drafts take at least a few
	// seconds to land even on the fastest paths.
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}

	var lastDraftRev int64 // internalDate of the last draft we posted
	tries := 0
	for {
		tries++
		thread, terr := svc.Users.Threads.Get("me", gmailThreadID).Format("full").Context(ctx).Do()
		if terr != nil {
			logger.Warn().Err(terr).Int("try", tries).Msg("Draft poll: threads.get failed; retrying")
		} else {
			latest := pickLatestDraft(thread.Messages)
			if latest != nil && latest.InternalDate > lastDraftRev {
				body := extractGmailMessageText(latest)
				if strings.TrimSpace(body) != "" {
					postDraftToRoom(ctx, connector, ce.Portal.MXID, body, latest, &logger)
					lastDraftRev = latest.InternalDate
					return // one draft is enough; stop polling
				}
			}
		}
		if tries >= 12 {
			logger.Info().Msg("Draft poll: no draft seen after 3 minutes; giving up")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pickLatestDraft returns the message in the thread that carries the DRAFT
// label and has the highest internalDate; nil if no draft is present.
func pickLatestDraft(msgs []*gmail.Message) *gmail.Message {
	var latest *gmail.Message
	for _, m := range msgs {
		isDraft := false
		for _, l := range m.LabelIds {
			if strings.EqualFold(l, "DRAFT") {
				isDraft = true
				break
			}
		}
		if !isDraft {
			continue
		}
		if latest == nil || m.InternalDate > latest.InternalDate {
			latest = m
		}
	}
	return latest
}

// extractGmailMessageText pulls text/plain out of a Gmail message payload.
// Falls back to a tag-stripped text/html part, then to the snippet.
func extractGmailMessageText(m *gmail.Message) string {
	if m == nil {
		return ""
	}
	if m.Payload != nil {
		if txt := walkGmailPart(m.Payload, "text/plain"); txt != "" {
			return txt
		}
		if html := walkGmailPart(m.Payload, "text/html"); html != "" {
			// Strip tags (rough but adequate for review-in-Matrix).
			out := html
			for {
				start := strings.Index(out, "<")
				if start < 0 {
					break
				}
				end := strings.Index(out[start:], ">")
				if end < 0 {
					break
				}
				out = out[:start] + " " + out[start+end+1:]
			}
			return strings.TrimSpace(out)
		}
	}
	return strings.TrimSpace(m.Snippet)
}

func walkGmailPart(p *gmail.MessagePart, want string) string {
	if p == nil {
		return ""
	}
	if strings.EqualFold(p.MimeType, want) && p.Body != nil && p.Body.Data != "" {
		// Gmail uses URL-safe base64.
		s := strings.ReplaceAll(p.Body.Data, "-", "+")
		s = strings.ReplaceAll(s, "_", "/")
		// Pad
		if pad := len(s) % 4; pad != 0 {
			s += strings.Repeat("=", 4-pad)
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
		return string(raw)
	}
	for _, sub := range p.Parts {
		if t := walkGmailPart(sub, want); t != "" {
			return t
		}
	}
	return ""
}

// postDraftToRoom renders the draft body as a blockquoted bot message and
// sends it into the portal room.
func postDraftToRoom(ctx context.Context, connector *EmailConnector, roomID id.RoomID, body string, m *gmail.Message, logger *zerolog.Logger) {
	intent := connector.Bridge.Bot
	if intent == nil {
		logger.Warn().Msg("Draft poll: bridge bot intent unavailable")
		return
	}

	// Header — pull subject from headers.
	subject := ""
	if m != nil && m.Payload != nil {
		for _, h := range m.Payload.Headers {
			if strings.EqualFold(h.Name, "Subject") {
				subject = h.Value
				break
			}
		}
	}

	header := "**📝 Draft reply**"
	if subject != "" {
		header = fmt.Sprintf("**📝 Draft reply** — `%s`", subject)
	}

	body = strings.TrimSpace(body)
	quoted := "> " + strings.ReplaceAll(body, "\n", "\n> ")
	md := header + "\n\n" + quoted + "\n\n_The draft is also saved in Gmail; edit and send from there._"

	content := format.RenderMarkdown(md, true, false)
	content.MsgType = event.MsgNotice
	if _, err := intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: &content}, nil); err != nil {
		logger.Warn().Err(err).Msg("Draft poll: SendMessage to portal failed")
		return
	}
	logger.Info().Str("subject", subject).Int("body_len", len(body)).Msg("Draft posted to portal")
}

// backlogMaxDays caps how far back the !matrimail backlog command will scan.
// Gmail's `newer_than:` query operator accepts arbitrary ranges, but scanning
// a wide window is expensive (one messages.list pagination per monitored
// label) and risks rate limits.
const backlogMaxDays = 30

// fnBacklog scans the past N days for messages bearing any of the account's
// monitored Gmail labels and feeds them through the bridge as if they had
// just been seen by the history poller. Each message goes through the
// processor's existing Message-ID dedup, so already-bridged messages are
// no-ops; the typical use is recovering messages the old (pre-labelAdded)
// poller missed.
//
// Usage:
//
//	!matrimail backlog              # 1 day, all modify-mode accounts
//	!matrimail backlog 7            # 7 days, all modify-mode accounts
//	!matrimail backlog 3 you@x.com  # 3 days, just one account
func fnBacklog(ce *commands.Event, connector *EmailConnector) {
	logins := ce.User.GetUserLogins()
	if len(logins) == 0 {
		ce.Reply("ℹ️ You're not connected to any email accounts. Use `!matrimail login` to get started.")
		return
	}

	lookbackDays := 1
	emailFilter := ""
	if len(ce.Args) >= 1 {
		var n int
		if _, perr := fmt.Sscanf(ce.Args[0], "%d", &n); perr != nil || n <= 0 {
			ce.Reply("❌ First argument must be a positive number of days. Usage: `!matrimail backlog [days] [email]`")
			return
		}
		if n > backlogMaxDays {
			ce.Reply("❌ Lookback capped at %d days (got %d). Run multiple smaller backlogs if you need more history.", backlogMaxDays, n)
			return
		}
		lookbackDays = n
	}
	if len(ce.Args) >= 2 {
		emailFilter = strings.TrimSpace(ce.Args[1])
	}

	if connector.GmailInbound == nil {
		ce.Reply("❌ Gmail inbound manager isn't initialized — backlog only works for modify-scope Gmail OAuth accounts.")
		return
	}

	var targets []string
	for _, login := range logins {
		client, ok := login.Client.(*EmailClient)
		if !ok || client == nil {
			continue
		}
		if !client.GmailAPIInbound {
			continue
		}
		if emailFilter != "" && client.Email != emailFilter {
			continue
		}
		targets = append(targets, client.Email)
	}
	if len(targets) == 0 {
		if emailFilter != "" {
			ce.Reply("❌ No modify-scope Gmail account matching **%s** is connected. (`!matrimail list` to inspect.)", emailFilter)
		} else {
			ce.Reply("❌ No modify-scope Gmail accounts connected — backlog only works for Gmail OAuth in modify scope.")
		}
		return
	}

	ce.Reply("🔁 Scanning last **%d day(s)** of monitored labels on %d account(s): %s …", lookbackDays, len(targets), strings.Join(targets, ", "))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var lines []string
	totalFed := 0
	for _, emailAddr := range targets {
		fed, err := connector.GmailInbound.Backlog(ctx, ce.User.MXID.String(), emailAddr, lookbackDays)
		if err != nil {
			lines = append(lines, fmt.Sprintf("• ❌ **%s**: %s", emailAddr, err.Error()))
			continue
		}
		totalFed += fed
		lines = append(lines, fmt.Sprintf("• ✅ **%s**: queued %d message(s)", emailAddr, fed))
	}

	ce.Reply("Backlog scan complete — %d message(s) queued total. Existing rooms will get the missing messages; new threads will create rooms as usual.\n\n%s", totalFed, strings.Join(lines, "\n"))
}
