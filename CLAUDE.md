# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project context

`matrimail` is a bidirectional Matrix↔Email bridge written in Go on top of the **mautrix bridgev2** framework. The repo directory is still named `emaildawg` on disk (the binary, module path, env vars, DB filename, and bot command prefix were renamed; the working tree and the org/repo URL `github.com/Leicas/matrimail` were not). Treat README references to `emaildawg` as historical migration notes, not current naming.

The framework's portal/message DB schema keys on `NetworkID = "email"`. That string is set in `connector.EmailConnector.GetName()` and **must not change** — it's what keeps existing rooms/threads compatible across the rename.

## Build / test / run

```bash
make build                      # builds ./matrimail (uses CGO + libolm; auto-detects libolm path on macOS/Linux)
make test                       # go test ./... with the same CGO env
make clean
go test ./pkg/connector/...     # single package
go test ./pkg/email/ -run TestThreadingExtractRefs  # single test by regex
./build.sh                      # alternative build that also injects mautrix.GoModVersion ldflag
```

`build.sh` differs from `make build` by setting `maunium.net/go/mautrix.GoModVersion` via `-X`. Use it when you need the framework to report the correct mautrix version (e.g. release builds); `make build` is fine for local iteration.

### libolm is mandatory

CGO must be enabled and link against `libolm` (Debian: `libolm-dev`; macOS: `brew install libolm`). **Never build with `nocrypto`** — the bridge requires E2EE support to function correctly against real homeservers. The Makefile errors out fast if libolm headers aren't found.

### First run / config generation

```bash
./matrimail --generate-example-config        # writes config.yaml skeleton
./matrimail --generate-registration          # only needed for HTTP appservice mode (NOT websocket/Beeper)
./matrimail --config ./data/config.yaml
```

The bridgev2 framework owns CLI flag parsing (via `mxmain.BridgeMain`); the only project-level entrypoint is `cmd/matrimail/main.go`, which constructs an `EmailConnector` and hands it to `BridgeMain`.

## Architecture

The whole bridge is one binary plugged into bridgev2 as a `NetworkConnector`. Read `pkg/connector/connector.go` first — it's the central wiring point and it documents non-obvious invariants.

### Package layout (`pkg/`)

- **`connector/`** — bridgev2 plugin. `EmailConnector` implements `NetworkConnector` + `StoppableNetwork`. Owns config, the IMAP manager, room/thread managers, the sent-message dedup table, and the `EmailAccountQuery` storage. `commands.go` registers all `!matrimail …` bot commands. `login.go` drives the multi-step login flow (provider detection → credentials → folder/label selection → validation). `client_send.go` + `client_compose.go` are the outbound Matrix→email path.
- **`email/`** — protocol-agnostic email logic: RFC 5322 threading (`threading.go`), MIME processing (`processor.go` — large, holds attachment upload, tracking-pixel filtering, participant extraction), outbound assembly (`outbound.go`), and the sender abstraction (`sender.go` + factory + Gmail/SMTP/Graph implementations).
- **`imap/`** — IMAP IDLE client (`client.go`), per-account `Manager`, and folder/label discovery used during login (`folders.go`).
- **`matrix/`** — thin layer on top of bridgev2's portal/ghost APIs for room creation and participant management.
- **`coordinator/`** — `state_coordinator.go` reconciles per-account connection state across the IMAP manager and the bridgev2 lifecycle.
- **`reliability/`** — generic circuit breaker, retry, and timeout primitives consumed by the IMAP and outbound paths.
- **`common/`** — shared ID helpers (network IDs for portals, ghosts, threads).
- **`logging/`** — log sanitization (`sanitize.go`); use it for anything that might contain credentials, message bodies, or PII before logging.

### Key invariants and gotchas

- **Don't wipe `ec.Config` in `Init`.** `connector.go:60` documents this: bridgev2's `LoadConfig` populates the `network:` block (including `GmailOAuth.{ClientID,ClientSecret}`) into `ec.Config` *before* `Init` runs. Replacing the struct breaks OAuth. Only fill zero-value defaults, never reassign the whole struct.
- **One email thread = one Matrix room.** Threading uses `Message-ID` / `References` / `In-Reply-To`, resolved against the in-memory `ThreadManager` cache first (24h TTL). On a miss, `thread_resolver_db.go` asks the bridgev2 `message` table which portal a previously bridged message landed in — it reads that table, it does not own or write it. Go through the framework's typed queries there rather than hand-written SQL: the previous version guessed at table and column names, matched nothing, and reported the failure as an ordinary miss. Don't bypass the resolver when generating room keys.
- **Thread state outlives the cache in `Portal.Metadata`** (`portal_metadata.go`), which is the only durable copy of the reply context. Write it through `PersistThreadState`, never by assigning `portal.Metadata` — callers do not always hold a complete thread, and three goroutines write it.
- **Sent-folder echo suppression.** Every outbound message we send is recorded in `sent_dedup.go` keyed by `Message-ID`. The IMAP processor consults this table when an IDLE notification fires on the Sent folder so users don't see their own messages twice. Gmail-via-API uses the *server-assigned* Message-ID; SMTP submission uses ours. If you change either send path, make sure the dedup write happens with the same ID the IMAP path will see.
- **Outbound Gmail uses the Gmail API**, not SMTP, when the account was OAuth'd. This is selected in `email/sender_factory.go`. SMTP submission (port 587 + STARTTLS) is the fallback for everything else.
- **Edits, redactions, and reactions don't bridge outbound** by design — email can't be unsent. Don't add half-hearted approximations of these without an explicit ask.
- **Database**: SQLite by default; URI must point at a writable path. Container path is `/home/nonroot/app/data/matrimail.db`; host path is `./data/matrimail.db`. Postgres is supported via bridgev2 but the compose file is SQLite-only.
- **Encryption passphrase**: `MATRIMAIL_PASSPHRASE` env var (production) or auto-generated and persisted in `data/`. This protects stored IMAP/SMTP credentials at rest — losing the passphrase means re-logging in every account.

### Login flow

`!matrimail login` is a multi-step interactive flow handled by `login.go` + `login_validation.go`:

1. Provider detection from the email domain (Gmail, Yahoo, Outlook, iCloud, FastMail, … fallback to autodetect).
2. Credential prompt — either app password, or for Gmail, the OAuth Authorization Code + PKCE + RFC 8252 loopback flow (NOT the deprecated device-code flow; Google rejects `mail.google.com` on device endpoints).
3. Folder/label discovery — IMAP for password and `full`-mode OAuth, Gmail labels API for `modify`-mode OAuth.
4. Validation: connect, IDLE/poller bootstrap, log success.

Gmail OAuth has two scope modes:

- **`modify` (default)**: `gmail.modify` + `gmail.send`. Sensitive scope; can be published without CASA. Inbound is via `users.history.list` polling (`pkg/email/inbound_gmail_api.go` + `pkg/connector/gmail_inbound.go`); outbound is Gmail API. No IMAP/SMTP authorized.
- **`full` (advanced/opt-in)**: `mail.google.com`. Restricted scope; locked into Testing-mode publishing → 7-day refresh tokens. Inbound is IMAP IDLE; outbound is Gmail API (preferred for the server-Message-ID dedup) or SMTP XOAUTH2.

The OAuth state machine is in `pkg/connector/login.go`; the loopback callback listener is `pkg/connector/oauth_listener.go` (binds to `127.0.0.1:RANDOMPORT`, refuses non-loopback addresses, validates `state` server-side, 10-min default timeout). Auth-URL building / code exchange / refresh / revoke live in `pkg/email/oauth_authcode.go`.

### Re-auth UX

When a refresh token dies (revoked, password change, 7-day Testing-mode expiry), `pkg/connector/reauth.go` flips the account's `auth_type` to `oauth-gmail-needs-reauth`, fires a debounced (1/hour) DM into the user's bridge management room, and reports the `EmailReauthRequired` bridge state. Accounts in this state are skipped by `createIMAPClient` and the Gmail inbound poller — they stay resident until the user runs `!matrimail login`. Hooked into both the IMAP `SetTokenProvider` callback and the Gmail API sender's wrapper `reauthAwareTokenSource` (`pkg/connector/reauth_tokensource.go`).

### Headless / paste-token escape hatch

For deployments where the bridge host can't expose a loopback port to a browser even via SSH `-L`, `!matrimail oauth paste-token <email> <refresh_token>` accepts a refresh token obtained out-of-band, validates it against Google by exchanging for an access token, confirms identity via `users.getProfile`, then persists. Counterpart `!matrimail oauth revoke <email>` severs at Google + flips the local flag. Both live in `pkg/connector/commands.go` (`fnOAuth`).

When changing the login flow, search for `LoginStep` constants and the state machine in `login.go` — the bridgev2 framework drives this via `LoginProcess` callbacks, not free-form prompts.

## Code conventions

- Logger is `zerolog`. Get it from the bridgev2 context (`zerolog.Ctx(ctx)`); don't construct package-level loggers.
- Use `pkg/logging.SanitizeForLog` (or equivalent) before logging email subjects, addresses, headers, or bodies.
- IMAP and email parsing surface bytes — be explicit about charset decoding (the `processor.go` MIME path has helpers).
- Don't add code paths that depend on `nocrypto` builds compiling. The repo is libolm-or-nothing.
