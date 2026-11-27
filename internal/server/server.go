package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"github.com/neticdk/go-token-handler/internal/auth"
	"github.com/neticdk/go-token-handler/internal/proxy"
	"golang.org/x/oauth2"
)

const (
	cleanupInterval = 10 * time.Minute
	// maxSessionBytes caps the encoded size of a session: a few tokens.
	maxSessionBytes = 32 << 10
	// providerTimeout bounds every call to an identity provider.
	providerTimeout = 30 * time.Second
)

// corsHeaders are the request headers a trusted cross-origin page may send.
var corsHeaders = "Accept, Accept-Language, Content-Type, If-Match, If-None-Match, Prefer, X-Request-Id"

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	h, err := New(ctx, cfg, logger)
	if err != nil {
		return err
	}

	go cleanSessions(ctx, cfg.SessionPath, cfg.SessionMaxAge, logger)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// New builds the handler. It discovers each provider, so the providers must
// be reachable.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	hashKey, blockKey, err := cfg.keys()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.SessionPath, 0o700); err != nil {
		return nil, fmt.Errorf("session path: %w", err)
	}
	store := sessions.NewFilesystemStore(cfg.SessionPath, hashKey, blockKey)
	store.MaxLength(maxSessionBytes)
	// Logins in progress live only in their cookie, so starting one, which
	// needs no authentication, writes nothing to disk.
	stateStore := sessions.NewCookieStore(hashKey, blockKey)
	stateStore.MaxAge(int(auth.StateMaxAge.Seconds()))

	client := &http.Client{Timeout: providerTimeout}
	providers := map[string]*auth.Provider{}
	for name, pc := range cfg.Providers {
		p, err := discover(oidc.ClientContext(ctx, client), pc, cfg.RedirectURL)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		providers[name] = p
	}

	a := auth.New(auth.Options{
		Providers:             providers,
		Store:                 store,
		StateStore:            stateStore,
		HTTPClient:            client,
		BasePath:              cfg.BasePath,
		SessionMaxAge:         cfg.SessionMaxAge,
		PostLogoutRedirectURL: cfg.PostLogoutRedirectURL,
		Logger:                logger,
	})

	mux := http.NewServeMux()
	a.Register(mux)
	mux.HandleFunc("GET "+cfg.BasePath+"/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for _, uc := range cfg.Upstreams {
		target, err := url.Parse(uc.URL)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", uc.Path, err)
		}
		path := cfg.BasePath + uc.Path
		mux.Handle(path+"/", a.Require(proxy.New(proxy.Upstream{
			Path:        path,
			URL:         target,
			StripPrefix: uc.StripPrefix,
		}, logger)))
	}

	cop := http.NewCrossOriginProtection()
	for _, o := range cfg.Origins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("origin %s: %w", o, err)
		}
	}
	var h http.Handler = mux
	h = cop.Handler(h)
	h = cors(cfg.Origins, h)
	h = logRequests(logger, h)
	return h, nil
}

func discover(ctx context.Context, pc ProviderConfig, redirectURL string) (*auth.Provider, error) {
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	op, err := oidc.NewProvider(ctx, pc.Issuer)
	if err != nil {
		return nil, err
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
		Revocation string `json:"revocation_endpoint"`
	}
	if err := op.Claims(&meta); err != nil {
		return nil, fmt.Errorf("read provider metadata: %w", err)
	}
	scopes := []string{oidc.ScopeOpenID}
	for _, s := range pc.Scopes {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	return &auth.Provider{
		Issuer: pc.Issuer,
		OAuth2: &oauth2.Config{
			ClientID:     pc.ClientID,
			ClientSecret: pc.ClientSecret,
			Endpoint:     op.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       scopes,
		},
		Verifier:      op.Verifier(&oidc.Config{ClientID: pc.ClientID}),
		EndSessionURL: meta.EndSession,
		RevocationURL: meta.Revocation,
	}, nil
}

// cors answers preflight requests from the configured origins and marks
// their responses as readable. It never widens what CrossOriginProtection
// allows: an origin it does not list gets no CORS headers.
func cors(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !slices.Contains(origins, origin) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", corsHeaders)
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per request: method, path, status and time.
// Query strings and headers are left out, since they can carry secrets.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
			"user_agent", r.UserAgent(),
		)
	})
}

// cleanSessions removes session files older than maxAge. The filesystem
// store only deletes a file on logout, so abandoned sessions would stay
// on disk forever.
func cleanSessions(ctx context.Context, dir string, maxAge time.Duration, logger *slog.Logger) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		removeExpired(dir, maxAge, time.Now(), logger)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func removeExpired(dir string, maxAge time.Duration, now time.Time, logger *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("read session directory", "error", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "session_") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("remove expired session", "error", err)
		}
	}
}
