package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"rclient/web"
)

// Config holds all runtime configuration for the server.
type Config struct {
	Listen string // e.g. ":8080"

	// AgentPath is the full path the agent WebSocket is served at, e.g.
	// "/ws/E9sqEPBUXMe3vACe7uX1teqhP". A long random string makes the endpoint
	// invisible to scanners; the bearer token is the second wall.
	AgentPath string

	// PanelPath is the URL prefix under which the UI and the panel WebSocket
	// are served, e.g. "/ui/zNRA7xa96sH9W2uQZZAqawg6w". Anything outside this
	// prefix returns plain 404.
	PanelPath string

	AgentToken string // shared secret expected from agents
	PanelUser  string // basic auth username for the panel
	PanelPass  string // basic auth password for the panel

	// EnrollPath is the JSON file holding agent_id -> per-agent secret.
	// Defaults to /var/lib/rclient/enroll.json inside the container.
	EnrollPath string
}

// Validate checks Config for misconfiguration that would lead to a wide-open
// or non-functional server.
func (c Config) Validate() error {
	if c.AgentPath == "" || !strings.HasPrefix(c.AgentPath, "/") {
		return errors.New("AgentPath must be set and start with /")
	}
	if c.PanelPath == "" || !strings.HasPrefix(c.PanelPath, "/") {
		return errors.New("PanelPath must be set and start with /")
	}
	if c.AgentPath == c.PanelPath {
		return errors.New("AgentPath and PanelPath must differ")
	}
	// Also reject one being a path-prefix of the other; otherwise ServeMux
	// route ordering would silently shadow whichever was registered last.
	a := strings.TrimRight(c.AgentPath, "/") + "/"
	p := strings.TrimRight(c.PanelPath, "/") + "/"
	if strings.HasPrefix(a, p) || strings.HasPrefix(p, a) {
		return errors.New("AgentPath and PanelPath must not nest")
	}
	if c.AgentToken == "" {
		return errors.New("AgentToken must be set")
	}
	if c.PanelPass == "" {
		return errors.New("PanelPass must be set")
	}
	return nil
}

type Server struct {
	cfg      Config
	hub      *Hub
	log      *slog.Logger
	throttle *loginThrottle
	enroll   *enrollStore
}

func New(cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.EnrollPath == "" {
		cfg.EnrollPath = "/var/lib/rclient/enroll.json"
	}
	es, err := newEnrollStore(cfg.EnrollPath)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		hub:      NewHub(log),
		log:      log,
		throttle: newLoginThrottle(),
		enroll:   es,
	}, nil
}

// Run blocks serving HTTP until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Agent endpoint. Path is randomized via config, plus a bearer token check
	// inside the handler. Anything else on the mux falls through to a plain 404.
	mux.HandleFunc(s.cfg.AgentPath, s.handleAgentWS)

	panelPrefix := strings.TrimRight(s.cfg.PanelPath, "/")

	// Auth endpoints — public (no session required), throttled inside the
	// handler. They live under the panel prefix so they're as obscure as
	// the panel itself.
	mux.HandleFunc(panelPrefix+"/login", s.handleLoginPage)
	mux.HandleFunc(panelPrefix+"/api/login", s.handleLoginSubmit)
	mux.HandleFunc(panelPrefix+"/api/logout", s.handleLogout)

	// Authenticated panel routes.
	mux.HandleFunc(panelPrefix+"/ws", s.authMiddleware(s.handlePanelWS))
	mux.HandleFunc(panelPrefix+"/api/connect", s.authMiddleware(s.handleConnectBlob))

	// Static panel UI from the embedded fs, served under PanelPath/.
	// Static files (css/js/fonts) are served WITHOUT auth so the login page
	// can pull its stylesheet. The index document and any HTML route still
	// require a valid session — we enforce that inline below.
	sub, err := fs.Sub(web.Files, "static")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc(panelPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		s.writeSecurityHeaders(w)
		rel := strings.TrimPrefix(r.URL.Path, panelPrefix)
		if rel == "" {
			rel = "/"
		}
		// Hide the raw login template from the file server — only the
		// dedicated /login handler should serve it.
		if rel == "/login.html" {
			http.NotFound(w, r)
			return
		}
		// The index page (and only the index) is gated behind a session.
		// Everything else under the panel prefix is harmless static.
		if rel == "/" || rel == "/index.html" {
			if !s.hasValidSession(r) {
				http.Redirect(w, r, panelPrefix+"/login", http.StatusSeeOther)
				return
			}
		}
		// Forward to the file server with a clean inner URL so paths like
		// /ui/<rand>/style.css resolve to /style.css inside the embed FS.
		r2 := *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.Path = rel
		fileServer.ServeHTTP(w, &r2)
	})

	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: mux,
	}

	go s.hub.pingLoop(ctx)

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	s.log.Info("server listening",
		"addr", s.cfg.Listen,
		"agent_path", s.cfg.AgentPath,
		"panel_path", panelPrefix+"/",
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
