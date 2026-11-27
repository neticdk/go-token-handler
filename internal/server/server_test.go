package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const devOrigin = "http://localhost:5173"

type upstreamRequest struct {
	path, rawQuery, authorization, cookie string
	header                                http.Header
}

type harness struct {
	t        *testing.T
	idp      *fakeIdP
	app      *httptest.Server
	client   *http.Client
	sessions string

	mu       sync.Mutex
	upstream []upstreamRequest
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, idp: newFakeIdP(t), sessions: t.TempDir()}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.upstream = append(h.upstream, upstreamRequest{
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			cookie:        r.Header.Get("Cookie"),
			header:        r.Header.Clone(),
		})
		h.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "upstream", Value: "1"})
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)

	h.app = httptest.NewUnstartedServer(nil)
	h.app.StartTLS()
	t.Cleanup(h.app.Close)

	cfg := Config{
		BasePath:              "/bff",
		Origins:               []string{h.app.URL, devOrigin},
		RedirectURL:           h.app.URL + "/callback",
		PostLogoutRedirectURL: h.app.URL + "/",
		HashKey:               randomKey(t, 64),
		BlockKey:              randomKey(t, 32),
		SessionPath:           h.sessions,
		SessionMaxAge:         time.Hour,
		Providers: map[string]ProviderConfig{
			"netic": {Issuer: h.idp.issuer(), ClientID: h.idp.clientID, ClientSecret: h.idp.clientSecret, Scopes: []string{"profile"}},
		},
		Upstreams: []UpstreamConfig{{Path: "/api", URL: up.URL, StripPrefix: true}},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	handler, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	h.app.Config.Handler = handler

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	h.client = h.app.Client()
	h.client.Jar = jar
	return h
}

func randomKey(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// do sends a request the way a browser on the application's origin would.
func (h *harness) do(method, path string, body any, headers ...string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(h.t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.app.URL+path, r)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", h.app.URL)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

type authResource struct {
	ID               string `json:"id"`
	Idp              string `json:"idp"`
	Path             string `json:"path"`
	AuthorizationURL string `json:"authorizationUrl"`
	LogoutURL        string `json:"logoutUrl"`
}

// startLogin starts a login and signs the user in at the provider. It
// returns the state and the code the provider would redirect back with.
func (h *harness) startLogin() (state, code string) {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/bff/auths", map[string]string{"idp": "netic", "path": "/clusters"})
	require.Equal(h.t, http.StatusCreated, resp.StatusCode)
	ar := decodeJSON[authResource](h.t, resp)
	u, err := url.Parse(ar.AuthorizationURL)
	require.NoError(h.t, err)
	q := u.Query()
	require.Equal(h.t, "S256", q.Get("code_challenge_method"))
	require.Equal(h.t, "openid profile", q.Get("scope"))
	require.NotEmpty(h.t, q.Get("nonce"))
	code = rand.Text()
	h.idp.authorize(code, q.Get("code_challenge"), q.Get("nonce"))
	return q.Get("state"), code
}

func (h *harness) login() string {
	h.t.Helper()
	state, code := h.startLogin()
	resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code})
	require.Equal(h.t, http.StatusOK, resp.StatusCode)
	ar := decodeJSON[authResource](h.t, resp)
	assert.Equal(h.t, "/clusters", ar.Path)
	require.NotEmpty(h.t, ar.ID)
	return ar.ID
}

func (h *harness) upstreamRequests() []upstreamRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]upstreamRequest(nil), h.upstream...)
}

func sessionFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "session_*"))
	require.NoError(t, err)
	return m
}

func TestLoginProxyLogout(t *testing.T) {
	h := newHarness(t, nil)
	id := h.login()

	resp := h.do(http.MethodGet, "/bff/auths", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	list := decodeJSON[struct {
		Count int      `json:"count"`
		Auths []string `json:"auths"`
	}](t, resp)
	assert.Equal(t, 1, list.Count)
	assert.Equal(t, []string{id}, list.Auths)

	resp = h.do(http.MethodGet, "/bff/api/clusters/a?x=1", nil,
		"Authorization", "Bearer forged", "X-Forwarded-For", "1.2.3.4", "Cookie", "other_app=secret")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Values("Set-Cookie"), "upstream cookies must not reach the browser")
	assert.Equal(t, h.app.URL, resp.Header.Get("Access-Control-Allow-Origin"), "the upstream's CORS headers are dropped")
	reqs := h.upstreamRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/clusters/a", reqs[0].path)
	for _, k := range []string{"X-Forwarded-For", "Origin", "Sec-Fetch-Site", "Cookie"} {
		assert.Empty(t, reqs[0].header.Get(k), k)
	}
	assert.Equal(t, "x=1", reqs[0].rawQuery)
	assert.True(t, strings.HasPrefix(reqs[0].authorization, "Bearer at-"), reqs[0].authorization)
	assert.Empty(t, reqs[0].cookie, "the session cookie must not reach the upstream")

	resp = h.do(http.MethodDelete, "/bff/auths/"+id, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	lo := decodeJSON[authResource](t, resp)
	u, err := url.Parse(lo.LogoutURL)
	require.NoError(t, err)
	assert.Equal(t, h.idp.issuer()+"/logout", u.Scheme+"://"+u.Host+u.Path)
	assert.Equal(t, h.idp.clientID, u.Query().Get("client_id"))
	assert.NotEmpty(t, u.Query().Get("id_token_hint"))
	assert.Equal(t, h.app.URL+"/", u.Query().Get("post_logout_redirect_uri"))
	assert.Len(t, h.idp.revokedTokens(), 1)
	assert.Empty(t, sessionFiles(t, h.sessions))

	resp = h.do(http.MethodGet, "/bff/api/clusters", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestStaleSessionCookie(t *testing.T) {
	h := newHarness(t, nil)
	h.login()
	u, err := url.Parse(h.app.URL)
	require.NoError(t, err)
	stale := h.client.Jar.Cookies(u)
	require.NotEmpty(t, stale)

	// The session file disappears, as after a restart without a volume.
	for _, f := range sessionFiles(t, h.sessions) {
		require.NoError(t, os.Remove(f))
	}
	resp := h.do(http.MethodGet, "/bff/auths", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 0, decodeJSON[struct {
		Count int `json:"count"`
	}](t, resp).Count)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/bff/api/clusters", nil).StatusCode)

	h.client.Jar.SetCookies(u, stale)
	h.login()
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, "/bff/api/clusters", nil).StatusCode)
}

func TestPlantedSessionCookie(t *testing.T) {
	h := newHarness(t, nil)
	h.login()
	u, err := url.Parse(h.app.URL)
	require.NoError(t, err)
	attacker := h.client.Jar.Cookies(u)

	// The victim's browser carries the attacker's session cookie and logs in.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	jar.SetCookies(u, attacker)
	h.client.Jar = jar
	h.login()

	victim := jar.Cookies(u)
	require.Len(t, victim, 1)
	assert.NotEqual(t, attacker[0].Value, victim[0].Value, "login issues a new session ID")

	h.client.Jar, err = cookiejar.New(nil)
	require.NoError(t, err)
	h.client.Jar.SetCookies(u, attacker)
	assert.Equal(t, http.StatusUnauthorized, h.do(http.MethodGet, "/bff/api/clusters", nil).StatusCode,
		"the planted session does not become the victim's")
}

func TestProxyConfinement(t *testing.T) {
	h := newHarness(t, nil)
	h.login()
	tests := []struct {
		name    string
		path    string
		headers []string
	}{
		{"dot segments", "/bff/api/../admin/secret", nil},
		{"encoded dot segments", "/bff/api/%2e%2e/admin/secret", nil},
		{"encoded slash", "/bff/api/a%2F..%2Fadmin", nil},
		{"encoded backslash", "/bff/api/a%5C..%5Cadmin", nil},
		{"websocket upgrade", "/bff/api/watch", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(http.MethodGet, tt.path, nil, tt.headers...)
			assert.NotEqual(t, http.StatusOK, resp.StatusCode)
			assert.Empty(t, h.upstreamRequests())
		})
	}
}

func TestErrorsDoNotLeakDetails(t *testing.T) {
	h := newHarness(t, nil)
	state, _ := h.startLogin()
	resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": "wrong"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "code exchange failed\n", string(body))
}

func TestLoginStateLeavesNoFiles(t *testing.T) {
	h := newHarness(t, nil)
	for range 50 {
		resp := h.do(http.MethodPost, "/bff/auths", map[string]string{"idp": "netic"})
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}
	assert.Empty(t, sessionFiles(t, h.sessions))

	resp := h.do(http.MethodPost, "/bff/auths", map[string]string{"idp": "netic", "path": "/" + strings.Repeat("a", 20<<20)})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUnauthenticatedProxyCall(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(http.MethodGet, "/bff/api/clusters", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, h.upstreamRequests())
}

func TestCreateRejects(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		name string
		body map[string]string
	}{
		{"unknown provider", map[string]string{"idp": "other"}},
		{"protocol relative path", map[string]string{"idp": "netic", "path": "//evil.example"}},
		{"absolute URL", map[string]string{"idp": "netic", "path": "https://evil.example"}},
		{"backslash path", map[string]string{"idp": "netic", "path": "/\\evil.example"}},
		{"overlong path", map[string]string{"idp": "netic", "path": "/" + strings.Repeat("a", 4096)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/bff/auths", tt.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestCompleteRejects(t *testing.T) {
	t.Run("unknown state", func(t *testing.T) {
		h := newHarness(t, nil)
		resp := h.do(http.MethodPut, "/bff/auths/NOSUCHSTATE", map[string]string{"code": "x"})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("state used twice", func(t *testing.T) {
		h := newHarness(t, nil)
		state, code := h.startLogin()
		resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp = h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		h := newHarness(t, nil)
		h.idp.nonceOverride = "other"
		state, code := h.startLogin()
		resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		resp = h.do(http.MethodGet, "/bff/api/clusters", nil)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("issuer mismatch", func(t *testing.T) {
		h := newHarness(t, nil)
		state, code := h.startLogin()
		resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code, "iss": "https://other.example"})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("matching issuer", func(t *testing.T) {
		h := newHarness(t, nil)
		state, code := h.startLogin()
		resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": code, "iss": h.idp.issuer()})
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("bad code", func(t *testing.T) {
		h := newHarness(t, nil)
		state, _ := h.startLogin()
		resp := h.do(http.MethodPut, "/bff/auths/"+state, map[string]string{"code": "wrong"})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestRefreshIsSharedAndSaved(t *testing.T) {
	h := newHarness(t, nil)
	// Within oauth2's ten-second expiry margin, so the first call refreshes.
	h.idp.firstExpiresIn = 5
	h.login()

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() {
			codes[i] = h.do(http.MethodGet, "/bff/api/clusters", nil).StatusCode
		})
	}
	wg.Wait()
	for _, c := range codes {
		assert.Equal(t, http.StatusOK, c)
	}
	assert.Equal(t, 1, h.idp.refreshCount(), "concurrent requests share one refresh")

	resp := h.do(http.MethodGet, "/bff/api/clusters", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, h.idp.refreshCount(), "the refreshed token is kept in the session")

	reqs := h.upstreamRequests()
	assert.Equal(t, reqs[0].authorization, reqs[len(reqs)-1].authorization)
}

func TestRefreshFailureEndsSession(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.firstExpiresIn = 5
	h.login()
	h.idp.mu.Lock()
	h.idp.refreshTokens = map[string]bool{}
	h.idp.mu.Unlock()

	resp := h.do(http.MethodGet, "/bff/api/clusters", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, sessionFiles(t, h.sessions))
	assert.Empty(t, h.upstreamRequests())
}

func TestSessionMaxAge(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SessionMaxAge = time.Second })
	h.login()
	time.Sleep(1100 * time.Millisecond)
	resp := h.do(http.MethodGet, "/bff/api/clusters", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLoginReplacesSession(t *testing.T) {
	h := newHarness(t, nil)
	first := h.login()
	second := h.login()
	assert.NotEqual(t, first, second)
	assert.Len(t, sessionFiles(t, h.sessions), 1, "the earlier session is removed")
}

func TestCrossOrigin(t *testing.T) {
	h := newHarness(t, nil)
	login := map[string]string{"idp": "netic"}
	tests := []struct {
		name       string
		headers    []string
		wantStatus int
		wantACAO   string
	}{
		{"same origin", nil, http.StatusCreated, h.app.URL},
		{"sibling host on the same site", []string{"Sec-Fetch-Site", "same-site", "Origin", "https://other.netic.dk"}, http.StatusForbidden, ""},
		{"cross site", []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}, http.StatusForbidden, ""},
		{"trusted development origin", []string{"Sec-Fetch-Site", "cross-site", "Origin", devOrigin}, http.StatusCreated, devOrigin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/bff/auths", login, tt.headers...)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantACAO, resp.Header.Get("Access-Control-Allow-Origin"))
		})
	}

	// A simple form-style POST needs no preflight, so the server must refuse
	// it itself. Browsers without Sec-Fetch-Site are caught by Origin.
	h.login()
	for _, headers := range [][]string{
		{"Content-Type", "text/plain", "Sec-Fetch-Site", "same-site", "Origin", "https://other.netic.dk"},
		{"Content-Type", "text/plain", "Sec-Fetch-Site", "", "Origin", "https://evil.example"},
	} {
		resp := h.do(http.MethodPost, "/bff/api/clusters", map[string]string{"name": "x"}, headers...)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}
	assert.Empty(t, h.upstreamRequests())

	for _, tt := range tests {
		t.Run(tt.name+" proxied", func(t *testing.T) {
			if tt.wantStatus != http.StatusForbidden {
				t.Skip()
			}
			resp := h.do(http.MethodDelete, "/bff/api/clusters/x", nil, tt.headers...)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantACAO, resp.Header.Get("Access-Control-Allow-Origin"))
		})
	}

	t.Run("preflight from a trusted origin", func(t *testing.T) {
		resp := h.do(http.MethodOptions, "/bff/api/clusters", nil,
			"Origin", devOrigin, "Sec-Fetch-Site", "cross-site",
			"Access-Control-Request-Method", "DELETE", "Access-Control-Request-Headers", "content-type")
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, devOrigin, resp.Header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
		assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Content-Type")
	})
	t.Run("preflight from another origin", func(t *testing.T) {
		resp := h.do(http.MethodOptions, "/bff/api/clusters", nil,
			"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site", "Access-Control-Request-Method", "DELETE")
		assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	})
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, nil)
	assert.Equal(t, http.StatusNoContent, h.do(http.MethodGet, "/bff/healthz", nil).StatusCode)
}

func TestRemoveExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{
		"session_old":   2 * time.Hour,
		"session_fresh": time.Minute,
		"other_old":     2 * time.Hour,
	} {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, nil, 0o600))
		require.NoError(t, os.Chtimes(p, now.Add(-age), now.Add(-age)))
	}
	removeExpired(dir, time.Hour, now, slog.New(slog.NewTextHandler(io.Discard, nil)))

	left, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	var names []string
	for _, l := range left {
		names = append(names, filepath.Base(l))
	}
	assert.ElementsMatch(t, []string{"session_fresh", "other_old"}, names)
}
