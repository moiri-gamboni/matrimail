package connector

import (
	_ "embed"
	"time"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	// Top-level blocks to match example-config.yaml structure
	IMAP       IMAPConfig       `yaml:"imap"`
	Logging    LoggingConfig    `yaml:"logging"`
	Processing ProcessingConfig `yaml:"email_processing"`
	// GmailOAuth holds the user-provided "Desktop app" OAuth 2.0 client
	// credentials used for the device-code login flow on Gmail accounts.
	GmailOAuth GmailOAuthConfig `yaml:"gmail_oauth"`
	// BeeperSync mirrors archived and read state between Gmail threads and
	// their chats in Beeper. Off unless APIURL is set.
	BeeperSync BeeperSyncConfig `yaml:"beeper_sync"`
	// DraftWebhook configures the optional external trigger used by the
	// `!matrimail draft` command (and the reaction trigger, once enabled) to
	// ask an out-of-process workflow — typically an n8n LLM-draft flow — to
	// generate a Gmail draft for a given thread. When URL is empty the
	// command short-circuits with an "unconfigured" error.
	DraftWebhook DraftWebhookConfig `yaml:"draft_webhook"`
	// Keep Network for internal use but don't map to YAML
	Network NetworkConfig `yaml:"-"`
}

// DraftWebhookConfig is the on-disk shape of the draft_webhook: block.
type DraftWebhookConfig struct {
	// URL of the HTTP endpoint (typically an n8n webhook) to POST a JSON
	// payload to when the user runs !matrimail draft. Empty disables the
	// feature; the command will report "not configured" instead of firing.
	URL string `yaml:"url"`

	// Secret is an optional bearer token sent in the Authorization header
	// (`Authorization: Bearer <secret>`). Use this if the webhook is on the
	// public internet — the endpoint should reject requests without it.
	Secret string `yaml:"secret"`

	// TimeoutSeconds bounds how long matrimail waits for the webhook to
	// respond before giving up. Default 10. The webhook is expected to be
	// fire-and-forget from matrimail's perspective: n8n acknowledges quickly,
	// then does the actual LLM call + Gmail draft creation asynchronously.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// EffectiveTimeout returns the configured webhook timeout with a sane
// default (10s) applied when zero or negative.
func (d DraftWebhookConfig) EffectiveTimeout() time.Duration {
	if d.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(d.TimeoutSeconds) * time.Second
}

// GmailOAuthConfig is the on-disk shape of the gmail_oauth: block. The
// runtime variant lives in pkg/email; this type only carries the YAML.
type GmailOAuthConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`

	// ListenerAddress overrides the address the loopback OAuth callback server
	// binds to. Default "127.0.0.1:0" (random ephemeral port). Override only if
	// the user has a fixed SSH-tunnel port configured. The ":0" suffix means
	// the OS picks a free port — recommended.
	ListenerAddress string `yaml:"listener_address"`

	// DefaultScopeMode is "modify" (recommended; Gmail API only, sensitive
	// scope) or "full" (mail.google.com; restricted scope; required for IMAP
	// XOAUTH2 + SMTP). Per-login overridable; this is just the default the
	// login UI presents.
	DefaultScopeMode string `yaml:"default_scope_mode"`

	// CallbackTimeoutSeconds bounds how long the loopback listener waits for
	// the user's browser callback. Default 600 (10 minutes). Past this the
	// listener shuts down and the login flow errors with "authorization timed
	// out, run !matrimail login again".
	CallbackTimeoutSeconds int `yaml:"callback_timeout_seconds"`
}

// EffectiveListenerAddress returns the configured loopback bind address with
// the safe default applied. Callers should always go through this to ensure
// we never accidentally bind to 0.0.0.0 from a typo'd config.
func (g GmailOAuthConfig) EffectiveListenerAddress() string {
	if g.ListenerAddress == "" {
		return "127.0.0.1:0"
	}
	return g.ListenerAddress
}

// EffectiveDefaultScopeMode returns "modify" unless the operator has set
// "full" in config. Empty / unrecognised values fall back to "modify".
func (g GmailOAuthConfig) EffectiveDefaultScopeMode() string {
	switch g.DefaultScopeMode {
	case ScopeModeFull, ScopeModeModify:
		return g.DefaultScopeMode
	default:
		return ScopeModeModify
	}
}

// EffectiveCallbackTimeout returns the OAuth callback timeout, defaulting to
// 10 minutes when unset.
func (g GmailOAuthConfig) EffectiveCallbackTimeout() time.Duration {
	if g.CallbackTimeoutSeconds <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(g.CallbackTimeoutSeconds) * time.Second
}

type NetworkConfig struct {
	IMAP IMAPConfig `yaml:"imap"`
}

type IMAPConfig struct {
	// IMAP connection settings will be configured per-user via DM commands
	// rather than in the global config file
	DefaultTimeout            int `yaml:"default_timeout"`
	StartupBackfillSeconds    int `yaml:"startup_backfill_seconds"`
	StartupBackfillMax        int `yaml:"startup_backfill_max"`
	InitialIdleTimeoutSeconds int `yaml:"initial_idle_timeout_seconds"`
}

type LoggingConfig struct {
	// When true, redact PII from logs using a global sanitizer hook.
	Sanitized       bool   `yaml:"sanitized"`
	PseudonymSecret string `yaml:"pseudonym_secret"`
}

// ProcessingConfig holds limits and behaviors for email → Matrix conversion
// Default values are defined once below and applied at connector startup.
const DefaultMaxUploadBytes = 25 * 1024 * 1024 // 25 MiB

type ProcessingConfig struct {
	// Maximum size in bytes for a single media upload. Set 0 to disable the check.
	MaxUploadBytes int  `yaml:"max_upload_bytes"`
	// If true, attempt gzip for oversized original HTML/text bodies before attaching.
	GzipLargeBodies bool `yaml:"gzip_large_bodies"`
}

func upgradeConfig(helper up.Helper) {
	// Copy all keys that exist in the embedded example (pkg/connector/example-config.yaml)
	
	// IMAP configuration
	helper.Copy(up.Int, "imap", "default_timeout")
	helper.Copy(up.Int, "imap", "startup_backfill_seconds") 
	helper.Copy(up.Int, "imap", "startup_backfill_max")
	helper.Copy(up.Int, "imap", "initial_idle_timeout_seconds")
	
	// Email processing configuration
	helper.Copy(up.Int, "email_processing", "max_upload_bytes")
	helper.Copy(up.Bool, "email_processing", "gzip_large_bodies")
	
	// Logging configuration
	helper.Copy(up.Bool, "logging", "sanitized")
	helper.Copy(up.Str, "logging", "pseudonym_secret")

	// Gmail OAuth (Desktop app client credentials for the auth-code + PKCE
	// + loopback flow).
	helper.Copy(up.Str, "gmail_oauth", "client_id")
	helper.Copy(up.Str, "gmail_oauth", "client_secret")
	helper.Copy(up.Str, "gmail_oauth", "listener_address")
	helper.Copy(up.Str, "gmail_oauth", "default_scope_mode")
	helper.Copy(up.Int, "gmail_oauth", "callback_timeout_seconds")

	// Beeper state sync (optional; off while api_url is empty).
	helper.Copy(up.Str, "beeper_sync", "api_url")
	helper.Copy(up.Str, "beeper_sync", "token_file")
	helper.Copy(up.Int, "beeper_sync", "interval_seconds")

	// Draft webhook (optional external trigger for !matrimail draft).
	helper.Copy(up.Str, "draft_webhook", "url")
	helper.Copy(up.Str, "draft_webhook", "secret")
	helper.Copy(up.Int, "draft_webhook", "timeout_seconds")
}

func (ec *EmailConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &ec.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Blocks: [][]string{},
		Base: ExampleConfig,
	}
}
