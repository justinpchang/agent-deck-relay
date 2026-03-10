#!/usr/bin/env bash
# install.sh — one-shot setup for agent-deck-relay.
# Run from the repo root: ./install.sh
# Re-running is safe — it updates everything in place.

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'

step() { echo -e "\n${BOLD}${BLUE}▸${NC} $*"; }
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}!${NC} $*"; }
die()  { echo -e "\n${RED}✗ ERROR:${NC} $*\n" >&2; exit 1; }

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"

# ── Prerequisites ─────────────────────────────────────────────────────────────
step "Checking prerequisites"

need() {
  local cmd="$1" hint="$2"
  if ! command -v "$cmd" &>/dev/null; then
    die "'$cmd' not found. $hint"
  fi
  ok "$cmd"
}

need go         "Install Go ≥1.22 from https://go.dev/dl/"
need tailscale  "Install Tailscale from https://tailscale.com, then run: sudo tailscale up"
need agent-deck "Install agent-deck from https://github.com/asheshgoplani/agent-deck"

# Require Go 1.22+
GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
GO_MINOR=$(echo "$GO_VER" | cut -d. -f2)
[[ $(echo "$GO_VER" | cut -d. -f1) -ge 1 && "$GO_MINOR" -ge 22 ]] \
  || die "Go 1.22+ required (found $GO_VER). Update at https://go.dev/dl/"

# Tailscale must be connected
tailscale status &>/dev/null \
  || die "Tailscale is not connected. Run: sudo tailscale up"

# ── Directories ───────────────────────────────────────────────────────────────
INSTALL_DIR="$HOME/.local/bin"
AD_DIR="$HOME/.agent-deck"
CERT_DIR="$AD_DIR/certs"
HOOKS_DIR="$AD_DIR/hooks"
WEB_DIR="$AD_DIR/web"

mkdir -p "$INSTALL_DIR" "$CERT_DIR" "$HOOKS_DIR" "$WEB_DIR"

# ── Build ─────────────────────────────────────────────────────────────────────
step "Building agent-deck-relay"
go build -o "$INSTALL_DIR/agent-deck-relay" "$REPO_DIR/cmd/agent-deck-relay"
ok "Binary → $INSTALL_DIR/agent-deck-relay"

# Warn if ~/.local/bin isn't in PATH yet (launchd will have it regardless)
if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
  warn "$INSTALL_DIR is not in your current PATH"
  warn "Run this or add it to your shell config:"
  warn "  export PATH=\"\$HOME/.local/bin:\$PATH\""
fi

# ── Tailscale hostname & cert ─────────────────────────────────────────────────
step "Getting Tailscale hostname"
TS_HOST=$(tailscale status --json \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['Self']['DNSName'].rstrip('.'))" \
  2>/dev/null) \
  || die "Could not read Tailscale hostname. Is Tailscale running? (tailscale up)"
ok "Hostname: $TS_HOST"

step "Fetching Tailscale HTTPS certificate"
CERT_FILE="$CERT_DIR/$TS_HOST.crt"
KEY_FILE="$CERT_DIR/$TS_HOST.key"

# tailscale cert writes <hostname>.crt / .key to the current dir
(cd "$CERT_DIR" && tailscale cert "$TS_HOST")

[[ -f "$CERT_FILE" ]] || die "Expected cert at $CERT_FILE — did tailscale cert succeed?"
[[ -f "$KEY_FILE"  ]] || die "Expected key at $KEY_FILE"
chmod 600 "$KEY_FILE"
ok "Certificate → $CERT_FILE"
ok "Key         → $KEY_FILE"

# ── Token ─────────────────────────────────────────────────────────────────────
step "Setting up relay token"
TOKEN_FILE="$AD_DIR/token"

if [[ -f "$TOKEN_FILE" ]]; then
  RELAY_TOKEN=$(cat "$TOKEN_FILE")
  ok "Using existing token from $TOKEN_FILE"
else
  RELAY_TOKEN=$(openssl rand -hex 16)
  printf '%s' "$RELAY_TOKEN" > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  ok "Generated new token → $TOKEN_FILE"
fi

# ── Shell environment ─────────────────────────────────────────────────────────
step "Configuring shell environment"

if   [[ "$SHELL" == *zsh*  ]]; then SHELL_RC="$HOME/.zshrc"
elif [[ "$SHELL" == *bash* ]]; then SHELL_RC="$HOME/.bashrc"
else                                SHELL_RC="$HOME/.profile"
fi

set_export() {
  local var="$1" val="$2"
  if grep -qE "^export $var=" "$SHELL_RC" 2>/dev/null; then
    sed -i '' "s|^export $var=.*|export $var=\"$val\"|" "$SHELL_RC"
    ok "Updated $var in $SHELL_RC"
  else
    echo "export $var=\"$val\"" >> "$SHELL_RC"
    ok "Added $var to $SHELL_RC"
  fi
}

# ~/.local/bin in PATH
if ! grep -q 'local/bin' "$SHELL_RC" 2>/dev/null; then
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$SHELL_RC"
  ok "Added ~/.local/bin to PATH in $SHELL_RC"
fi

set_export "RELAY_TOKEN" "$RELAY_TOKEN"
set_export "RELAY_URL"   "http://127.0.0.1:8421"

# ── PWA web files ─────────────────────────────────────────────────────────────
step "Installing PWA files"
cp -r "$REPO_DIR/web/." "$WEB_DIR/"
ok "PWA → $WEB_DIR"

# ── Claude Stop hook ──────────────────────────────────────────────────────────
step "Installing Claude Stop hook"
cp "$REPO_DIR/hooks/mobile-notify.sh" "$HOOKS_DIR/mobile-notify.sh"
chmod +x "$HOOKS_DIR/mobile-notify.sh"
ok "Hook script → $HOOKS_DIR/mobile-notify.sh"

CLAUDE_SETTINGS="$HOME/.claude/settings.json"
HOOK_STATUS=$(python3 - "$HOOKS_DIR/mobile-notify.sh" "$CLAUDE_SETTINGS" <<'PYEOF'
import json, os, sys

hook_path  = sys.argv[1]
cfg_path   = sys.argv[2]

new_hook = {"type": "command", "command": hook_path, "async": True}

try:
    with open(cfg_path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
except json.JSONDecodeError:
    print("json_error"); sys.exit(0)

stop = cfg.setdefault("hooks", {}).setdefault("Stop", [])

for group in stop:
    for h in group.get("hooks", []):
        if "mobile-notify" in h.get("command", ""):
            print("already"); sys.exit(0)

if stop:
    stop[0].setdefault("hooks", []).append(new_hook)
else:
    stop.append({"hooks": [new_hook]})

os.makedirs(os.path.dirname(cfg_path), exist_ok=True)
with open(cfg_path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")

print("installed")
PYEOF
)

case "$HOOK_STATUS" in
  installed)   ok "Hook registered in $CLAUDE_SETTINGS" ;;
  already)     ok "Hook already in $CLAUDE_SETTINGS" ;;
  json_error)  warn "Could not parse $CLAUDE_SETTINGS — add the hook manually (see README)" ;;
esac

# ── launchd service ───────────────────────────────────────────────────────────
step "Installing launchd service (auto-starts on login)"
PLIST="$HOME/Library/LaunchAgents/com.agent-deck.relay.plist"
mkdir -p "$HOME/Library/LaunchAgents"

# Collect a sane PATH for launchd (it doesn't inherit your shell PATH)
LAUNCHD_PATH="$INSTALL_DIR:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.agent-deck.relay</string>
  <key>ProgramArguments</key>
  <array>
    <string>$INSTALL_DIR/agent-deck-relay</string>
    <string>--listen</string><string>$TS_HOST:8421</string>
    <string>--token</string><string>$RELAY_TOKEN</string>
    <string>--tls-cert</string><string>$CERT_FILE</string>
    <string>--tls-key</string><string>$KEY_FILE</string>
    <string>--web</string><string>$WEB_DIR</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>$HOME</string>
    <key>PATH</key><string>$LAUNCHD_PATH</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/tmp/agent-deck-relay.log</string>
  <key>StandardErrorPath</key><string>/tmp/agent-deck-relay.err</string>
</dict></plist>
PLIST_EOF

# Unload if already running, then reload
launchctl unload "$PLIST" 2>/dev/null || true
launchctl load   "$PLIST"

# Give it a moment to start
sleep 1

if launchctl list | grep -q "com.agent-deck.relay"; then
  ok "Service running (auto-starts on login)"
else
  warn "Service may not have started. Check:"
  warn "  tail -f /tmp/agent-deck-relay.err"
fi

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BOLD}${GREEN}  Setup complete!${NC}"
echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""
echo -e "  On your iPhone, open ${BOLD}Safari${NC} and go to:"
echo ""
echo -e "    ${BOLD}${BLUE}https://$TS_HOST:8421${NC}"
echo ""
echo -e "  Then:"
echo -e "    1. Enter your token when prompted: ${BOLD}$RELAY_TOKEN${NC}"
echo -e "    2. Tap ${BOLD}Share ↑${NC} → ${BOLD}Add to Home Screen${NC} → ${BOLD}Add${NC}"
echo -e "    3. Open from your Home Screen (required for push)"
echo -e "    4. Tap ${BOLD}🔔 Alerts${NC} and allow notifications"
echo ""
echo -e "  Logs:   tail -f /tmp/agent-deck-relay.log"
echo -e "  Health: curl https://$TS_HOST:8421/health"
echo ""
echo -e "  To renew your TLS cert (expires every ~90 days):"
echo -e "    cd $CERT_DIR && tailscale cert $TS_HOST"
echo -e "    launchctl kickstart -k gui/\$(id -u)/com.agent-deck.relay"
echo ""
