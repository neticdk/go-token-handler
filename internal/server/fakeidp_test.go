package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// fakeIdP is a minimal OpenID Connect provider for tests.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer

	clientID, clientSecret string

	mu            sync.Mutex
	codes         map[string]pendingCode
	refreshTokens map[string]bool // valid refresh tokens
	revoked       []string
	refreshes     int
	// firstExpiresIn is the lifetime of tokens from a code exchange.
	firstExpiresIn int
	// nonceOverride replaces the nonce in ID tokens when set.
	nonceOverride string
}

type pendingCode struct {
	challenge string
	nonce     string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	require.NoError(t, err)

	f := &fakeIdP{
		t:              t,
		key:            key,
		signer:         signer,
		clientID:       "inventory",
		clientSecret:   "s3cret",
		codes:          map[string]pendingCode{},
		refreshTokens:  map[string]bool{},
		firstExpiresIn: 3600,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("POST /revoke", f.revoke)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) issuer() string { return f.srv.URL }

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                f.issuer(),
		"authorization_endpoint":                f.issuer() + "/auth",
		"token_endpoint":                        f.issuer() + "/token",
		"jwks_uri":                              f.issuer() + "/jwks",
		"end_session_endpoint":                  f.issuer() + "/logout",
		"revocation_endpoint":                   f.issuer() + "/revoke",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *fakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

// authorize records a code as if the user had signed in at the provider.
func (f *fakeIdP) authorize(code, challenge, nonce string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = pendingCode{challenge: challenge, nonce: nonce}
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != f.clientID || secret != f.clientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	expiresIn := 3600
	nonce := ""
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		pc, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != pc.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		nonce = pc.nonce
		expiresIn = f.firstExpiresIn
	case "refresh_token":
		rt := r.PostFormValue("refresh_token")
		if !f.refreshTokens[rt] {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		delete(f.refreshTokens, rt) // rotate
		f.refreshes++
		// Slow enough for concurrent callers to pile up on one refresh.
		time.Sleep(50 * time.Millisecond)
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	if f.nonceOverride != "" {
		nonce = f.nonceOverride
	}

	rt := rand.Text()
	f.refreshTokens[rt] = true
	now := time.Now()
	idToken, err := jwt.Signed(f.signer).Claims(map[string]any{
		"iss":   f.issuer(),
		"aud":   f.clientID,
		"sub":   "user-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": nonce,
	}).Serialize()
	require.NoError(f.t, err)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "at-" + rand.Text(),
		"token_type":    "Bearer",
		"refresh_token": rt,
		"id_token":      idToken,
		"expires_in":    expiresIn,
	})
}

func (f *fakeIdP) revoke(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := r.PostFormValue("token")
	f.revoked = append(f.revoked, rt)
	delete(f.refreshTokens, rt)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeIdP) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

func (f *fakeIdP) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}
