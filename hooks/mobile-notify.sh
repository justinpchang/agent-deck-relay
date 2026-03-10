#!/usr/bin/env bash

set -euo pipefail

RELAY_URL="${RELAY_URL:-http://127.0.0.1:8421}"
RELAY_TOKEN="${RELAY_TOKEN:-}"
LOG_FILE="${HOME}/.agent-deck/hook.log"

log() { echo "[$(date '+%H:%M:%S')] $*" >> "$LOG_FILE" 2>/dev/null || true; }

# ── Identify the calling session ────────────────────────────────────────────
# agent-deck sets AGENT_DECK_SESSION_ID in the environment of each tmux session.
SESSION_ID="${AGENT_DECK_SESSION_ID:-}"

if [[ -z "$SESSION_ID" ]]; then
  # Fallback: ask agent-deck to detect the current session from tmux context.
  SESSION_ID=$(agent-deck session current -q 2>/dev/null || echo "")
fi

if [[ -z "$SESSION_ID" ]]; then
  log "WARN: could not determine session ID — hook fired but no session context available"
  # Still exit 0 so Claude is unaffected
  exit 0
fi

log "hook fired for session: $SESSION_ID"

# ── Grab last output ─────────────────────────────────────────────────────────
# Cap at 400 chars. agent-deck session output may return JSONL; we take the
# raw text and let the relay/notification truncate further if needed.
OUTPUT=$(agent-deck session output "$SESSION_ID" 2>/dev/null | tail -c 400 || echo "")

# ── Build JSON payload ───────────────────────────────────────────────────────
# Use jq if available (safer escaping); fall back to printf.
if command -v jq &>/dev/null; then
  PAYLOAD=$(jq -n \
    --arg session "$SESSION_ID" \
    --arg summary "$OUTPUT" \
    '{"session":$session,"summary":$summary}')
else
  # Escape double quotes and backslashes manually
  SESSION_SAFE="${SESSION_ID//\\/\\\\}"; SESSION_SAFE="${SESSION_SAFE//\"/\\\"}"
  OUTPUT_SAFE="${OUTPUT//\\/\\\\}";      OUTPUT_SAFE="${OUTPUT_SAFE//\"/\\\"}"
  # Replace newlines with \n
  OUTPUT_SAFE="${OUTPUT_SAFE//$'\n'/\\n}"
  PAYLOAD="{\"session\":\"${SESSION_SAFE}\",\"summary\":\"${OUTPUT_SAFE}\"}"
fi

# ── POST to relay ────────────────────────────────────────────────────────────
# -m 3: give up after 3 seconds so we never stall Claude
# -s:   silent (no progress bar)
# -f:   fail on HTTP errors (4xx/5xx)
# stderr goes to log, not to Claude's stdout
HTTP_ARGS=(-s -m 3 -w '%{http_code}'
           -X POST "$RELAY_URL/api/hook"
           -H "Content-Type: application/json"
           -d "$PAYLOAD")

if [[ -n "$RELAY_TOKEN" ]]; then
  HTTP_ARGS+=(-H "Authorization: Bearer $RELAY_TOKEN")
fi

STATUS=$(curl "${HTTP_ARGS[@]}" 2>>"$LOG_FILE" || echo "000")

if [[ "$STATUS" == "204" || "$STATUS" == "200" ]]; then
  log "hook delivered OK (HTTP $STATUS)"
elif [[ "$STATUS" == "000" ]]; then
  log "WARN: relay unreachable (curl failed) — is agent-deck-relay running at $RELAY_URL?"
else
  log "WARN: relay returned HTTP $STATUS"
fi

# Always exit 0 — hooks must not interfere with Claude's own exit code.
exit 0
