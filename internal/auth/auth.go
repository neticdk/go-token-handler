// Package auth runs the OAuth 2.0 authorization code flow on behalf of a
// single page application and keeps the resulting tokens in a server-side
// session. The browser only ever holds an HttpOnly session cookie.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

const (
	// SessionCookie is the name of the cookie that refers to the token
	// session. The __Host- prefix makes browsers refuse it unless it is
	// Secure, has Path=/ and no Domain, so a sibling host cannot plant one.
	SessionCookie = "__Host-token"
	// StateMaxAge is how long a started login stays valid.
	StateMaxAge = 5 * time.Minute

	stateCookiePrefix = "__Host-state_"
	maxBodyBytes      = 16 << 10
	maxPathLength     = 2048
	refreshTimeout    = 15 * time.Second

	keyLogin   = "login"
	keySession = "session"
)

// loginState is kept between starting a login and completing it.
type loginState struct {
	Provider string
	Verifier string
	Nonce    string
	Path     string
}

// tokenSession is the server-side state of a signed-in user.
type tokenSession struct {
	ID       string
	Provider string
	Token    oauth2.Token
	IDToken  string
	Created  time.Time
}

func init() {
	gob.Register(loginState{})
	gob.Register(tokenSession{})
}

// Provider is an OpenID Connect provider users sign in with.
type Provider struct {
	// Issuer is checked against the iss parameter of the authorization
	// response (RFC 9207) when the application passes it on.
	Issuer   string
	OAuth2   *oauth2.Config
	Verifier *oidc.IDTokenVerifier
	// EndSessionURL is the provider's end_session_endpoint. Logout returns a
	// URL to it when set.
	EndSessionURL string
	// RevocationURL is the provider's revocation_endpoint. Logout revokes
	// the refresh token there when set.
	RevocationURL string
}

// Options configures a Handler.
type Options struct {
	Providers map[string]*Provider
	// Store keeps token sessions server-side.
	Store sessions.Store
	// StateStore keeps started logins. It should be a cookie store, so a
	// login that is never completed leaves nothing on the server.
	StateStore sessions.Store
	// BasePath is the path prefix the handler is reached at from the
	// browser, such as "/bff".
	BasePath string
	// SessionMaxAge is the absolute lifetime of a session, whatever the
	// provider allows for its refresh token.
	SessionMaxAge time.Duration
	// PostLogoutRedirectURL is passed to the provider on logout.
	PostLogoutRedirectURL string
	// HTTPClient is used for calls to the providers. Defaults to
	// http.DefaultClient.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Handler serves the login endpoints and authenticates proxied requests.
type Handler struct {
	opts    Options
	refresh singleflight.Group
	now     func() time.Time
}

// New returns a Handler.
func New(opts Options) *Handler {
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Handler{opts: opts, now: time.Now}
}

// Register adds the login endpoints under BasePath + "/auths" to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	p := h.opts.BasePath + "/auths"
	mux.HandleFunc("GET "+p, h.list)
	mux.HandleFunc("POST "+p, h.create)
	mux.HandleFunc("PUT "+p+"/{id}", h.complete)
	mux.HandleFunc("DELETE "+p+"/{id}", h.logout)
}

func (h *Handler) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, h.opts.HTTPClient)
}

func (h *Handler) cookieOptions(maxAge time.Duration) *sessions.Options {
	return &sessions.Options{
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// authResource is the JSON form of a login, as the API has always had it.
type authResource struct {
	ID               string `json:"id,omitempty"`
	Idp              string `json:"idp,omitempty"`
	Path             string `json:"path,omitempty"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	Code             string `json:"code,omitempty"`
	Iss              string `json:"iss,omitempty"`
	LogoutURL        string `json:"logoutUrl,omitempty"`
}

// create starts a login and returns the URL to send the browser to.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in authResource
	if !h.decode(w, r, &in) {
		return
	}
	p, ok := h.opts.Providers[in.Idp]
	if !ok {
		h.fail(w, r, http.StatusBadRequest, "unknown identity provider", nil)
		return
	}
	if !validReturnPath(in.Path) {
		h.fail(w, r, http.StatusBadRequest, "path must be a local path", nil)
		return
	}

	state := rand.Text()
	ls := loginState{
		Provider: in.Idp,
		Verifier: oauth2.GenerateVerifier(),
		Nonce:    rand.Text(),
		Path:     in.Path,
	}
	s := sessions.NewSession(h.opts.StateStore, stateCookiePrefix+state)
	s.IsNew = true
	s.Options = h.cookieOptions(StateMaxAge)
	s.Values[keyLogin] = ls
	if err := s.Save(r, w); err != nil {
		h.fail(w, r, http.StatusInternalServerError, "", fmt.Errorf("save login state: %w", err))
		return
	}

	h.json(w, http.StatusCreated, authResource{
		Idp:              in.Idp,
		Path:             in.Path,
		AuthorizationURL: p.OAuth2.AuthCodeURL(state, oauth2.S256ChallengeOption(ls.Verifier), oidc.Nonce(ls.Nonce)),
	})
}

// complete exchanges the authorization code for tokens and starts a session.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	var in authResource
	if !h.decode(w, r, &in) {
		return
	}
	state := r.PathValue("id")

	s, err := h.opts.StateStore.Get(r, stateCookiePrefix+state)
	ls, ok := s.Values[keyLogin].(loginState)
	if err != nil || !ok {
		h.fail(w, r, http.StatusBadRequest, "unknown or expired login", err)
		return
	}
	// The login state is single use, whatever happens next.
	s.Options = h.cookieOptions(-time.Second)
	if err := s.Save(r, w); err != nil {
		h.fail(w, r, http.StatusInternalServerError, "", fmt.Errorf("remove login state: %w", err))
		return
	}

	p, ok := h.opts.Providers[ls.Provider]
	if !ok {
		h.fail(w, r, http.StatusBadRequest, "unknown identity provider", nil)
		return
	}
	if in.Iss != "" && in.Iss != p.Issuer {
		h.fail(w, r, http.StatusUnauthorized, "authorization response from another issuer", nil)
		return
	}
	ctx := h.ctx(r.Context())
	tok, err := p.OAuth2.Exchange(ctx, in.Code, oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		h.fail(w, r, http.StatusUnauthorized, "code exchange failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := p.Verifier.Verify(ctx, rawID)
	if err != nil {
		h.fail(w, r, http.StatusUnauthorized, "invalid ID token", err)
		return
	}
	if idt.Nonce != ls.Nonce {
		h.fail(w, r, http.StatusUnauthorized, "invalid ID token", errors.New("nonce mismatch"))
		return
	}

	// Drop any earlier session so a session ID never outlives a login.
	if old, err := h.opts.Store.Get(r, SessionCookie); err == nil && !old.IsNew {
		old.Options = h.cookieOptions(-time.Second)
		if err := old.Save(r, w); err != nil {
			h.opts.Logger.WarnContext(r.Context(), "remove previous session", "error", err)
		}
	}

	ts := tokenSession{
		ID:       rand.Text(),
		Provider: ls.Provider,
		Token:    *tok,
		IDToken:  rawID,
		Created:  h.now(),
	}
	ns := sessions.NewSession(h.opts.Store, SessionCookie)
	ns.IsNew = true
	ns.Options = h.cookieOptions(h.opts.SessionMaxAge)
	ns.Values[keySession] = ts
	if err := ns.Save(r, w); err != nil {
		h.fail(w, r, http.StatusInternalServerError, "", fmt.Errorf("save session: %w", err))
		return
	}

	h.json(w, http.StatusOK, authResource{ID: ts.ID, Idp: ls.Provider, Path: ls.Path})
}

// list returns the current login, if there is one.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	type list struct {
		Count    int            `json:"count"`
		Auths    []string       `json:"auths"`
		Included []authResource `json:"@included,omitempty"`
	}
	l := list{Auths: []string{}}
	if _, ts, ok := h.session(r); ok {
		l.Count = 1
		l.Auths = append(l.Auths, ts.ID)
		l.Included = []authResource{{ID: ts.ID, Idp: ts.Provider}}
	}
	h.json(w, http.StatusOK, l)
}

// logout ends the session, revokes its refresh token and returns the
// provider's logout URL for the browser to visit.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	s, ts, ok := h.session(r)
	if !ok || ts.ID != r.PathValue("id") {
		h.fail(w, r, http.StatusNotFound, "no such login", nil)
		return
	}
	s.Options = h.cookieOptions(-time.Second)
	if err := s.Save(r, w); err != nil {
		h.fail(w, r, http.StatusInternalServerError, "", fmt.Errorf("remove session: %w", err))
		return
	}

	p, ok := h.opts.Providers[ts.Provider]
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.revoke(h.ctx(r.Context()), p, ts.Token.RefreshToken); err != nil {
		h.opts.Logger.WarnContext(r.Context(), "revoke refresh token", "error", err)
	}
	if p.EndSessionURL == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.json(w, http.StatusOK, authResource{ID: ts.ID, Idp: ts.Provider, LogoutURL: h.logoutURL(p, ts.IDToken)})
}

func (h *Handler) logoutURL(p *Provider, idToken string) string {
	u, err := url.Parse(p.EndSessionURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", p.OAuth2.ClientID)
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	if h.opts.PostLogoutRedirectURL != "" {
		q.Set("post_logout_redirect_uri", h.opts.PostLogoutRedirectURL)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (h *Handler) revoke(ctx context.Context, p *Provider, refreshToken string) error {
	if p.RevocationURL == "" || refreshToken == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.RevocationURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(p.OAuth2.ClientID), url.QueryEscape(p.OAuth2.ClientSecret))
	resp, err := h.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revocation endpoint returned %s", resp.Status)
	}
	return nil
}

// Require lets a request through only with a live session, and replaces its
// Authorization header with the session's access token.
func (h *Handler) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ts, ok := h.session(r)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if h.now().Sub(ts.Created) > h.opts.SessionMaxAge {
			h.end(w, r, s)
			return
		}
		tok, err := h.token(r, ts)
		if err != nil {
			h.opts.Logger.InfoContext(r.Context(), "token refresh failed, ending session", "error", err)
			h.end(w, r, s)
			return
		}
		if tok.AccessToken != ts.Token.AccessToken {
			ts.Token = *tok
			if id, ok := tok.Extra("id_token").(string); ok && id != "" {
				ts.IDToken = id
			}
			s.Values[keySession] = ts
			if err := s.Save(r, w); err != nil {
				h.opts.Logger.WarnContext(r.Context(), "save refreshed token", "error", err)
			}
		}

		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		next.ServeHTTP(w, r)
	})
}

// token returns a valid access token, refreshing it when it has expired.
// Concurrent requests of one session share a single refresh.
func (h *Handler) token(r *http.Request, ts tokenSession) (*oauth2.Token, error) {
	if ts.Token.Valid() {
		return &ts.Token, nil
	}
	p, ok := h.opts.Providers[ts.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown identity provider %q", ts.Provider)
	}
	v, err, _ := h.refresh.Do(ts.ID, func() (any, error) {
		// Not tied to one request: other requests may be waiting on it.
		ctx, cancel := context.WithTimeout(h.ctx(context.WithoutCancel(r.Context())), refreshTimeout)
		defer cancel()
		return p.OAuth2.TokenSource(ctx, &ts.Token).Token()
	})
	if err != nil {
		return nil, err
	}
	return v.(*oauth2.Token), nil
}

func (h *Handler) session(r *http.Request) (*sessions.Session, tokenSession, bool) {
	s, err := h.opts.Store.Get(r, SessionCookie)
	if err != nil {
		return s, tokenSession{}, false
	}
	ts, ok := s.Values[keySession].(tokenSession)
	return s, ts, ok
}

// end removes the session and answers 401.
func (h *Handler) end(w http.ResponseWriter, r *http.Request, s *sessions.Session) {
	s.Options = h.cookieOptions(-time.Second)
	if err := s.Save(r, w); err != nil {
		h.opts.Logger.WarnContext(r.Context(), "remove session", "error", err)
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		h.fail(w, r, http.StatusBadRequest, "invalid request body", err)
		return false
	}
	return true
}

func (h *Handler) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errchkjson // the status is already written
}

// fail answers with status and a short message, and logs the cause. The
// cause never reaches the client.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, msg string, cause error) {
	if msg == "" {
		msg = http.StatusText(status)
	}
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	h.opts.Logger.Log(r.Context(), level, msg, "status", status, "error", cause)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, status)
}

// validReturnPath accepts an empty path or one on this site, so the
// application cannot be made to redirect elsewhere after login.
func validReturnPath(p string) bool {
	if p == "" {
		return true
	}
	return len(p) <= maxPathLength && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "/\\")
}
