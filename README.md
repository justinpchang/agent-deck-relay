# agent-deck-relay

Monitor and reply to Claude Code sessions from your iPhone.

**The problem:** You start several Claude sessions across git worktrees and walk away. You have no idea when they finish, hit a prompt, or need input.

**What this adds:**
- Push notification the moment any session needs your input
- Mobile PWA showing all sessions with live status
- Tap a notification → read the conversation → type a reply → Claude receives it

---

## Requirements

- **[agent-deck](https://github.com/asheshgoplani/agent-deck)** — installed and working
- **[Tailscale](https://tailscale.com)** — running on your Mac and iPhone, signed into the same account
- **Go 1.22+** — `brew install go` or [go.dev/dl](https://go.dev/dl/)
- **iPhone** — iOS 16.4+, Safari

---

## Install

```bash
git clone https://github.com/you/agent-deck-relay
cd agent-deck-relay
./install.sh
```

The script handles everything:
- Builds and installs the binary to `~/.local/bin/`
- Fetches a Tailscale HTTPS certificate (required for push on iOS)
- Generates a secret relay token
- Installs the Claude `Stop` hook
- Creates a launchd service that auto-starts on login
- Prints the URL and token to use on your iPhone

Re-running is safe — it updates everything in place.

---

## Add to iPhone

At the end of `./install.sh`, you'll see something like:

```
  On your iPhone, open Safari and go to:

    https://mymac.tail1234.ts.net:8421

  Then:
    1. Enter your token when prompted
    2. Tap Share ↑ → Add to Home Screen → Add
    3. Open from your Home Screen  ← required for push to work
    4. Tap 🔔 Alerts and allow notifications
```

Done. The next time Claude finishes a turn, your phone gets a notification.

---

## Usage

| Action | What happens |
|---|---|
| Notification arrives | A session went `waiting` — Claude needs input |
| Tap notification | PWA opens to that session |
| Output pane | Full conversation transcript with tool call diffs |
| Type + Send | Calls `agent-deck session send` on your Mac |
| ⏹ button (while running) | Sends interrupt to Claude |
| Pull to refresh / ↻ | Re-fetches latest output |

---

## Maintenance

**Renew TLS cert** (expires every ~90 days):
```bash
cd ~/.agent-deck/certs && tailscale cert <your-hostname>.ts.net
launchctl kickstart -k gui/$(id -u)/com.agent-deck.relay
```

**Check relay health:**
```bash
curl -H "Authorization: Bearer $RELAY_TOKEN" https://<hostname>:8421/debug/status | python3 -m json.tool
```

**View logs:**
```bash
tail -f /tmp/agent-deck-relay.log
```

**Fire a test push:**
```bash
curl -H "Authorization: Bearer $RELAY_TOKEN" "https://<hostname>:8421/debug/push-test"
```

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| "browser does not support service workers" | You're on HTTP, not HTTPS — make sure you're using the `https://` URL |
| No notifications | PWA must be opened from Home Screen, not a browser tab |
| Notifications blocked on iOS | Remove from Home Screen, re-add, try again |
| Sessions not loading | Wrong token in PWA; tap the lock icon or clear site data and re-enter |
| Hook not firing | Check `tail -f ~/.agent-deck/hook.log` |
| Relay unreachable in hook.log | `launchctl list \| grep agent-deck` — is the service running? |
| `agent-deck list` errors in relay log | `agent-deck` not in launchd PATH; check `/tmp/agent-deck-relay.err` |
| Push sent but not received | Check `/debug/status` — subscription may have expired; tap 🔔 again |

---

## How it works

```
Claude Stop hook
      │ POST /api/hook (loopback only)
      ▼
agent-deck-relay  ──── polls `agent-deck list --json` every 4s
      │                detects status changes
      │ Web Push (VAPID over HTTPS)
      ▼
iPhone (Safari PWA, installed to Home Screen)
      │ taps notification
      ▼
PWA detail view  ──── POST /api/sessions/{id}/send
      │                shells out to `agent-deck session send`
      ▼
Claude receives your reply
```

The relay is a **sidecar** — it calls the existing `agent-deck` CLI and never touches its internals. The only external dependency is [`webpush-go`](https://github.com/SherClockHolmes/webpush-go) for VAPID key generation and push delivery.

**Why HTTPS?** Web Push requires a secure context. Tailscale's built-in cert authority issues trusted certificates for your `*.ts.net` hostname, so there's nothing to configure in iOS.

**Why SSE and not WebSockets?** SSE is unidirectional (server → client), reconnects automatically in the browser, and is simpler to implement. Writes go over normal POST requests.

---

## File layout

```
cmd/agent-deck-relay/main.go   Go HTTP server (the relay daemon)
web/                           Mobile PWA (session list + reply drawer)
  index.html
  sw.js                        Service worker (push + caching)
  manifest.json
  icons/
hooks/mobile-notify.sh         Claude Stop hook (installed to ~/.agent-deck/hooks/)
install.sh                     One-shot setup script
```

State is persisted in `~/.agent-deck/`:
```
relay-state.json   VAPID keys + push subscriptions
token              Relay bearer token (0600)
certs/             Tailscale TLS certificate + key
web/               PWA files served by the relay
hooks/             Claude hook scripts
hook.log           Hook script log
```
