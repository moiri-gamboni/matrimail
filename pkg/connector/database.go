package connector

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/oauth2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// AuthType identifies which credential mechanism is in use for an email account.
const (
	AuthTypePassword              = "password"                 // legacy: SMTP+IMAP via app password / normal password
	AuthTypeOAuthGmail            = "oauth-gmail"              // Gmail / Workspace via Google OAuth (auth code + PKCE + loopback)
	AuthTypeOAuthGmailNeedsReauth = "oauth-gmail-needs-reauth" // refresh token expired/revoked; user must run !matrimail login
)

// OAuthProvider identifiers persisted in EmailAccount.OAuthProvider.
const (
	OAuthProviderGoogle    = "google"
	OAuthProviderMicrosoft = "microsoft" // reserved for v2; not yet emitted
)

// OAuth scope modes — controls whether the account uses Gmail-API-only access
// (gmail.modify, sensitive scope, can be published without CASA) or full
// IMAP/SMTP XOAUTH2 access (mail.google.com, restricted scope, locked into
// Testing-mode publishing → 7-day refresh tokens).
const (
	ScopeModeModify = "modify" // Gmail API only; default; recommended
	ScopeModeFull   = "full"   // mail.google.com; advanced/opt-in; required for IMAP
)

// EmailAccount represents a stored email account with credentials.
//
// Authentication is one of two modes, indicated by AuthType:
//
//   - AuthTypePassword (default for backward compat): Password is an app
//     password used with PLAIN auth on SMTP and IMAP.
//   - AuthTypeOAuthGmail: OAuthRefreshToken / OAuthAccessToken / OAuthExpiry
//     hold the encrypted Google OAuth tokens. Password is unused.
type EmailAccount struct {
	UserMXID         string    `json:"user_mxid"`
	Email            string    `json:"email"`
	Username         string    `json:"username"`
	Password         string    `json:"password"`
	Host             string    `json:"host"`
	Port             int       `json:"port"`
	TLS              bool      `json:"tls"`
	CreatedAt        time.Time `json:"created_at"`
	LastSyncTime     time.Time `json:"last_sync_time"`
	MonitoredFolders []string  `json:"monitored_folders"` // List of folders to monitor (e.g., ["INBOX", "BridgeToBeeper"])

	// AuthType is "password" or "oauth-gmail". Empty value reads as "password"
	// for legacy rows.
	AuthType string `json:"auth_type"`

	// OAuth fields populated when AuthType == AuthTypeOAuthGmail. Tokens stored
	// in the database are AES-GCM encrypted; the in-memory struct holds plaintext
	// after a successful Load*. Expiry is the access-token expiry as time.Time.
	OAuthProvider     string    `json:"oauth_provider,omitempty"`
	OAuthRefreshToken string    `json:"oauth_refresh_token,omitempty"`
	OAuthAccessToken  string    `json:"oauth_access_token,omitempty"`
	OAuthExpiry       time.Time `json:"oauth_expiry,omitempty"`

	// ScopeMode is "modify" (default; Gmail API only) or "full" (mail.google.com
	// for IMAP+SMTP XOAUTH2). Set at login time by the user's choice; consulted
	// by createIMAPClient and the Gmail API inbound poller to pick a transport.
	ScopeMode string `json:"scope_mode,omitempty"`

	// OAuthIssuedAt records when the current refresh token was minted. Used to
	// warn the user at day 6 if their app is in Testing mode (7-day refresh-token
	// expiry). Zero means unknown / pre-migration row.
	OAuthIssuedAt time.Time `json:"oauth_issued_at,omitempty"`
}

// GetMonitoredFoldersJSON serializes MonitoredFolders to JSON for database storage
func (a *EmailAccount) GetMonitoredFoldersJSON() string {
	if len(a.MonitoredFolders) == 0 {
		return `["INBOX"]` // Default to INBOX if not set
	}
	data, err := json.Marshal(a.MonitoredFolders)
	if err != nil {
		return `["INBOX"]`
	}
	return string(data)
}

// SetMonitoredFoldersFromJSON deserializes MonitoredFolders from JSON database storage
func (a *EmailAccount) SetMonitoredFoldersFromJSON(jsonStr string) {
	if jsonStr == "" {
		a.MonitoredFolders = []string{"INBOX"}
		return
	}
	if err := json.Unmarshal([]byte(jsonStr), &a.MonitoredFolders); err != nil {
		a.MonitoredFolders = []string{"INBOX"}
	}
}

// EmailAccountQuery handles database operations for email accounts
type EmailAccountQuery struct {
	DB *database.Database
}

// --- Minimal AES-GCM helper and key management (self-contained) ---

const encPrefix = "v2:"

const (
	pbkdf2Iterations = 100000 // PBKDF2 iterations for key derivation
	saltSize         = 32     // Salt size in bytes
)

var (
	keyOnce sync.Once
	dbKey   []byte
	keyErr  error
)

func getDBKey() ([]byte, error) {
	keyOnce.Do(func() {
		// Step 1: Check environment variable (highest priority for production)
		passphrase := strings.TrimSpace(os.Getenv("MATRIMAIL_PASSPHRASE"))

		// Step 2: Check for passphrase file if env var not set.
		//
		// "Absent" and "present but unreadable" must not be conflated. A file
		// that exists and cannot be read -- permissions changed, a volume
		// mounted late, an I/O error -- used to fall through to step 3, which
		// then overwrote that very file with a fresh key. That converts a
		// recoverable state (key intact, temporarily unreadable) into an
		// unrecoverable one, and for an OAuth account the lost credential is a
		// mailbox refresh token. Refusing to start is a visible failure;
		// starting with the wrong key is not.
		if passphrase == "" {
			p, err := readPassphraseFile()
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				keyErr = fmt.Errorf("passphrase file exists but could not be read (refusing to start rather than overwrite it): %w", err)
				return
			}
			passphrase = p
		}

		// Step 3: Auto-generate secure passphrase if neither exists
		if passphrase == "" {
			passphrase, keyErr = generateAndStorePassphrase()
			if keyErr != nil {
				return
			}
		}

		salt, err := getSalt()
		if err != nil {
			keyErr = fmt.Errorf("failed to get salt: %w", err)
			return
		}

		// Derive key using PBKDF2
		dbKey = pbkdf2.Key([]byte(passphrase), salt, pbkdf2Iterations, 32, sha256.New)
	})
	if keyErr != nil {
		return nil, keyErr
	}
	if len(dbKey) != 32 {
		return nil, fmt.Errorf("derived key must be 32 bytes, got %d", len(dbKey))
	}
	return dbKey, nil
}

// getLegacyConfigDir returns the platform's user-config dir under
// ~/.config/matrimail / AppData / Library — the historical passphrase
// location. New writes go to ./data/ via getPassphraseFilePath, but reads
// still consult this path for installations that already auto-generated a
// passphrase before the move (so an upgrade doesn't blow away the existing
// key and force a re-login of every account).
func getLegacyConfigDir() (string, error) {
	if configDir := os.Getenv("XDG_CONFIG_HOME"); configDir != "" {
		return filepath.Join(configDir, "matrimail"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(homeDir, "AppData", "Roaming", "Matrimail"), nil
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", "Matrimail"), nil
	default:
		return filepath.Join(homeDir, ".config", "matrimail"), nil
	}
}

// getPassphraseFilePath returns the canonical passphrase path:
// ./data/passphrase relative to the bridge's CWD. Co-located with the
// SQLite DB and matrimail.salt — both already inside ./data/ and on the
// persistent volume in the example docker-compose. Storing the passphrase
// elsewhere (the old ~/.config/matrimail/passphrase) means that on
// Distroless containers where only ./data/ is volume-mounted, every
// restart loses the auto-generated passphrase, existing encrypted
// credentials become undecryptable, and the user gets logged out on every
// restart.
func getPassphraseFilePath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}
	return filepath.Join(cwd, "data", "passphrase"), nil
}

// readPassphraseFile reads the passphrase from ./data/passphrase, falling
// back to the legacy ~/.config/matrimail/passphrase location for backward
// compat with installations that auto-generated their key before the move.
func readPassphraseFile() (string, error) {
	primary, err := getPassphraseFilePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(primary)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		// The primary key exists and we cannot read it. Falling through to the
		// legacy location would return that location's not-exist error, which
		// the caller would read as "no key yet" and regenerate -- overwriting
		// the very file we failed to read. Report the real error instead.
		return "", fmt.Errorf("read %s: %w", primary, err)
	}
	legacyDir, err := getLegacyConfigDir()
	if err != nil {
		return "", err
	}
	data, err = os.ReadFile(filepath.Join(legacyDir, "passphrase"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// generateAndStorePassphrase creates a new secure passphrase and writes it to
// the primary location (./data/passphrase). Co-located with the SQLite DB
// and salt so a single volume mount persists everything the encryption
// layer needs across container restarts.
func generateAndStorePassphrase() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random passphrase: %w", err)
	}
	passphrase := base64.StdEncoding.EncodeToString(randomBytes)

	passphrasePath, err := getPassphraseFilePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(passphrasePath), 0o700); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}
	// O_EXCL, not a plain write: auto-generation is a first-run affordance and
	// has no business replacing an existing key. Without it, any path that
	// reaches here with a key already on disk silently makes every stored
	// credential undecryptable.
	f, err := os.OpenFile(passphrasePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("refusing to generate a passphrase over %s: %w", passphrasePath, err)
	}
	if _, err := f.WriteString(passphrase); err != nil {
		f.Close()
		return "", fmt.Errorf("failed to write passphrase file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("failed to write passphrase file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Auto-generated secure passphrase stored at %s\n", passphrasePath)
	fmt.Fprintf(os.Stderr, "Set MATRIMAIL_PASSPHRASE in your environment to override this default.\n")

	return passphrase, nil
}

// getSalt returns the salt for PBKDF2, generating one if needed
func getSalt() ([]byte, error) {
	// Use absolute path for security - prevent directory traversal attacks
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}
	dataDir := filepath.Join(cwd, "data")
	saltPath := filepath.Join(dataDir, "matrimail.salt")

	// Try to read existing salt
	if data, err := os.ReadFile(saltPath); err == nil {
		salt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err == nil && len(salt) == saltSize {
			return salt, nil
		}
	}

	// Generate new salt
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	// Create directory with secure permissions
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	// Save salt
	saltB64 := base64.StdEncoding.EncodeToString(salt)
	if err := os.WriteFile(saltPath, []byte(saltB64), 0o600); err != nil {
		return nil, fmt.Errorf("failed to save salt: %w", err)
	}

	return salt, nil
}

// VerifyKeyMatchesStoredCredentials refuses to continue when the passphrase
// currently in effect cannot decrypt credentials that are already stored.
//
// Nothing re-encrypts stored rows when the passphrase changes. Set
// MATRIMAIL_PASSPHRASE to a different value, or start with the data directory's
// passphrase file missing so a fresh one is generated, and every stored
// credential silently becomes unreadable: an app password the user has to find
// again, or a Gmail refresh token that cannot be recovered at all. Before this
// check the bridge started anyway and reported the damage one account at a
// time, as ordinary decrypt failures, long after the cause.
//
// A wrong passphrase fails every row, so one successful decrypt is proof the
// key is right and a single corrupt row is not mistaken for it. A database
// holding no encrypted credentials yet is a fresh install and passes.
func (eaq *EmailAccountQuery) VerifyKeyMatchesStoredCredentials(ctx context.Context) error {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT COALESCE(password, ''), COALESCE(oauth_refresh_token, '') FROM email_accounts
	`))
	if err != nil {
		return fmt.Errorf("read stored credentials: %w", err)
	}
	defer rows.Close()

	var stored []string
	for rows.Next() {
		var password, refresh string
		if err := rows.Scan(&password, &refresh); err != nil {
			return fmt.Errorf("read stored credentials: %w", err)
		}
		stored = append(stored, password, refresh)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read stored credentials: %w", err)
	}
	return keyMatchesStored(stored, decryptString)
}

// keyMatchesStored holds the decision so it can be tested; the key is derived
// once per process, so a test cannot exercise the real thing with two different
// passphrases.
func keyMatchesStored(stored []string, decrypt func(string) (string, error)) error {
	encrypted := 0
	for _, v := range stored {
		if !strings.HasPrefix(v, encPrefix) {
			continue
		}
		encrypted++
		if _, err := decrypt(v); err == nil {
			return nil
		}
	}
	if encrypted == 0 {
		return nil
	}
	return fmt.Errorf("the passphrase in use cannot decrypt any of the %d stored credentials. "+
		"Nothing re-encrypts them when the passphrase changes, so this almost certainly means "+
		"MATRIMAIL_PASSPHRASE now holds a different value than when these accounts were added, or the "+
		"data directory's passphrase file was lost and a new one was generated. Restore the previous "+
		"passphrase and the accounts will work again; without it they cannot be recovered and each "+
		"account has to be logged in again", encrypted)
}

func encryptString(plain string) (string, error) {
	key, err := getDBKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	buf := append(nonce, ct...)
	return encPrefix + base64.StdEncoding.EncodeToString(buf), nil
}

func decryptString(stored string) (string, error) {
	// Check for old v1 encrypted data and provide helpful error message
	if strings.HasPrefix(stored, "v1:") {
		return "", errors.New("cannot decrypt old v1 encrypted data - please delete your database and reconfigure your email accounts with the new secure system")
	}

	// Only accept v2 encrypted data
	if !strings.HasPrefix(stored, encPrefix) {
		return "", errors.New("value is not encrypted with expected v2: prefix")
	}

	key, err := getDBKey()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce := raw[:gcm.NonceSize()]
	ct := raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (eaq *EmailAccountQuery) CreateTable(ctx context.Context) error {
	// Create table. New columns (auth_type and the oauth_* group) are NULLable
	// for the migration path on existing databases; the ALTER statements below
	// take care of adding them when CREATE TABLE IF NOT EXISTS finds an older
	// schema already in place.
	// Note on column types: the four columns below that store Go nanosecond
	// timestamps (oauth_expiry, oauth_token_issued_at, last_reauth_notice_at)
	// or 64-bit Gmail history IDs (oauth_history_id) MUST be BIGINT, not
	// INTEGER. Postgres INTEGER is 32-bit (max ~2.1B) and overflows on values
	// like UnixNano() ≈ 1.7×10¹⁸; SQLite's INTEGER affinity is 64-bit so it
	// doesn't care either way. Use BIGINT in the canonical schema and a
	// best-effort `ALTER COLUMN ... TYPE BIGINT` on Postgres for any existing
	// deployments that predate this migration.
	_, err := eaq.DB.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS email_accounts (
			user_mxid TEXT NOT NULL,
			email TEXT NOT NULL,
			username TEXT NOT NULL,
			password TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			tls BOOLEAN NOT NULL,
			created_at TIMESTAMP NOT NULL,
			last_sync_time TIMESTAMP,
			monitored_folders TEXT DEFAULT '["INBOX"]',
			auth_type TEXT NOT NULL DEFAULT 'password',
			oauth_provider TEXT,
			oauth_refresh_token TEXT,
			oauth_access_token TEXT,
			oauth_expiry BIGINT,
			scope_mode TEXT,
			oauth_token_issued_at BIGINT,
			oauth_history_id BIGINT,
			last_reauth_notice_at BIGINT,
			PRIMARY KEY (user_mxid, email)
		)
	`)
	if err != nil {
		return err
	}

	// Migration ALTERs: SQLite returns an error when the column already exists,
	// which we deliberately swallow. Each statement is independent so a single
	// already-applied migration doesn't block the rest.
	for _, stmt := range []string{
		`ALTER TABLE email_accounts ADD COLUMN monitored_folders TEXT DEFAULT '["INBOX"]'`,
		`ALTER TABLE email_accounts ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'password'`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_provider TEXT`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_refresh_token TEXT`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_access_token TEXT`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_expiry BIGINT`,
		// New (auth-code rework):
		`ALTER TABLE email_accounts ADD COLUMN scope_mode TEXT`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_token_issued_at BIGINT`,
		`ALTER TABLE email_accounts ADD COLUMN oauth_history_id BIGINT`,
		`ALTER TABLE email_accounts ADD COLUMN last_reauth_notice_at BIGINT`,
	} {
		_, _ = eaq.DB.Exec(ctx, stmt)
	}

	// Postgres-only widening migrations for deployments that originally
	// CREATE'd these columns as INTEGER (32-bit) before this fix. SQLite's
	// INTEGER affinity is already 64-bit and the syntax differs, so skip
	// there. Errors are swallowed because the columns may have been BIGINT
	// from the start (fresh deploys after this fix), in which case the ALTER
	// is a no-op that Postgres returns success on anyway.
	if eaq.DB.Dialect == dbutil.Postgres {
		for _, stmt := range []string{
			`ALTER TABLE email_accounts ALTER COLUMN oauth_expiry TYPE BIGINT`,
			`ALTER TABLE email_accounts ALTER COLUMN oauth_token_issued_at TYPE BIGINT`,
			`ALTER TABLE email_accounts ALTER COLUMN oauth_history_id TYPE BIGINT`,
			`ALTER TABLE email_accounts ALTER COLUMN last_reauth_notice_at TYPE BIGINT`,
		} {
			_, _ = eaq.DB.Exec(ctx, stmt)
		}
	}

	// Create performance indexes
	_, err = eaq.DB.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_email_accounts_user_created
		ON email_accounts(user_mxid, created_at)
	`)
	if err != nil {
		return err
	}

	_, err = eaq.DB.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_email_accounts_last_sync
		ON email_accounts(user_mxid, last_sync_time)
	`)

	return err
}

// SaveOAuthToken persists the OAuth refresh + access token for the given
// account, encrypting both tokens at rest with the same AES-GCM key used for
// passwords. Sets auth_type='oauth-gmail' and oauth_provider=provider, and
// preserves any existing scope_mode (so token-refresh callers don't have to
// re-pass it). Use SaveOAuthTokenWithScope at login time to set scope_mode.
//
// The account row must already exist (created by the login flow). This method
// does NOT create the row — it updates it in place.
func (eaq *EmailAccountQuery) SaveOAuthToken(ctx context.Context, userMXID, email, provider string, tok *oauth2.Token) error {
	return eaq.saveOAuthTokenInternal(ctx, userMXID, email, provider, "", tok, false)
}

// SaveOAuthTokenWithScope is the login-time variant: it explicitly sets
// scope_mode and stamps oauth_token_issued_at = now. Refresh-time callers
// should use SaveOAuthToken (which preserves the existing scope_mode and does
// not bump issued_at).
func (eaq *EmailAccountQuery) SaveOAuthTokenWithScope(ctx context.Context, userMXID, email, provider, scopeMode string, tok *oauth2.Token) error {
	if scopeMode == "" {
		scopeMode = ScopeModeModify
	}
	return eaq.saveOAuthTokenInternal(ctx, userMXID, email, provider, scopeMode, tok, true)
}

func (eaq *EmailAccountQuery) saveOAuthTokenInternal(ctx context.Context, userMXID, email, provider, scopeMode string, tok *oauth2.Token, stampIssuedAt bool) error {
	if tok == nil {
		return errors.New("SaveOAuthToken: nil token")
	}
	if provider == "" {
		provider = OAuthProviderGoogle
	}

	encRefresh, err := encryptString(tok.RefreshToken)
	if err != nil {
		return fmt.Errorf("encrypt refresh token: %w", err)
	}
	encAccess, err := encryptString(tok.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}

	expiryNanos := tok.Expiry.UnixNano()
	if tok.Expiry.IsZero() {
		expiryNanos = 0
	}

	authType := AuthTypeOAuthGmail
	var (
		res sql.Result
	)
	if scopeMode != "" {
		// Login-time path: set scope_mode + issued_at.
		issuedAt := int64(0)
		if stampIssuedAt {
			issuedAt = time.Now().UnixNano()
		}
		res, err = eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
			UPDATE email_accounts
			SET auth_type = ?, oauth_provider = ?, oauth_refresh_token = ?, oauth_access_token = ?, oauth_expiry = ?,
			    scope_mode = ?, oauth_token_issued_at = ?, last_reauth_notice_at = 0
			WHERE user_mxid = ? AND email = ?
		`), authType, provider, encRefresh, encAccess, expiryNanos, scopeMode, issuedAt, userMXID, email)
	} else {
		// Refresh-time path: leave scope_mode + issued_at untouched.
		res, err = eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
			UPDATE email_accounts
			SET auth_type = ?, oauth_provider = ?, oauth_refresh_token = ?, oauth_access_token = ?, oauth_expiry = ?
			WHERE user_mxid = ? AND email = ?
		`), authType, provider, encRefresh, encAccess, expiryNanos, userMXID, email)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("SaveOAuthToken: no email_accounts row for %s/%s", userMXID, email)
	}
	return nil
}

// SetAuthType updates only the auth_type column. Used by the re-auth path to
// flip an account into AuthTypeOAuthGmailNeedsReauth (and back to
// AuthTypeOAuthGmail when the user re-logs in).
func (eaq *EmailAccountQuery) SetAuthType(ctx context.Context, userMXID, email, authType string) error {
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		UPDATE email_accounts SET auth_type = ? WHERE user_mxid = ? AND email = ?
	`), authType, userMXID, email)
	return err
}

// MarkReauthNotified records that we sent a re-auth DM at the given time, used
// to debounce repeat notifications. Returns whether the timestamp was actually
// stored (i.e. the row exists).
func (eaq *EmailAccountQuery) MarkReauthNotified(ctx context.Context, userMXID, email string, at time.Time) error {
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		UPDATE email_accounts SET last_reauth_notice_at = ? WHERE user_mxid = ? AND email = ?
	`), at.UnixNano(), userMXID, email)
	return err
}

// LastReauthNotifiedAt returns when (if ever) we last sent a re-auth DM for
// this account. Zero time means never (or the account has been reset).
func (eaq *EmailAccountQuery) LastReauthNotifiedAt(ctx context.Context, userMXID, email string) (time.Time, error) {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT COALESCE(last_reauth_notice_at, 0) FROM email_accounts WHERE user_mxid = ? AND email = ?
	`), userMXID, email)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, rows.Err()
	}
	var nanos sql.NullInt64
	if err := rows.Scan(&nanos); err != nil {
		return time.Time{}, err
	}
	if !nanos.Valid || nanos.Int64 == 0 {
		return time.Time{}, nil
	}
	return time.Unix(0, nanos.Int64), nil
}

// GetGmailHistoryID / SetGmailHistoryID persist the Gmail API users.history.list
// cursor for accounts using ScopeModeModify. Zero means "no cursor yet — sync
// from the current historyId on first poll".
func (eaq *EmailAccountQuery) GetGmailHistoryID(ctx context.Context, userMXID, email string) (uint64, error) {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT COALESCE(oauth_history_id, 0) FROM email_accounts WHERE user_mxid = ? AND email = ?
	`), userMXID, email)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var raw sql.NullInt64
	if err := rows.Scan(&raw); err != nil {
		return 0, err
	}
	if !raw.Valid || raw.Int64 < 0 {
		return 0, nil
	}
	return uint64(raw.Int64), nil
}

func (eaq *EmailAccountQuery) SetGmailHistoryID(ctx context.Context, userMXID, email string, historyID uint64) error {
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		UPDATE email_accounts SET oauth_history_id = ? WHERE user_mxid = ? AND email = ?
	`), int64(historyID), userMXID, email)
	return err
}

// LoadOAuthToken returns the saved OAuth token for the account. If the account
// is not in OAuth mode (auth_type != "oauth-gmail" / "oauth-gmail-needs-reauth")
// it returns ("", nil, nil) so callers can fall back to the password path.
//
// Both AuthTypeOAuthGmail and AuthTypeOAuthGmailNeedsReauth return the token
// (the refresh token may still be valid even if a previous attempt failed —
// e.g. transient network errors flagged the row but the user hasn't run
// !matrimail login yet). Callers responsible for connection setup should
// inspect the auth_type via GetOAuthAccount and skip connecting if it's
// flagged for re-auth.
func (eaq *EmailAccountQuery) LoadOAuthToken(ctx context.Context, userMXID, email string) (string, *oauth2.Token, error) {
	info, err := eaq.GetOAuthAccount(ctx, userMXID, email)
	if err != nil || info == nil {
		return "", nil, err
	}
	return info.Provider, info.Token, nil
}

// OAuthAccountInfo bundles everything LoadOAuthToken returns plus the auth
// state and scope mode. Callers that need to make routing decisions
// (Gmail-API vs IMAP, skip-because-needs-reauth) should use this instead of
// LoadOAuthToken.
type OAuthAccountInfo struct {
	AuthType  string        // AuthTypeOAuthGmail or AuthTypeOAuthGmailNeedsReauth
	Provider  string        // OAuthProviderGoogle, etc.
	ScopeMode string        // ScopeModeModify | ScopeModeFull
	Token     *oauth2.Token // nil if no refresh token stored
	IssuedAt  time.Time     // when the current refresh token was minted; zero if unknown
}

// GetOAuthAccount returns nil, nil when the account is not in OAuth mode (so
// callers can fall through to the password path). Returns a non-nil
// *OAuthAccountInfo for both AuthTypeOAuthGmail and AuthTypeOAuthGmailNeedsReauth.
func (eaq *EmailAccountQuery) GetOAuthAccount(ctx context.Context, userMXID, email string) (*OAuthAccountInfo, error) {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT auth_type, COALESCE(oauth_provider, ''), COALESCE(oauth_refresh_token, ''),
		       COALESCE(oauth_access_token, ''), COALESCE(oauth_expiry, 0),
		       COALESCE(scope_mode, ''), COALESCE(oauth_token_issued_at, 0)
		FROM email_accounts
		WHERE user_mxid = ? AND email = ?
	`), userMXID, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if rerr := rows.Err(); rerr != nil {
			return nil, rerr
		}
		return nil, nil
	}

	var authType, provider, encRefresh, encAccess, scopeMode string
	var expiryNanos, issuedAtNanos sql.NullInt64
	if err := rows.Scan(&authType, &provider, &encRefresh, &encAccess, &expiryNanos, &scopeMode, &issuedAtNanos); err != nil {
		return nil, err
	}

	if authType != AuthTypeOAuthGmail && authType != AuthTypeOAuthGmailNeedsReauth {
		return nil, nil
	}
	info := &OAuthAccountInfo{AuthType: authType, Provider: provider, ScopeMode: scopeMode}
	if info.ScopeMode == "" {
		// Pre-migration rows or rows from before scope_mode existed: assume
		// the legacy "full" mode (mail.google.com via XOAUTH2). New OAuth
		// logins always set scope_mode explicitly via SaveOAuthTokenWithScope.
		info.ScopeMode = ScopeModeFull
	}
	if issuedAtNanos.Valid && issuedAtNanos.Int64 > 0 {
		info.IssuedAt = time.Unix(0, issuedAtNanos.Int64)
	}
	if encRefresh == "" {
		// Flagged for re-auth and the refresh token has already been wiped —
		// the row is just a placeholder. Return the info shell with no token.
		return info, nil
	}

	refresh, err := decryptString(encRefresh)
	if err != nil {
		return info, fmt.Errorf("decrypt refresh token: %w", err)
	}
	var access string
	if encAccess != "" {
		access, err = decryptString(encAccess)
		if err != nil {
			// Access token may be missing or stale; that's fine, the refresh
			// token will mint a new one.
			access = ""
		}
	}

	info.Token = &oauth2.Token{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
	}
	if expiryNanos.Valid && expiryNanos.Int64 > 0 {
		info.Token.Expiry = time.Unix(0, expiryNanos.Int64)
	}
	return info, nil
}

func (eaq *EmailAccountQuery) GetAccount(ctx context.Context, userMXID, email string) (*EmailAccount, error) {
	account := &EmailAccount{}
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT user_mxid, email, username, password, host, port, tls, created_at, last_sync_time,
		       COALESCE(monitored_folders, '["INBOX"]'), COALESCE(auth_type, 'password')
		FROM email_accounts
		WHERE user_mxid = ? AND email = ?
	`), userMXID, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		// Check if Next() failed due to an error
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("database query failed: %w", err)
		}
		return nil, nil // No account found
	}

	var monitoredFoldersJSON string
	err = rows.Scan(
		&account.UserMXID, &account.Email, &account.Username, &account.Password,
		&account.Host, &account.Port, &account.TLS, &account.CreatedAt, &account.LastSyncTime,
		&monitoredFoldersJSON, &account.AuthType,
	)
	if err != nil {
		return nil, err
	}
	account.SetMonitoredFoldersFromJSON(monitoredFoldersJSON)
	// Decrypt password (fresh deployments always store encrypted)
	plain, derr := decryptString(account.Password)
	if derr != nil {
		// Don't expose decryption details to prevent information disclosure
		return nil, fmt.Errorf("failed to decrypt stored credentials")
	}
	account.Password = plain
	return account, nil
}

func (eaq *EmailAccountQuery) GetUserAccounts(ctx context.Context, userMXID string) ([]*EmailAccount, error) {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT user_mxid, email, username, password, host, port, tls, created_at, last_sync_time,
		       COALESCE(monitored_folders, '["INBOX"]'), COALESCE(auth_type, 'password')
		FROM email_accounts
		WHERE user_mxid = ?
		ORDER BY created_at ASC
	`), userMXID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []*EmailAccount
	for rows.Next() {
		account := &EmailAccount{}
		var monitoredFoldersJSON string
		err = rows.Scan(
			&account.UserMXID, &account.Email, &account.Username, &account.Password,
			&account.Host, &account.Port, &account.TLS, &account.CreatedAt, &account.LastSyncTime,
			&monitoredFoldersJSON, &account.AuthType,
		)
		if err != nil {
			return nil, err
		}
		account.SetMonitoredFoldersFromJSON(monitoredFoldersJSON)
		plain, derr := decryptString(account.Password)
		if derr != nil {
			// Don't expose decryption details to prevent information disclosure
			return nil, fmt.Errorf("failed to decrypt stored credentials")
		}
		account.Password = plain
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// GetUserAccountsBasic returns user accounts without decrypting passwords (for display/status)
func (eaq *EmailAccountQuery) GetUserAccountsBasic(ctx context.Context, userMXID string) ([]*EmailAccount, error) {
	rows, err := eaq.DB.Query(ctx, dialectQuery(eaq.DB.Dialect, `
		SELECT user_mxid, email, username, host, port, tls, created_at, last_sync_time,
		       COALESCE(monitored_folders, '["INBOX"]'), COALESCE(auth_type, 'password')
		FROM email_accounts
		WHERE user_mxid = ?
		ORDER BY created_at ASC
	`), userMXID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Pre-allocate slice with reasonable capacity to reduce reallocations
	accounts := make([]*EmailAccount, 0, 4)
	for rows.Next() {
		account := &EmailAccount{}
		var monitoredFoldersJSON string
		err = rows.Scan(
			&account.UserMXID, &account.Email, &account.Username,
			&account.Host, &account.Port, &account.TLS, &account.CreatedAt, &account.LastSyncTime,
			&monitoredFoldersJSON, &account.AuthType,
		)
		if err != nil {
			return nil, err
		}
		account.SetMonitoredFoldersFromJSON(monitoredFoldersJSON)
		// Password is left empty for basic account info
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (eaq *EmailAccountQuery) UpsertAccount(ctx context.Context, account *EmailAccount) error {
	enc, err := encryptString(account.Password)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %w", err)
	}
	// ON CONFLICT DO UPDATE works on both SQLite 3.24+ and Postgres 9.5+.
	// Note: this UPSERT only touches the password-flow columns; auth_type and
	// oauth_* columns are managed separately via SaveOAuthToken/UpdateMonitoredFolders.
	_, err = eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		INSERT INTO email_accounts
		(user_mxid, email, username, password, host, port, tls, created_at, last_sync_time, monitored_folders)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_mxid, email) DO UPDATE SET
			username = EXCLUDED.username,
			password = EXCLUDED.password,
			host = EXCLUDED.host,
			port = EXCLUDED.port,
			tls = EXCLUDED.tls,
			last_sync_time = EXCLUDED.last_sync_time,
			monitored_folders = EXCLUDED.monitored_folders
	`), account.UserMXID, account.Email, account.Username, enc,
		account.Host, account.Port, account.TLS, account.CreatedAt, account.LastSyncTime,
		account.GetMonitoredFoldersJSON())
	return err
}

func (eaq *EmailAccountQuery) DeleteAccount(ctx context.Context, userMXID, email string) error {
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		DELETE FROM email_accounts
		WHERE user_mxid = ? AND email = ?
	`), userMXID, email)
	return err
}

func (eaq *EmailAccountQuery) UpdateLastSync(ctx context.Context, userMXID, email string, syncTime time.Time) error {
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		UPDATE email_accounts
		SET last_sync_time = ?
		WHERE user_mxid = ? AND email = ?
	`), syncTime, userMXID, email)
	return err
}

// UpdateMonitoredFolders persists a fresh folder list onto an existing
// account row without going through INSERT OR REPLACE (which would erase any
// auth_type / oauth_* columns set on the side via SaveOAuthToken). Used by
// the OAuth login flow's completeLogin step.
func (eaq *EmailAccountQuery) UpdateMonitoredFolders(ctx context.Context, userMXID, email string, folders []string) error {
	tmp := &EmailAccount{MonitoredFolders: folders}
	_, err := eaq.DB.Exec(ctx, dialectQuery(eaq.DB.Dialect, `
		UPDATE email_accounts
		SET monitored_folders = ?
		WHERE user_mxid = ? AND email = ?
	`), tmp.GetMonitoredFoldersJSON(), userMXID, email)
	return err
}
