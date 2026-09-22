# matrimail

A bidirectional Matrix↔Email bridge built on mautrix bridgev2.

## Status

This project is usable and under active development. Core features work and the bridge is suitable for personal use and small deployments.

## Migration from emaildawg

If you ran the previous `emaildawg` build, do a clean break before starting matrimail:

1. Stop the old bridge.
2. Delete the old `data/` directory (this drops `emaildawg.db`, the salt file, and any auto-generated passphrase). Credential storage is fresh — you will re-login from Matrix.
3. Update any `config.yaml` `database.uri` references from `emaildawg.db` to `matrimail.db`.
4. Update any service files / shell scripts that referenced the `emaildawg` binary or `EMAILDAWG_PASSPHRASE` env var to use `matrimail` and `MATRIMAIL_PASSPHRASE`.
5. Build and start matrimail, then `!matrimail login` to add your accounts again.

The bridgev2 portal/message DB schema uses `NetworkID = "email"` for both old and new builds, so once you log in again, room/thread keys remain compatible at the protocol layer. Only the credential / config storage is reset.

## What this bridge does for you

- **Easy setup:** Just give it your email and app password and it figures out the server settings
- **Choose your folders:** During login, pick which folders or Gmail labels to monitor — not just INBOX
- **Real-time email delivery:** New emails show up in Matrix immediately - no waiting or manual syncing
- **Handles attachments:** Photos, PDFs, documents all get uploaded to Matrix automatically
- **Smart conversation threading:** Emails in the same thread become one chat
- **Reliable connections:** Automatically reconnects if your internet hiccups or email server has issues
- **Cleans up messy emails:** Filters out tracking pixels and tiny placeholder images that clutter your conversations
- **Secure storage:** Your email credentials are encrypted on your machine
- **Reply or compose new threads from Matrix:** Send a message in a thread room to reply, or use `!matrimail compose to:foo@bar.com` to start a new email thread.

## Architecture

- Implemented in Go using the mautrix bridgev2 framework
- One email thread maps to one Matrix room
- Participants (To/CC/BCC) are represented as Matrix ghost users
- Attachments are uploaded to the homeserver media repo

## Quick start

You can run Matrimail in two supported ways. Pick one and follow it end-to-end.

### A) With Beeper Bridge Manager (recommended for Beeper users)

This is the easiest way if you're already using Beeper's bridge system.

**What you need first:**

- Beeper Bridge Manager installed and logged in: `bbctl login`
- Go 1.22+ and libolm on your system

**Step-by-step setup:**

1. **Get the code and run setup:**
   ```bash
   git clone https://github.com/Leicas/matrimail
   cd matrimail
   ./setup.sh
   ```
2. **What setup.sh does:**

   - Installs libolm if you're on macOS and don't have it
   - Builds the bridge binary (creates `./matrimail`)
   - Creates a `./data/` folder in your project directory
   - Asks you for a bridge name (just pick something like `my-email-bridge` - this is NOT your email address)
   - Uses bbctl to generate `./data/config.yaml` with Beeper's websocket settings

3. **Important: setup.sh does NOT configure your email accounts**
   The config file it creates connects to Beeper's infrastructure, but you'll add your actual email accounts later using bot commands.

4. **Start the bridge:**

   ```bash
   ./matrimail --config ./data/config.yaml
   ```

5. **Add your email account:**
   - Find the bot in your Matrix client (in Beeper, you can go to Settings -> Accounts -> Bridges (under Self-hosted bridges), youremailbridgename here -> Create a bot room)
   - Send it: `!matrimail login`
   - Follow the guided setup for your Email account

**Where things are stored:**

- **Config file:** `./data/config.yaml` (in your matrimail folder)
- **Database:** `./data/matrimail.db` (created when you first add an email account)
- **Logs:** `./logs/bridge.log` (created when the bridge starts)

**If something goes wrong:**

- Check the logs in your matrimail folder at `./logs/bridge.log`
- Make sure bbctl is logged in: `bbctl whoami`
- The bridge name you picked doesn't matter - it's just an identifier for bbctl

### B) Standalone (no Bridge Manager)

Prerequisites:

- Go 1.22+
- libolm
- A Matrix homeserver you control (standard mode) or Beeper hungryserv (websocket mode)

Steps:

```bash
git clone https://github.com/Leicas/matrimail
cd matrimail
make build
./matrimail --generate-example-config
# Edit config.yaml with your homeserver details
# If you are using a standard homeserver over HTTP appservice:
#   ./matrimail --generate-registration
#   Add registration.yaml to your homeserver and set appservice -> address/hostname/port accordingly.
./matrimail
```

### C) Docker Compose (standalone)

Use this if you want to run the compiled image with SQLite (default). This flow does not use Bridge Manager.

```bash
git clone https://github.com/Leicas/matrimail
cd matrimail
make build
./matrimail --generate-example-config
# Edit config.yaml, especially database.uri for container path (see below)

docker-compose up -d
```

Notes:

- Config path: docker-compose mounts ./config.yaml into /opt/matrimail/config.yaml and sets MAUTRIX_CONFIG_PATH accordingly.
- Registration: NOT needed when using websocket mode. Only generate and mount registration.yaml if you run in HTTP appservice mode on a standard homeserver.
- Data path: The container writes to /home/nonroot/app/data, a volume owned by the nonroot user. No need to create ./data on the host.
- Postgres: optional. The compose file focuses on SQLite. Add Postgres yourself if desired.

## Configuration

Initial setup (summary):

1. Generate a config (with setup.sh via Bridge Manager or with `--generate-example-config`).
2. Edit homeserver settings in config.yaml.
3. If using a standard homeserver over HTTP appservice, generate and register registration.yaml with your homeserver.
4. Start the bridge.

## Using the Bridge

Once your bridge is running, send these commands in a DM to the bot:

### Getting Started

- `!matrimail login` — Connect your email account (walks you through the setup)
- `!matrimail login email:you@gmail.com password:yourapppassword` — Quick setup if you know your details
- `!matrimail help` — Show available commands and help

### Managing Your Accounts

- `!matrimail list` — See all your connected email accounts, status, and monitored folders
- `!matrimail logout` — Disconnect all email accounts
- `!matrimail logout you@gmail.com` — Disconnect just one specific account
- `!matrimail status` — Check if everything's working (connection health, monitoring status)
- `!matrimail config folders` — Change which folders to monitor (requires logout/login)

### Troubleshooting

- `!matrimail sync` — Force check for new emails on all accounts
- `!matrimail sync you@gmail.com` — Force sync just one account
- `!matrimail reconnect` — Fix connection issues for all accounts
- `!matrimail reconnect you@gmail.com` — Reconnect just one account
- `!matrimail ping` — Basic bridge health check

### Advanced

- `!matrimail passphrase` — Manage the password that encrypts your email credentials
- `!matrimail passphrase show-location` — Show where the encryption passphrase is stored (never prints it)

  `generate` and `set` were removed: both wrote a new passphrase without
  re-encrypting the credentials it protects, so the next restart could decrypt
  nothing. Set `MATRIMAIL_PASSPHRASE` in the service environment instead.
- `!matrimail nuke confirm` — **DANGER:** Delete all bridge data and reset (requires confirmation)

## Sending email

Once you've connected an account, you can send email from Matrix:

- **Reply to a thread**: just type in the thread's Matrix room. Your message becomes an email reply with proper threading headers (`In-Reply-To` + `References`).
- **Compose a new thread (bot command)**: send `!matrimail compose to:alice@example.com subject:"Project update"` in the bot DM. matrimail creates a new room; your first message in that room becomes the new email.
- **Compose a new thread (Matrix-native)**: in clients that surface bridge "start chat" (Element, Beeper), enter `alice@example.com` to start a thread. Same first-message-becomes-email behavior.

### Outbound transport

- **Gmail accounts**: outbound goes through the Gmail API (`Users.Messages.Send`) when you've connected via OAuth. Server-assigned `Message-ID` is used for dedup, eliminating the Message-ID-rewrite false-positives that affect SMTP submissions.
- **Other providers**: SMTP submission on port 587 with STARTTLS. Same app password as your IMAP login.
- **Sent-folder dedup**: matrimail tracks every Message-ID it sends and skips the inbound IMAP echo so you don't see your own messages twice.

### What's NOT bridged outbound (v1)

- Matrix message edits — email can't be unsent. Edits show a status icon, no email is sent.
- Matrix redactions — email can't be unsent. Redactions only affect the local Matrix copy.
- Reactions — emoji reactions don't bridge to email.

Deployment-specific paths:

- Docker Compose: place config.yaml in the project root; compose mounts it read-only into the container. registration.yaml is only needed for HTTP appservice mode.
- Bridge Manager: setup.sh writes ./data/config.yaml; run the binary with `--config ./data/config.yaml`.

### Database configuration (IMPORTANT)

The bridge needs a persistent database path. If you use SQLite (default), set the database URI to the data folder used by your deployment mode.

Docker Compose (distroless nonroot):

```yaml
# config.yaml (bridgev2)
database:
  type: sqlite3
  uri: "file:/home/nonroot/app/data/matrimail.db?_fk=1"
```

Host/manual (runs in repo working directory):

```yaml
# config.yaml (bridgev2)
database:
  type: sqlite3
  uri: "file:./data/matrimail.db?_fk=1"
```

Notes:

- The container data directory is /home/nonroot/app/data (a named volume). Do not point the URI to /opt/matrimail.
- The host data directory is ./data. Ensure it exists and is writable.
- For Postgres, set type: postgres and provide a proper DSN instead of sqlite3.

## Security and runtime notes

- Never build or run with nocrypto. libolm is required for proper E2EE support.
- The Docker image runs as a non-root user; data directory permissions are handled by the image and compose volume mapping.
- **Encryption passphrase:** Set `MATRIMAIL_PASSPHRASE` environment variable in production, or the bridge will auto-generate one and store it in a file.

## Email provider support

The bridge automatically detects and configures settings for these providers:

### Major Providers (App Password Required)

- **Gmail:** gmail.com, googlemail.com
- **Yahoo:** yahoo.com, yahoo.co.uk, yahoo.fr, yahoo.de
- **Microsoft:** outlook.com, hotmail.com, live.com, msn.com, office365.com
- **Apple iCloud:** icloud.com, me.com, mac.com

### Other Supported Providers

- **FastMail:** fastmail.com

### Unknown Providers

For email providers not in the list above, the bridge will:

- Try to auto-detect your IMAP settings (usually works)
- Show you exactly what server it's trying to connect to
- Give helpful troubleshooting tips if the connection fails
- Suggest common IMAP server patterns your provider might use

**About App Passwords:**
Most major email providers require you to generate a special "App Password" instead of using your regular login password. The bridge will guide you through this during setup and provide links to the right settings pages.

**The bridge tells you what's happening:**

- When it recognizes your email provider and uses optimized settings
- When it's trying to auto-detect settings for an unknown provider
- Exactly which server and port it's attempting to connect to

## How it works

1. **Threads map to rooms.** One email thread = one Matrix room.
2. **Real-time delivery via IMAP IDLE.** No polling delays.
3. **Participants come from To/CC/BCC.** Each appears as a ghost user.
4. **Threading uses Message-ID, References, and In-Reply-To.** Standard RFC 5322.
5. **Attachments are uploaded to Matrix media.** PDFs, images, documents.
6. **Participant changes are posted as notices.** CC changes, new recipients.

**Sent folder behavior:** matrimail monitors your Sent folder via IMAP IDLE to capture replies you send from other email clients (Gmail web, phone app, etc.). When matrimail itself sends an outbound email, it records the Message-ID in a dedup table and short-circuits the inbound IMAP echo so you only see one copy in Matrix.

## Folder Selection

When you log in, the bridge shows you all available folders and labels:

```
✅ Connected to Gmail successfully!
📁 Choose which folders to monitor:
  1. 📥 INBOX [folder]
  2. 📤 Sent [folder]
  3. 🏷️ BridgeToBeeper [label]
  4. 🏷️ Important [label]
```

- **Type folder numbers** separated by commas (e.g., `1,3`)
- **Type `default`** to just monitor INBOX
- **Type `cancel`** to abort

After confirming your selection, only emails in those folders will appear in Matrix.

**To change folders later:** Use `!matrimail config folders` - this will guide you to logout and login again to select new folders.

## License

AGPL-3.0-or-later. See LICENSE.

Portions derived from mautrix-whatsapp (AGPL-3.0-or-later) by Tulir Asokan and contributors.
