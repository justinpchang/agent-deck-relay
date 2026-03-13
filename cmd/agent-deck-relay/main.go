// agent-deck-relay: Mobile monitoring relay for agent-deck sessions.
//
// Intended flow:
//   1. User walks away from desk, sessions are running in agent-deck.
//   2. Claude finishes a turn → Stop hook fires → relay POSTs /api/hook.
//   3. Relay sends Web Push → iPhone shows notification with session name + summary.
//   4. User taps notification → PWA opens to that session's detail view.
//   5. User reads last output, types a reply, taps Send.
//   6. Relay calls `agent-deck session send` → Claude receives the message.
//
// Debug checklist if things break:
//   - Run with RELAY_DEBUG=1 for verbose per-request logging.
//   - GET /debug/status — JSON health dump (sessions, sub count, VAPID key).
//   - GET /debug/push-test?session=<id> — fires a test push to all subscribers.
//   - Tail ~/.agent-deck/relay.log (if started via launchd).
//   - Check relay-state.json exists and contains subscriptions + VAPID keys.
//   - Verify `agent-deck list --json` works from the same shell.
//   - On iOS: push only works when the PWA is installed to Home Screen.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// claudeProjectKey encodes a filesystem path to the directory name Claude Code uses
// under ~/.claude/projects/. Claude replaces every character that isn't alphanumeric
// or a hyphen with a hyphen — not just slashes. e.g. "/foo/.bar/baz" → "-foo--bar-baz".
var nonAlphanumDash = regexp.MustCompile(`[^a-zA-Z0-9-]`)

func claudeProjectKey(path string) string {
	return nonAlphanumDash.ReplaceAllString(path, "-")
}

// shortPath returns the last two meaningful path components, e.g.
// "/Users/justin/dev/owner" → "dev/owner". Mirrors the JS shortPath helper.
func shortPath(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return p
}

// sessionCreatedAt parses the Unix timestamp embedded in an agent-deck session ID.
// agent-deck IDs are formatted as "<hash>-<unix_seconds>", e.g. "05cc70f6-1773251148".
// Returns zero Time and false if the ID doesn't follow this format.
func sessionCreatedAt(id string) (time.Time, bool) {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || ts <= 0 {
		return time.Time{}, false
	}
	return time.Unix(ts, 0), true
}

// ── Config ────────────────────────────────────────────────────────────────────

var (
	listenAddr   = flag.String("listen", "127.0.0.1:8421", "address:port to listen on (use your Tailscale IP for phone access)")
	tokenFlag    = flag.String("token", os.Getenv("RELAY_TOKEN"), "bearer token required for all API calls (set via RELAY_TOKEN env var)")
	adProfile    = flag.String("profile", "", "agent-deck profile flag value (passed as -p to every CLI call)")
	pollInterval = flag.Duration("poll", 4*time.Second, "how often to run `agent-deck list --json`")
	vapidEmail   = flag.String("vapid-email", "mailto:you@example.com", "VAPID contact; shown to push services if they need to reach you")
	stateFile    = flag.String("state", os.Getenv("HOME")+"/.agent-deck/relay-state.json", "persisted VAPID keys + push subscriptions")
	webDir       = flag.String("web", "", "directory to serve the PWA from (defaults to ./web/ next to binary)")
	tlsCert      = flag.String("tls-cert", "", "path to TLS certificate file (enables HTTPS; required for push notifications on iOS)")
	tlsKey       = flag.String("tls-key", "", "path to TLS private key file")
	debugMode    = os.Getenv("RELAY_DEBUG") == "1"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// Session mirrors the JSON shape of `agent-deck list --json`.
// We only use a subset; extra fields are preserved by json.RawMessage round-trips
// if we ever need to forward them.
type Session struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // running | waiting | idle | error
	Tool   string `json:"tool"`
	Group  string `json:"group"`
	Path   string `json:"path"`
}

// Event is what we broadcast over SSE and include in push payloads.
type Event struct {
	Type      string    `json:"type"`                 // "status_change" | "hook" | "heartbeat"
	Session   *Session  `json:"session,omitempty"`
	OldStatus string    `json:"old_status,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Ts        time.Time `json:"ts"`
}

// PushSubscription is the W3C PushSubscription JSON shape sent from the browser.
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// HookPayload is what the Claude Stop hook script POSTs.
type HookPayload struct {
	Session string `json:"session"` // session id or title
	Summary string `json:"summary"` // last ~400 chars of output
}

// HistEntry is one recorded output snapshot, stored when a hook fires.
type HistEntry struct {
	Ts      time.Time `json:"ts"`
	Summary string    `json:"summary"`
}

// AugSession is Session with relay-tracked last_seen appended for the PWA.
type AugSession struct {
	Session
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// ── Persisted state ───────────────────────────────────────────────────────────

type State struct {
	VAPIDPublic  string             `json:"vapid_public"`
	VAPIDPrivate string             `json:"vapid_private"`
	Subs         []PushSubscription `json:"subscriptions"`
}

func loadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	return &s, nil
}

func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file then rename for atomic update.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// ── Relay ─────────────────────────────────────────────────────────────────────

type Relay struct {
	mu       sync.RWMutex
	sessions map[string]Session // keyed by session ID

	sseMu   sync.Mutex
	sseClients map[chan Event]struct{}

	// Push state
	state     *State
	stateFile string
	vapidPub  string
	vapidPriv string
	email     string

	// CLI
	profile string

	// Per-session output history (populated by hook events, capped at 20 per session)
	history map[string][]HistEntry

	// Diagnostics
	lastPollTime  time.Time
	lastPollError string
	pollCount     int64
	pushSent      int64
	pushFailed    int64
}

func NewRelay(state *State, stateFile, pub, priv, email, profile string) *Relay {
	return &Relay{
		sessions:   make(map[string]Session),
		sseClients: make(map[chan Event]struct{}),
		history:    make(map[string][]HistEntry),
		state:      state,
		stateFile:  stateFile,
		vapidPub:   pub,
		vapidPriv:  priv,
		email:      email,
		profile:    profile,
	}
}

// ── CLI helpers ───────────────────────────────────────────────────────────────

// adCmd builds an exec.Cmd for `agent-deck` with an optional -p profile flag.
func (r *Relay) adCmd(args ...string) *exec.Cmd {
	if r.profile != "" {
		args = append([]string{"-p", r.profile}, args...)
	}
	cmd := exec.Command("agent-deck", args...)
	debugLog("exec: agent-deck %s", strings.Join(args, " "))
	return cmd
}

// fetchSessions calls `agent-deck list --json` and returns the result.
// It is the single source of truth for session state.
func (r *Relay) fetchSessions() ([]Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := r.adCmd("list", "--json")
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("agent-deck list timed out after 8s")
		}
		// Include stderr in error for easier debugging
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("agent-deck list failed (exit %d): %s", exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("agent-deck list: %w", err)
	}

	// agent-deck prints a plain-text message instead of "[]" when there are no sessions.
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || strings.HasPrefix(trimmed, "No sessions") {
		return []Session{}, nil
	}

	var sessions []Session
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("parse sessions JSON: %w\nraw output: %s", err, truncate(string(out), 500))
	}
	return sessions, nil
}

// sessionByIDOrTitle looks up a session in the local cache by ID or title.
// Returns a zero Session and false if not found.
func (r *Relay) sessionByIDOrTitle(key string) (Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.sessions[key]; ok {
		return s, true
	}
	for _, s := range r.sessions {
		if s.Title == key {
			return s, true
		}
	}
	return Session{}, false
}

// ── Poller ────────────────────────────────────────────────────────────────────

func (r *Relay) poll(ctx context.Context) {
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPoll()
		}
	}
}

func (r *Relay) runPoll() {
	sessions, err := r.fetchSessions()

	r.mu.Lock()
	r.lastPollTime = time.Now()
	r.pollCount++
	if err != nil {
		r.lastPollError = err.Error()
		r.mu.Unlock()
		log.Printf("poll error: %v", err)
		return
	}
	r.lastPollError = ""

	// Collect changes before replacing the map
	type statusChange struct {
		s   Session
		old string
	}
	var changed []statusChange
	var removed []Session

	newMap := make(map[string]Session, len(sessions))
	for _, s := range sessions {
		newMap[s.ID] = s
		if prev, known := r.sessions[s.ID]; known && prev.Status != s.Status {
			changed = append(changed, statusChange{s, prev.Status})
		}
	}
	for id, prev := range r.sessions {
		if _, stillExists := newMap[id]; !stillExists {
			removed = append(removed, prev)
		}
	}
	r.sessions = newMap // replace entirely — purges deleted sessions
	r.mu.Unlock()

	for _, c := range changed {
		s := c.s
		r.broadcast(Event{Type: "status_change", Session: &s, OldStatus: c.old, Ts: time.Now()})
		if s.Status == "waiting" {
			go r.pushToAll(s, "")
		}
	}
	for _, s := range removed {
		s := s
		debugLog("session removed from cache: %s (%s)", s.ID, s.Title)
		r.broadcast(Event{Type: "removed", Session: &s, Ts: time.Now()})
	}
}

// ── SSE broadcast ─────────────────────────────────────────────────────────────

func (r *Relay) broadcast(ev Event) {
	r.sseMu.Lock()
	defer r.sseMu.Unlock()
	for ch := range r.sseClients {
		select {
		case ch <- ev:
		default:
			// Client is too slow; drop the event rather than block.
			debugLog("SSE client too slow, dropping event type=%s", ev.Type)
		}
	}
}

func (r *Relay) addSSEClient(ch chan Event) {
	r.sseMu.Lock()
	r.sseClients[ch] = struct{}{}
	r.sseMu.Unlock()
}

func (r *Relay) removeSSEClient(ch chan Event) {
	r.sseMu.Lock()
	delete(r.sseClients, ch)
	r.sseMu.Unlock()
}

// ── Web Push ──────────────────────────────────────────────────────────────────

// pushToAll sends a Web Push notification to every stored subscription.
// summary is optional; if empty, a generic message is used.
func (r *Relay) pushToAll(s Session, summary string) {
	icon := map[string]string{
		"running": "🟢", "waiting": "🟡", "idle": "⚪", "error": "🔴",
	}[s.Status]
	if icon == "" {
		icon = "•"
	}

	title := fmt.Sprintf("%s %s", icon, s.Title)
	body := summary
	if body == "" {
		body = "Waiting for your reply"
	}
	// Prepend short project path for multi-agent context, e.g. "dev/owner • ..."
	if s.Path != "" {
		body = shortPath(s.Path) + " • " + body
	}
	body = truncate(body, 200)

	payload, _ := json.Marshal(map[string]string{
		"title":     title,
		"body":      body,
		"sessionId": s.ID,
		"url":       fmt.Sprintf("/?session=%s", s.ID),
	})

	r.mu.RLock()
	subs := make([]PushSubscription, len(r.state.Subs))
	copy(subs, r.state.Subs)
	r.mu.RUnlock()

	if len(subs) == 0 {
		debugLog("pushToAll: no subscriptions registered — skipping (is the PWA installed to Home Screen?)")
		return
	}

	var expired []string
	for _, sub := range subs {
		if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
			log.Printf("push: skipping subscription without encryption keys (...%s) — re-open the PWA to re-register", last20(sub.Endpoint))
			expired = append(expired, sub.Endpoint)
			continue
		}
		resp, err := webpush.SendNotification(payload, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth},
		}, &webpush.Options{
			Subscriber:      r.email,
			VAPIDPublicKey:  r.vapidPub,
			VAPIDPrivateKey: r.vapidPriv,
			TTL:             300,
		})
		if err != nil {
			r.mu.Lock(); r.pushFailed++; r.mu.Unlock()
			log.Printf("push send error (endpoint ...%s): %v", last20(sub.Endpoint), err)
			continue
		}
		resp.Body.Close()

		switch resp.StatusCode {
		case 200, 201:
			r.mu.Lock(); r.pushSent++; r.mu.Unlock()
			debugLog("push OK → ...%s", last20(sub.Endpoint))
		case 403, 404, 410:
			// Subscription is gone or rejected; queue for removal
			log.Printf("push: subscription invalid/expired (%d) (...%s), removing", resp.StatusCode, last20(sub.Endpoint))
			expired = append(expired, sub.Endpoint)
		default:
			r.mu.Lock(); r.pushFailed++; r.mu.Unlock()
			log.Printf("push: unexpected status %d for ...%s", resp.StatusCode, last20(sub.Endpoint))
		}
	}

	if len(expired) > 0 {
		r.removeExpiredSubs(expired)
	}
}

func (r *Relay) removeExpiredSubs(endpoints []string) {
	set := make(map[string]bool, len(endpoints))
	for _, e := range endpoints {
		set[e] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.state.Subs[:0]
	for _, s := range r.state.Subs {
		if !set[s.Endpoint] {
			kept = append(kept, s)
		}
	}
	r.state.Subs = kept
	if err := r.state.save(r.stateFile); err != nil {
		log.Printf("save state after sub removal: %v", err)
	}
}

// ── HTTP middleware ───────────────────────────────────────────────────────────

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if debugMode {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware enforces bearer token auth on /api/ and /debug/ routes.
// /api/hook is exempt (it's loopback-only and the hook script may not always have a token).
// /health and static PWA files are always public so the phone can load the app.
// SSE connections can't set headers, so they may pass the token as ?token=<tok>.
func authMiddleware(tok string, next http.Handler) http.Handler {
	if tok == "" {
		return next // auth disabled
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		needsAuth := (strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/debug/")) &&
			p != "/api/hook"
		if !needsAuth {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		query := r.URL.Query().Get("token") // fallback for SSE (EventSource can't set headers)
		if auth != "Bearer "+tok && query != tok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="agent-deck-relay"`)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── API handlers ──────────────────────────────────────────────────────────────

// GET /api/sessions
func (r *Relay) handleSessions(w http.ResponseWriter, req *http.Request) {
	sessions, err := r.fetchSessions()
	if err != nil {
		log.Printf("handleSessions: %v", err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.mu.RLock()
	result := make([]AugSession, len(sessions))
	for i, s := range sessions {
		aug := AugSession{Session: s}
		if entries := r.history[s.ID]; len(entries) > 0 {
			t := entries[len(entries)-1].Ts
			aug.LastSeen = &t
		}
		result[i] = aug
	}
	r.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// GET /api/sessions/{id}/history — ordered list of past hook summaries
func (r *Relay) handleHistory(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	r.mu.RLock()
	entries := append([]HistEntry{}, r.history[id]...)
	r.mu.RUnlock()
	if entries == nil {
		entries = []HistEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// GET /api/sessions/{id}/output
func (r *Relay) handleOutput(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		jsonError(w, "missing session id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	cmd := r.adCmd("session", "output", id)
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)

	out, err := cmd.Output()
	if err != nil {
		log.Printf("handleOutput(%s): %v", id, err)
		jsonError(w, fmt.Sprintf("could not get output: %v", err), http.StatusInternalServerError)
		return
	}
	// agent-deck session output returns raw text (JSONL lines for Claude).
	// We return it as-is so the PWA can display it.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}

// POST /api/sessions/{id}/interrupt — sends Ctrl+C to interrupt a running Claude session.
func (r *Relay) handleInterrupt(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	// Send Ctrl+C (ASCII 0x03) via agent-deck session send.
	// This is best-effort — agent-deck may or may not pass the raw byte through.
	cmd := r.adCmd("session", "send", id, "\x03")
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("interrupt(%s): %v: %s", id, err, strings.TrimSpace(string(out)))
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/sessions/{id}/transcript — reads the Claude Code JSONL conversation file directly.
// Claude stores full conversation history at ~/.claude/projects/<path-encoded>/<session-id>.jsonl
// where the path encoding replaces every "/" with "-".
func (r *Relay) handleTranscript(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")

	s, ok := r.sessionByIDOrTitle(id)
	if !ok {
		// Refresh cache and retry
		if sessions, err := r.fetchSessions(); err == nil {
			r.mu.Lock()
			for _, sess := range sessions {
				r.sessions[sess.ID] = sess
			}
			r.mu.Unlock()
			s, ok = r.sessionByIDOrTitle(id)
		}
	}
	if !ok || s.Path == "" {
		http.Error(w, "session not found or missing path", http.StatusNotFound)
		return
	}

	home := os.Getenv("HOME")
	claudeDir := filepath.Join(home, ".claude", "projects", claudeProjectKey(s.Path))
	transcriptPath := filepath.Join(claudeDir, s.ID+".jsonl")

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		// The agent-deck session ID format (<hash>-<unix_ts>) doesn't match the UUID filenames
		// Claude Code uses for JSONL files. Scan the project dir and match by file birth time
		// (macOS APFS preserves creation time), falling back to most-recently-modified.
		entries, dirErr := os.ReadDir(claudeDir)
		if dirErr != nil {
			http.Error(w, fmt.Sprintf("claude project dir not found: %v", dirErr), http.StatusNotFound)
			return
		}
		createdAt, hasCreatedAt := sessionCreatedAt(s.ID)
		var bestPath string
		var bestDiff time.Duration = -1 // -1 = unset
		var latestPath string
		var latestMod time.Time
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if info.ModTime().After(latestMod) {
				latestMod = info.ModTime()
				latestPath = filepath.Join(claudeDir, e.Name())
			}
			if hasCreatedAt {
				if stat, ok := info.Sys().(*syscall.Stat_t); ok {
					birth := time.Unix(stat.Birthtimespec.Sec, int64(stat.Birthtimespec.Nsec))
					diff := birth.Sub(createdAt)
					if diff < 0 {
						diff = -diff
					}
					if bestDiff < 0 || diff < bestDiff {
						bestDiff = diff
						bestPath = filepath.Join(claudeDir, e.Name())
					}
				}
			}
		}
		// Use birth-time match if within 30s; otherwise fall back to most recently modified.
		const maxBirthDiff = 30 * time.Second
		pickedPath := latestPath
		if hasCreatedAt && bestPath != "" && bestDiff >= 0 && bestDiff <= maxBirthDiff {
			pickedPath = bestPath
			debugLog("transcript: matched %s by birth time (diff=%v) for session %s", pickedPath, bestDiff, id)
		} else {
			debugLog("transcript: using most recent %s for session %s", pickedPath, id)
		}
		if pickedPath == "" {
			http.Error(w, "no transcript found", http.StatusNotFound)
			return
		}
		data, err = os.ReadFile(pickedPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("could not read transcript: %v", err), http.StatusNotFound)
			return
		}
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Write(data)
}

// POST /api/sessions/{id}/send  body: {"message": "..."}
func (r *Relay) handleSend(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		jsonError(w, "missing session id", http.StatusBadRequest)
		return
	}

	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		jsonError(w, "message must not be empty", http.StatusBadRequest)
		return
	}

	// Verify session exists in our cache before trying to send
	if _, ok := r.sessionByIDOrTitle(id); !ok {
		// Not in cache yet — do a fresh fetch
		sessions, err := r.fetchSessions()
		if err == nil {
			r.mu.Lock()
			for _, s := range sessions {
				r.sessions[s.ID] = s
			}
			r.mu.Unlock()
		}
		if _, ok = r.sessionByIDOrTitle(id); !ok {
			jsonError(w, fmt.Sprintf("session %q not found", id), http.StatusNotFound)
			return
		}
	}

	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	cmd := r.adCmd("session", "send", id, body.Message)
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)

	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("handleSend(%s): %v\noutput: %s", id, err, string(out))
		jsonError(w, fmt.Sprintf("send failed: %s", strings.TrimSpace(string(out))), http.StatusInternalServerError)
		return
	}
	log.Printf("sent message to session %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/events — SSE stream of session events
func (r *Relay) handleEvents(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if behind proxy

	ch := make(chan Event, 32)
	r.addSSEClient(ch)
	defer r.removeSSEClient(ch)

	// Send current snapshot immediately so the client isn't blank
	r.mu.RLock()
	snapshot := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		snapshot = append(snapshot, s)
	}
	r.mu.RUnlock()
	for _, s := range snapshot {
		sc := s
		data, _ := json.Marshal(Event{Type: "snapshot", Session: &sc, Ts: time.Now()})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			debugLog("SSE client disconnected")
			return
		case <-heartbeat.C:
			// Keep connection alive through iOS background network management
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// POST /api/hook — called by the Claude Stop hook script (loopback only)
func (r *Relay) handleHook(w http.ResponseWriter, req *http.Request) {
	// Extra safety: only accept from loopback. The hook script always runs locally.
	// (The route is not wrapped in authMiddleware but we guard here anyway.)
	host := req.RemoteAddr
	if !strings.HasPrefix(host, "127.") && !strings.HasPrefix(host, "[::1]") {
		log.Printf("hook rejected from non-loopback: %s", host)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var payload HookPayload
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.Session == "" {
		jsonError(w, "session field required", http.StatusBadRequest)
		return
	}

	log.Printf("hook received from session %q (summary len=%d)", payload.Session, len(payload.Summary))

	s, ok := r.sessionByIDOrTitle(payload.Session)
	if !ok {
		// Build a minimal session so push has something useful to show
		s = Session{
			ID: payload.Session, Title: payload.Session, Status: "waiting",
		}
		log.Printf("hook: session %q not in cache — will use bare session object", payload.Session)
	}

	// Record in per-session history (capped at 20 entries)
	r.mu.Lock()
	entries := append(r.history[s.ID], HistEntry{Ts: time.Now(), Summary: payload.Summary})
	if len(entries) > 20 {
		entries = entries[len(entries)-20:]
	}
	r.history[s.ID] = entries
	r.mu.Unlock()

	ev := Event{
		Type:    "hook",
		Session: &s,
		Summary: payload.Summary,
		Ts:      time.Now(),
	}
	r.broadcast(ev)
	go r.pushToAll(s, payload.Summary)

	w.WriteHeader(http.StatusNoContent)
}

// POST /api/push/subscribe — register a Web Push subscription from the PWA
func (r *Relay) handleSubscribe(w http.ResponseWriter, req *http.Request) {
	var sub PushSubscription
	if err := json.NewDecoder(req.Body).Decode(&sub); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		jsonError(w, "subscription missing required fields (endpoint, keys.p256dh, keys.auth)", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.state.Subs {
		if existing.Endpoint == sub.Endpoint {
			if existing.Keys.P256dh == sub.Keys.P256dh && existing.Keys.Auth == sub.Keys.Auth {
				log.Printf("push: subscription unchanged (...%s)", last20(sub.Endpoint))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// Keys changed (e.g., previously stored without keys) — update in place
			r.state.Subs[i] = sub
			if err := r.state.save(r.stateFile); err != nil {
				log.Printf("save state after subscribe update: %v", err)
				jsonError(w, "could not persist subscription", http.StatusInternalServerError)
				return
			}
			log.Printf("push: subscription updated (...%s)", last20(sub.Endpoint))
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	r.state.Subs = append(r.state.Subs, sub)
	if err := r.state.save(r.stateFile); err != nil {
		log.Printf("save state after subscribe: %v", err)
		jsonError(w, "could not persist subscription", http.StatusInternalServerError)
		return
	}
	log.Printf("push: new subscription registered (...%s) — total: %d", last20(sub.Endpoint), len(r.state.Subs))
	w.WriteHeader(http.StatusCreated)
}

// GET /api/vapid-public — VAPID public key for the PWA to use when subscribing
func (r *Relay) handleVAPIDPublic(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"publicKey": r.vapidPub})
}

// GET /health — simple liveness check
func (r *Relay) handleHealth(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	pollOK := r.lastPollError == ""
	r.mu.RUnlock()

	status := "ok"
	if !pollOK {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// GET /debug/status — detailed internal state for diagnosing problems
func (r *Relay) handleDebugStatus(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	r.sseMu.Lock()
	sseCount := len(r.sseClients)
	r.sseMu.Unlock()

	sessions := make([]map[string]string, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, map[string]string{
			"id": s.ID, "title": s.Title, "status": s.Status, "tool": s.Tool,
		})
	}
	subEndpoints := make([]string, len(r.state.Subs))
	for i, s := range r.state.Subs {
		subEndpoints[i] = "..." + last20(s.Endpoint)
	}
	info := map[string]any{
		"uptime_approx":     time.Since(r.lastPollTime).Truncate(time.Second).String(),
		"last_poll_time":    r.lastPollTime.Format(time.RFC3339),
		"last_poll_error":   r.lastPollError,
		"poll_count":        r.pollCount,
		"push_sent":         r.pushSent,
		"push_failed":       r.pushFailed,
		"sessions":          sessions,
		"subscriptions":     subEndpoints,
		"sse_clients":       sseCount,
		"vapid_public_key":  r.vapidPub[:20] + "...",
		"profile":           r.profile,
		"state_file":        r.stateFile,
	}
	r.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(info)
}

// GET /debug/push-test?session=<id>&delay=<seconds>  — send a test push to all subscribers.
// delay (default 0) fires the push after N seconds on the server side, so the client
// can navigate away before it arrives (iOS freezes JS timers when the app is backgrounded).
func (r *Relay) handlePushTest(w http.ResponseWriter, req *http.Request) {
	sid := req.URL.Query().Get("session")
	s := Session{ID: sid, Title: sid, Status: "waiting"}
	if sid != "" {
		if found, ok := r.sessionByIDOrTitle(sid); ok {
			s = found
		}
	} else {
		s = Session{ID: "test", Title: "Test Notification", Status: "waiting"}
	}

	delay := 0
	if d := req.URL.Query().Get("delay"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 60 {
			delay = n
		}
	}

	r.mu.RLock()
	subCount := len(r.state.Subs)
	r.mu.RUnlock()

	log.Printf("debug: scheduling test push for session %q to %d subscriber(s) in %ds", s.Title, subCount, delay)
	go func() {
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Second)
		}
		r.pushToAll(s, "This is a test notification from agent-deck-relay.")
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"scheduled":  true,
		"delay":      delay,
		"session":    s.Title,
		"recipients": subCount,
	})
}

// ── Routing ───────────────────────────────────────────────────────────────────

func (r *Relay) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// /api/hook is loopback-only (guarded inside the handler).
	mux.HandleFunc("POST /api/hook", r.handleHook)

	mux.HandleFunc("GET /api/sessions", r.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}/output", r.handleOutput)
	mux.HandleFunc("GET /api/sessions/{id}/transcript", r.handleTranscript)
	mux.HandleFunc("GET /api/sessions/{id}/history", r.handleHistory)
	mux.HandleFunc("POST /api/sessions/{id}/send", r.handleSend)
	mux.HandleFunc("POST /api/sessions/{id}/interrupt", r.handleInterrupt)
	mux.HandleFunc("GET /api/events", r.handleEvents)
	mux.HandleFunc("POST /api/push/subscribe", r.handleSubscribe)
	mux.HandleFunc("GET /api/vapid-public", r.handleVAPIDPublic)
	mux.HandleFunc("GET /health", r.handleHealth)
	mux.HandleFunc("GET /debug/status", r.handleDebugStatus)
	mux.HandleFunc("GET /debug/push-test", r.handlePushTest)

	// PWA static files
	webRoot := *webDir
	if webRoot == "" {
		exe, _ := os.Executable()
		// Walk up from binary path to find web/ — supports running from repo root
		candidates := []string{
			exe + "/../web",
			"./web",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c + "/index.html"); err == nil {
				webRoot = c
				break
			}
		}
		if webRoot == "" {
			log.Println("warning: could not find web/ directory — PWA will not be served")
			webRoot = "/dev/null"
		}
	}
	log.Printf("serving PWA from: %s", webRoot)
	mux.Handle("/", http.FileServer(http.Dir(webRoot)))

	return loggingMiddleware(corsMiddleware(authMiddleware(*tokenFlag, mux)))
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	if debugMode {
		log.Println("debug mode enabled (RELAY_DEBUG=1)")
	}

	// Load or initialise state
	state, err := loadState(*stateFile)
	if err != nil {
		log.Fatalf("FATAL: %v\nHint: delete %s to reset", err, *stateFile)
	}

	// Resolve VAPID keys: state file > generate
	pub, priv := state.VAPIDPublic, state.VAPIDPrivate
	if pub == "" || priv == "" {
		log.Println("generating VAPID key pair (one-time setup)...")
		priv, pub, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Fatalf("generate VAPID keys: %v", err)
		}
		state.VAPIDPublic = pub
		state.VAPIDPrivate = priv
		if err := state.save(*stateFile); err != nil {
			log.Printf("warning: could not save VAPID keys to %s: %v", *stateFile, err)
			log.Printf("  The PWA will need to re-subscribe next restart.")
		} else {
			log.Printf("VAPID keys saved to %s", *stateFile)
		}
	}

	// The webpush library prepends "mailto:" to the subscriber automatically,
	// so strip it if the caller already included it to avoid "mailto:mailto:...".
	email := strings.TrimPrefix(*vapidEmail, "mailto:")
	relay := NewRelay(state, *stateFile, pub, priv, email, *adProfile)

	// Warm up session cache before the first poll tick
	log.Println("fetching initial session list...")
	if sessions, err := relay.fetchSessions(); err != nil {
		log.Printf("warning: initial session fetch failed: %v", err)
		log.Printf("  The poller will retry automatically.")
	} else {
		relay.mu.Lock()
		for _, s := range sessions {
			relay.sessions[s.ID] = s
		}
		relay.mu.Unlock()
		log.Printf("loaded %d session(s)", len(sessions))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relay.poll(ctx)

	srv := &http.Server{
		Addr:         *listenAddr,
		Handler:      relay.buildHandler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE connections have no write deadline
		IdleTimeout:  120 * time.Second,
	}

	// Validate TLS flags
	usingTLS := *tlsCert != "" || *tlsKey != ""
	if usingTLS && (*tlsCert == "" || *tlsKey == "") {
		log.Fatalf("FATAL: --tls-cert and --tls-key must both be provided together")
	}

	scheme := "http"
	if usingTLS {
		scheme = "https"
	}

	// Print startup summary
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │           agent-deck relay                  │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Printf("  Listening    %s://%s\n", scheme, *listenAddr)
	if usingTLS {
		fmt.Printf("  TLS cert     %s\n", *tlsCert)
	} else {
		fmt.Printf("  TLS          disabled (push notifications require HTTPS on iOS)\n")
	}
	if *tokenFlag != "" {
		fmt.Printf("  Auth         token set (%d chars)\n", len(*tokenFlag))
	} else {
		fmt.Printf("  Auth         ⚠ NO TOKEN SET — all API endpoints are public!\n")
	}
	fmt.Printf("  VAPID key    %s...\n", pub[:20])
	fmt.Printf("  Profile      %q\n", *adProfile)
	fmt.Printf("  State file   %s\n", *stateFile)
	fmt.Printf("  Debug        RELAY_DEBUG=1 for verbose logs\n")
	fmt.Printf("  Diagnostics  GET /debug/status\n")
	fmt.Printf("  Test push    GET /debug/push-test\n")
	fmt.Println()

	go func() {
		var err error
		if usingTLS {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	received := <-sig
	log.Printf("received %s — shutting down gracefully...", received)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("done")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func last20(s string) string {
	if len(s) <= 20 {
		return s
	}
	return s[len(s)-20:]
}

func debugLog(format string, args ...any) {
	if debugMode {
		log.Printf("[debug] "+format, args...)
	}
}