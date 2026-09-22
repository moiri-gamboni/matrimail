# Gmail OAuth setup for matrimail

matrimail uses Google's standard **Authorization Code + PKCE + loopback redirect** flow (RFC 8252 Native Apps) to authorize Gmail accounts without app passwords. Setup has two parts:

1. **Once per bridge**: create a Google Cloud project + OAuth client and paste the credentials into `config.yaml` (this document).
2. **Once per email account**: log in via `!matrimail login` in your Matrix client. The bridge spins up a transient HTTP server on `127.0.0.1:RANDOMPORT`, hands you a Google authorization URL, and detects the redirect automatically when you finish.

## Why you have to do step 1 (BYO Cloud project)

The `https://mail.google.com/` scope (used in `full` mode for IMAP/SMTP XOAUTH2) is a **restricted** scope under Google's policy. To ship a verified OAuth client that all matrimail users could share, the project would need to pass annual CASA security assessments — out of reach for a self-hosted FOSS bridge. Even the default `gmail.modify` scope (a "sensitive" scope) requires a verification process that's hard to maintain on shared infrastructure.

Practical answer: you create your own Google Cloud project and matrimail uses it on your behalf. This is exactly what `isync`, `mbsync`, `EmailEngine`, and similar self-hosted email tools require.

## Step 1: create a Google Cloud project and OAuth client

### 1.1 Create the project

1. Go to <https://console.cloud.google.com/projectcreate>.
2. Project name: anything (e.g. `matrimail`).
3. Wait for the project to provision, then make sure it's selected at the top of the console.

### 1.2 Enable the Gmail API

1. Navigate to **APIs & Services → Library**.
2. Search for "Gmail API".
3. Click **Enable**.

### 1.3 Configure the OAuth consent screen

1. **APIs & Services → OAuth consent screen**.
2. User Type. Pick **Internal** if this Google account belongs to a Google Workspace organisation and you are only bridging addresses in that organisation. Otherwise **Internal** is not offered and you must pick **External**. The choice has consequences that are awkward to change later:

   | | Internal | External |
   |---|---|---|
   | Who can authorise | Only accounts in your Workspace organisation | Any Google account you list as a test user |
   | Verification by Google | Not required | Not required while in Testing, required to leave it |
   | Refresh token lifetime | No fixed expiry, until revoked | **Expires after 7 days while the app is in Testing**, so you re-authorise weekly |
   | Restricted scopes (`full` mode) | Available, though an admin may need to trust the app | Require verification and a third-party security assessment |

   Click **Create**.
3. App information:
   - App name: `matrimail` (or any name you'll recognize).
   - User support email: your email.
   - Developer contact: your email.
   - Skip the optional logo / domains fields.
4. Click **Save and continue**.
5. **Scopes** screen → **Add or remove scopes**:
   - For the **default `modify` mode** (recommended): add `https://www.googleapis.com/auth/gmail.modify` and `https://www.googleapis.com/auth/gmail.send`. Both are "sensitive" but not "restricted".
   - For **`full` mode** (advanced; only if you need IMAP semantics): add `https://mail.google.com/` instead. This is "restricted" — see the trade-offs section below.
6. **Test users** screen → **Add users**: add every Gmail address you intend to bridge (up to 100). Without this, Google will block authorization with "Access blocked: this app's request is invalid". External apps only; an Internal app has no test-user list because everyone in the organisation is already allowed.
7. **Save and continue** → **Back to dashboard**.

⚠️ Leave the publishing status as **Testing**. See the trade-offs section for what this means for your refresh-token lifetime.

### 1.4 Create the OAuth client

1. **APIs & Services → Credentials**.
2. **Create Credentials → OAuth client ID**.
3. Application type: **Desktop app**.
4. Name: anything (e.g. `matrimail-desktop`).
5. Click **Create**.
6. Copy the **Client ID** and **Client secret** from the popup.

(Desktop client type automatically authorizes loopback redirects to `http://127.0.0.1` / `http://localhost` on any port — no need to manually register a redirect URI.)

### 1.5 Paste credentials into `config.yaml`

```yaml
gmail_oauth:
    client_id: "YOUR_CLIENT_ID.apps.googleusercontent.com"
    client_secret: "GOCSPX-..."
    # Defaults are fine; uncomment to customize:
    # listener_address: "127.0.0.1:0"
    # default_scope_mode: "modify"
    # callback_timeout_seconds: 600
```

Restart matrimail.

## Step 2: log in to a Gmail account

Once `gmail_oauth.client_id` and `client_secret` are set, the bridge advertises the OAuth flow as a login option.

### 2.1 Bridge running on the same machine as your browser (typical)

1. In your Matrix client, send `!matrimail login` to the bridge bot.
2. Pick the **Gmail (OAuth, recommended)** flow.
3. Enter your Gmail address. Leave the scope-mode field blank (defaults to `modify`) or type `full` if you need IMAP.
4. The bridge replies with an authorization URL.
5. Open the URL — anywhere; it doesn't have to be on the bridge host as long as it can reach `http://127.0.0.1:RANDOMPORT` on the bridge host.
6. Sign in to Google, click **Continue** through the unverified-app warning ("This app isn't verified" → **Advanced** → **Go to matrimail (unsafe)**), and **Allow**.
7. The browser tab shows "Matrimail authorized — you may now close this tab".
8. The bridge continues: confirms identity, lists folders/labels, asks you to pick which to monitor, done.

### 2.2 Bridge running on a remote (headless) server

Loopback redirects require the browser to reach `http://127.0.0.1:PORT` on the **bridge host**. From a workstation, set up an SSH local port-forward **before** opening the URL:

```bash
ssh -L 8888:127.0.0.1:8888 user@bridge-host
```

…and configure matrimail to bind the listener to that port:

```yaml
gmail_oauth:
    listener_address: "127.0.0.1:8888"
```

Then `!matrimail login` as in 2.1; when the auth URL says `http://127.0.0.1:8888/callback`, your workstation's browser hits the SSH-tunneled port and the bridge sees the callback.

### 2.3 True headless (no browser, no SSH)

> **This puts a long-lived full-mailbox credential into your chat history. Use 2.2 instead if you possibly can.**
>
> A refresh token lasts until it is revoked for an Internal app, and a week even for an External app in Testing, and for as long as it lives it can mint access tokens to the whole mailbox. Typing one into a Matrix room writes it to the homeserver's event store, to every device that syncs the room, and to any backup of either. Redacting the message afterwards does not reliably remove it from all of those copies, and on a homeserver you do not run you cannot verify that it did.
>
> If you have already done this, treat the token as compromised: run `!matrimail oauth revoke your@email.com` to sever it at Google, then authorise again through 2.2.

If even SSH port-forward isn't available, the **paste-token escape hatch** exists:

1. On a machine with a browser, run any OAuth tool that supports authorization-code + PKCE against your Cloud project's `client_id` (e.g. `oauth2l`, `mutt_oauth2.py`, `oauth-helper`). Use `https://www.googleapis.com/auth/gmail.modify` + `https://www.googleapis.com/auth/gmail.send` (or `https://mail.google.com/` for full mode).
2. Save the resulting refresh token.
3. In your Matrix client: `!matrimail oauth paste-token your@email.com 1//0e...`
4. matrimail validates the token against Google, confirms the authorized account matches the email you specified, and stores it.

You can revoke later via `!matrimail oauth revoke your@email.com` (severs at Google + flags account locally) or `!matrimail logout your@email.com` (full delete).

## Trade-offs: `modify` vs `full` mode

| | `modify` (default) | `full` (advanced) |
|---|---|---|
| **Scope** | `gmail.modify` + `gmail.send` | `mail.google.com` |
| **Scope class** | Sensitive | Restricted |
| **Inbound transport** | Gmail API `users.history.list` polling (~30s latency) | IMAP IDLE (push, ~immediate) |
| **Outbound transport** | Gmail API `Users.Messages.Send` | IMAP `APPEND` to Sent + SMTP submission via XOAUTH2 |
| **Verification cost** | Google verification (free, manual) | Google verification + annual CASA Tier 2 ($500–$4,500/yr, recurring) |
| **Refresh token in Testing mode** | 7-day expiry | 7-day expiry |
| **Refresh token after publishing** | Long-lived (months / inactivity-revoked only) | Long-lived (same) — but publishing requires CASA |
| **Practical recommendation** | Use this. Default. | Only if your Workspace admin disabled the Gmail API or you need IMAP semantics for some other reason. Plan for weekly re-auth. |

## What happens when the refresh token dies

You'll see a DM from the matrimail bot in your management room: "Matrimail needs you to re-authorize Google for `your@email.com`". Run `!matrimail login` again — you don't need to repeat the GCP setup, just the per-account flow.

The account stays paused (not connected, no message loss for whatever Gmail buffers in the meantime) until you re-auth.

## Troubleshooting

- **"Access blocked: this app's request is invalid"** at the consent screen: you forgot to add the Gmail address as a test user in step 1.3.6.
- **`invalid_scope` from `/token`**: you're trying to use the device-code flow somehow. matrimail doesn't use it; if you're running a fork or pre-rework version, update to current `feat/matrimail-bidirectional`.
- **`redirect_uri_mismatch`**: you're using a Web-app OAuth client instead of Desktop. Re-do step 1.4 with Application type "Desktop app".
- **OAuth callback timed out**: 10-minute window from `!matrimail login` to authorization complete. Run the command again and don't tab away for too long.
- **Listener can't bind to 127.0.0.1:8888**: another process is using the port. Either stop it, set `listener_address: "127.0.0.1:0"` to use a random port, or pick a different fixed port.
- **"authorized account X does not match the email you entered Y"**: you signed in to the wrong Google account. Sign out of all Google accounts in the browser, run `!matrimail login` again, and pick the right one when Google's account chooser appears.

## See also

- Google's [OAuth 2.0 for iOS & Desktop Apps](https://developers.google.com/identity/protocols/oauth2/native-app)
- Google's [OAuth 2.0 Scopes for Google APIs](https://developers.google.com/identity/protocols/oauth2/scopes#gmail)
- [RFC 8252: OAuth 2.0 for Native Apps](https://www.rfc-editor.org/rfc/rfc8252.html)
- [Google CASA assessment overview](https://appdefensealliance.dev/casa)
