package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"rclient/internal/proto"
	"rclient/web"
)

// loginThrottle slows down repeated bad-auth attempts from the same IP.
// In-memory only; per-process. For three boxes this is plenty.
type loginThrottle struct {
	mu    sync.Mutex
	fails map[string]*throttleEntry
}

type throttleEntry struct {
	count    int
	lockUntil time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{fails: map[string]*throttleEntry{}}
}

// check returns a delay to apply before answering, and whether the attempt
// should be answered with 401 immediately (i.e. is currently locked out).
func (t *loginThrottle) check(ip string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		return 0, false
	}
	if time.Now().Before(e.lockUntil) {
		return 0, true
	}
	// Quadratic backoff up to 5s of artificial latency. Stops fast brute force
	// without affecting the legit user noticeably.
	d := time.Duration(e.count*e.count) * 100 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d, false
}

func (t *loginThrottle) registerFailure(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fails[ip]
	if e == nil {
		e = &throttleEntry{}
		t.fails[ip] = e
	}
	e.count++
	// Hard lock after 10 failures for 10 minutes.
	if e.count >= 10 {
		e.lockUntil = time.Now().Add(10 * time.Minute)
	}
}

func (t *loginThrottle) registerSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, ip)
}

// clientIP extracts the client address. Caddy is expected to set X-Real-IP
// since the upstream connection is from the proxy itself.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// Take the first one, that's the original client.
		if i := strings.IndexByte(v, ','); i > 0 {
			return v[:i]
		}
		return v
	}
	return r.RemoteAddr
}

// authMiddleware gates panel HTTP and WS endpoints behind a valid session
// cookie. Unauthenticated browser GETs to HTML routes are redirected to
// the login page; everything else (WS, API) gets a 401 so JavaScript can
// react cleanly.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeSecurityHeaders(w)
		if s.hasValidSession(r) {
			next(w, r)
			return
		}
		// Decide: redirect to login (for documents) or return 401 (for API/WS).
		if r.Method == http.MethodGet && wantsHTML(r) {
			http.Redirect(w, r, s.cookiePath()+"login", http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// writeSecurityHeaders sets the cheap defensive headers used everywhere on
// the panel. Pulled out so the login page gets them too.
func (s *Server) writeSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// CSP: allow our own assets + Google Fonts (used by the panel CSS).
	// xterm.js writes inline styles on its canvas/elements, so style-src
	// has to permit 'unsafe-inline' for styles only — script-src stays
	// strict ('self' only).
	h.Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self'; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src https://fonts.gstatic.com; "+
			"img-src 'self' data:; "+
			"connect-src 'self' wss: ws:; "+
			"frame-ancestors 'none'; "+
			"base-uri 'none'; "+
			"form-action 'self'")
}

// wantsHTML returns true when the request is a normal browser navigation
// rather than an XHR/WebSocket call.
func wantsHTML(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return false
	}
	a := r.Header.Get("Accept")
	return a == "" || strings.Contains(a, "text/html") || strings.Contains(a, "*/*")
}

// handleLoginPage serves the static login HTML.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityHeaders(w)
	if s.hasValidSession(r) {
		http.Redirect(w, r, s.cookiePath(), http.StatusSeeOther)
		return
	}
	data, err := web.Files.ReadFile("static/login.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// handleLoginSubmit accepts a JSON {user, password} body and either sets the
// session cookie or returns 401. Throttled by IP just like the old basic
// auth flow.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	delay, locked := s.throttle.check(ip)
	if locked {
		w.Header().Set("Retry-After", "600")
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}

	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(body.User), []byte(s.cfg.PanelUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.cfg.PanelPass)) == 1
	if !userOK || !passOK {
		if delay > 0 {
			time.Sleep(delay)
		}
		s.throttle.registerFailure(ip)
		s.log.Info("panel login failed", "from", ip)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	s.throttle.registerSuccess(ip)
	s.setSessionCookie(w, r)
	s.log.Info("panel login ok", "from", ip)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleLogout clears the cookie and tells the panel to redirect.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityHeaders(w)
	s.clearSessionCookie(w)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleConnectBlob returns the {url, token} blob the panel shows when the
// user clicks "Add device". The URL is derived from the request — whatever
// hostname the user is hitting the panel at is what the agent will dial.
func (s *Server) handleConnectBlob(w http.ResponseWriter, r *http.Request) {
	s.writeSecurityHeaders(w)
	host := r.Host
	if host == "" {
		http.Error(w, "no host", http.StatusInternalServerError)
		return
	}
	url := "wss://" + host + s.cfg.AgentPath

	payload := map[string]string{"url": url, "token": s.cfg.AgentToken}
	raw, _ := json.Marshal(payload)
	blob := base64.RawURLEncoding.EncodeToString(raw)

	resp := map[string]string{
		"connect": blob,
		"url":     url,
	}
	out, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// originAllowed verifies that the WebSocket Origin header matches the
// request Host, rejecting cross-origin upgrade requests. This stops a
// malicious page in the same browser from hijacking a cached basic-auth
// session against the panel WS endpoint.
//
// Browsers always include the port in Origin if it isn't the scheme
// default; reverse proxies sometimes drop it from Host. We compare both
// the full host:port and the bare hostname so the check works whether
// or not Host has been rewritten upstream.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Native clients (curl, our own ws library) won't set Origin.
		// We only enforce when the browser sets it.
		return true
	}
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(origin, prefix) {
			origin = origin[len(prefix):]
			break
		}
	}
	if origin == r.Host {
		return true
	}
	// Compare bare hostnames in case the proxy stripped the port from Host.
	originHost := origin
	if i := strings.IndexByte(origin, ':'); i >= 0 {
		originHost = origin[:i]
	}
	rHost := r.Host
	if i := strings.IndexByte(rHost, ':'); i >= 0 {
		rHost = rHost[:i]
	}
	return originHost != "" && originHost == rHost
}

func (s *Server) handlePanelWS(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// We did the origin check ourselves above with our own rule.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)

	pc := &panelConn{
		id:     uuid.NewString(),
		ws:     ws,
		sendCh: make(chan proto.Envelope, 256),
	}
	s.hub.addPanel(pc)
	defer s.hub.removePanel(pc.id)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Send the initial agent list snapshot.
	list := proto.AgentList{Agents: s.hub.snapshot()}
	if raw, err := json.Marshal(list); err == nil {
		pc.sendCh <- proto.Envelope{Type: proto.TypeAgentList, Data: raw}
	}

	// Writer.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-pc.sendCh:
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := writeJSON(wctx, ws, env)
				wcancel()
				if err != nil {
					_ = ws.Close(websocket.StatusInternalError, "write failed")
					return
				}
			}
		}
	}()

	// Reader.
	for {
		var env proto.Envelope
		if err := readJSON(ctx, ws, &env); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.log.Debug("panel disconnected", "err", err)
			}
			return
		}
		s.dispatchPanelMessage(env, clientIP(r))
	}
}

// dispatchPanelMessage handles a single message from a panel client.
func (s *Server) dispatchPanelMessage(env proto.Envelope, ip string) {
	switch env.Type {
	case proto.TypePanelShellOpen:
		var o proto.PanelShellOpen
		if err := json.Unmarshal(env.Data, &o); err != nil {
			return
		}
		// Audit log: every shell open is recorded with the originating IP so
		// that later shell output can be traced back to a panel session.
		s.log.Info("panel shell open",
			"agent", o.AgentID, "shell", o.ShellID,
			"from", ip, "cols", o.Cols, "rows", o.Rows)
		raw, _ := json.Marshal(proto.ShellOpen{
			ShellID: o.ShellID, Cols: o.Cols, Rows: o.Rows,
		})
		s.hub.sendToAgent(o.AgentID, proto.Envelope{Type: proto.TypeShellOpen, Data: raw})

	case proto.TypePanelShellInput:
		var in proto.PanelShellInput
		if err := json.Unmarshal(env.Data, &in); err != nil {
			return
		}
		raw, _ := json.Marshal(proto.ShellInput{ShellID: in.ShellID, Data: in.Data})
		s.hub.sendToAgent(in.AgentID, proto.Envelope{Type: proto.TypeShellInput, Data: raw})

	case proto.TypePanelShellResize:
		var rs proto.PanelShellResize
		if err := json.Unmarshal(env.Data, &rs); err != nil {
			return
		}
		raw, _ := json.Marshal(proto.ShellResize{
			ShellID: rs.ShellID, Cols: rs.Cols, Rows: rs.Rows,
		})
		s.hub.sendToAgent(rs.AgentID, proto.Envelope{Type: proto.TypeShellResize, Data: raw})

	case proto.TypePanelShellClose:
		var c proto.PanelShellClose
		if err := json.Unmarshal(env.Data, &c); err != nil {
			return
		}
		s.log.Info("panel shell close",
			"agent", c.AgentID, "shell", c.ShellID, "from", ip)
		raw, _ := json.Marshal(proto.ShellClose{ShellID: c.ShellID})
		s.hub.sendToAgent(c.AgentID, proto.Envelope{Type: proto.TypeShellClose, Data: raw})
	}
}
