package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/neticdk/go-token-handler/pkg/pkce"
	"github.com/stretchr/testify/assert"
	"golang.org/x/oauth2"
)

func TestCreateAuth(t *testing.T) {
	e := echo.New()

	mw := session.Middleware(&sessionStoreMock{Sessions: map[string]*sessions.Session{}})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"idp": "myidp"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	c := e.NewContext(req, rec)
	mw(echo.NotFoundHandler)(c)

	auth := &auth{
		providers: map[string]*oauth2.Config{
			"myidp": {
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				Endpoint: oauth2.Endpoint{
					AuthURL: "http://localhost",
				},
			},
		},
	}

	err := auth.createAuth(c)
	if assert.NoError(t, err) {
		assert.Equal(t, http.StatusCreated, rec.Code)

		data := map[string]string{}
		assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &data))
		assert.Equal(t, "myidp", data["idp"])

		url, err := url.Parse(data["authorizationUrl"])
		assert.NoError(t, err)
		assert.Equal(t, "client-id", url.Query().Get("client_id"))
		assert.Equal(t, "code", url.Query().Get("response_type"))
		assert.Equal(t, "S256", url.Query().Get("code_challenge_method"))
	}
}

func TestUpdateAuth(t *testing.T) {
	e := echo.New()
	state := uuid.NewString()

	sStore := &sessionStoreMock{
		Sessions: map[string]*sessions.Session{},
	}
	mw := session.Middleware(sStore)
	ses := sessions.NewSession(sStore, state)
	sStore.Sessions[state] = ses

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1073741824)
		v := r.Form
		assert.Len(t, v, 3)
		assert.Equal(t, "authorization_code", v.Get("grant_type"))
		assert.Equal(t, "auth-code", v.Get("code"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"access_token":"xyz","token_type":"Bearer"}`))
	}))
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"idp": "myidp","code":"auth-code"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	c := e.NewContext(req, rec)
	c.SetPath("/auths/:state")
	c.SetParamNames("state")
	c.SetParamValues(state)
	mw(echo.NotFoundHandler)(c)

	auth := &auth{
		providers: map[string]*oauth2.Config{
			"myidp": {
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				Endpoint: oauth2.Endpoint{
					TokenURL: server.URL,
				},
			},
		},
	}

	v, _ := pkce.NewCodeVerifier()
	ses.Values[authState] = authentication{
		Idp:      "myidp",
		Verifier: v,
	}

	err := auth.updateAuth(c)
	if assert.NoError(t, err) {
		assert.Equal(t, http.StatusOK, rec.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	e := echo.New()

	sStore := &sessionStoreMock{
		Sessions: map[string]*sessions.Session{},
	}
	mw := session.Middleware(sStore)
	ses := sessions.NewSession(sStore, CookieTokenName)
	sStore.Sessions[CookieTokenName] = ses

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"idp": "myidp"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	c := e.NewContext(req, rec)
	mw(echo.NotFoundHandler)(c)

	auth := &auth{
		providers: map[string]*oauth2.Config{
			"myidp": {
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				Endpoint: oauth2.Endpoint{
					TokenURL: "server.URL",
				},
			},
		},
	}

	ses.Values[sessionKeyToken] = oauth2.Token{AccessToken: "xyz", TokenType: "Bearer"}
	ses.Values[sessionKeyProvider] = "myidp"

	f := auth.AuthMiddleware()
	h := f(func(c echo.Context) error { return nil })
	if assert.NoError(t, h(c)) {
		assert.True(t, true)
		assert.Equal(t, "Bearer xyz", req.Header.Get("Authorization"))
	}
}

type sessionStoreMock struct {
	Sessions map[string]*sessions.Session
}

func (s *sessionStoreMock) Get(r *http.Request, name string) (*sessions.Session, error) {
	if ses, ok := s.Sessions[name]; ok {
		return ses, nil
	}
	return sessions.NewSession(s, name), nil
}

func (s *sessionStoreMock) New(r *http.Request, name string) (*sessions.Session, error) {
	if ses, ok := s.Sessions[name]; ok {
		return ses, nil
	}
	return sessions.NewSession(s, name), nil
}

func (*sessionStoreMock) Save(r *http.Request, w http.ResponseWriter, s *sessions.Session) error {
	return nil
}
