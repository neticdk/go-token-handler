package auth

import (
	"encoding/gob"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/neticdk/go-token-handler/pkg/pkce"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
)

const (
	CookieTokenName = "token"

	sessionKeyID       = "id"
	sessionKeyProvider = "provider"
	sessionKeyToken    = "token"

	authState = "state"
)

func RegisterAuthEndpoint(e *echo.Echo, hashKey, blockKey []byte, providers map[string]*oauth2.Config, origins []string) echo.MiddlewareFunc {
	a := &auth{
		providers: providers,
	}

	g := e.Group("/auths", middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     origins,
		AllowMethods:     []string{http.MethodPut, http.MethodPost},
		AllowCredentials: true,
	}))

	g.OPTIONS("", func(c echo.Context) error { _ = c.NoContent(http.StatusNoContent); return nil })
	g.GET("", a.listAuth)
	g.POST("", a.createAuth)
	g.PUT("/:state", a.updateAuth)
	g.DELETE("/:state", a.delete)

	return a.AuthMiddleware()
}

type auth struct {
	providers map[string]*oauth2.Config
}

type authentication struct {
	Idp      string
	Verifier *pkce.CodeVerifier
	Path     string
}

type AuthResource struct {
	ID               string `json:"id,omitempty"`
	Idp              string `json:"idp"`
	Path             string `json:"path,omitempty"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	Code             string `json:"code,omitempty"`
}

func (a *auth) AuthMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			s, err := session.Get(CookieTokenName, c)
			if err != nil {
				_ = c.NoContent(http.StatusInternalServerError)
				return fmt.Errorf("unable to get session: %w", err)
			}

			token, ok := s.Values[sessionKeyToken].(oauth2.Token)
			if !ok {
				_ = c.NoContent(http.StatusUnauthorized)
				return fmt.Errorf("unable to get token from session")
			}

			idp, ok := s.Values[sessionKeyProvider].(string)
			if !ok {
				_ = c.NoContent(http.StatusInternalServerError)
				return fmt.Errorf("unable to find idp in session")
			}
			p, ok := a.providers[idp]
			if !ok {
				_ = c.NoContent(http.StatusInternalServerError)
				return fmt.Errorf("unable to find idp %s in configured providers", idp)
			}

			ts := p.TokenSource(c.Request().Context(), &token)
			t, err := ts.Token()
			if err != nil {
				_ = c.NoContent(http.StatusUnauthorized)
				return fmt.Errorf("unable to get access token: %w", err)
			}
			t.SetAuthHeader(c.Request())

			return next(c)
		}
	}
}

func init() {
	gob.Register(authentication{})
	gob.Register(oauth2.Token{})
}

func (a *auth) createAuth(c echo.Context) error {
	payload := AuthResource{}
	err := c.Bind(&payload)
	if err != nil {
		return fmt.Errorf("unable to parse payload: %w", err)
	}

	codeVerifier, err := pkce.NewCodeVerifier()
	if err != nil {
		return fmt.Errorf("unable to create code verifier: %w", err)
	}

	au := authentication{
		Verifier: codeVerifier,
		Idp:      payload.Idp,
		Path:     payload.Path,
	}

	state := uuid.New().String()
	s, err := session.Get(state, c)
	if err != nil {
		return err
	}
	s.Options = &sessions.Options{
		Secure:   true,
		MaxAge:   300,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Path:     "/auths",
	}
	s.Values[authState] = au
	err = s.Save(c.Request(), c.Response())
	if err != nil {
		return fmt.Errorf("unable to store authetication session: %w", err)
	}

	p, ok := a.providers[au.Idp]
	if !ok {
		return fmt.Errorf("unable to find idp: %s", au.Idp)
	}

	payload.AuthorizationURL = p.AuthCodeURL(state, au.Verifier.CodeChallengeOptions()...)

	err = c.JSON(http.StatusCreated, payload)
	if err != nil {
		return err
	}

	return nil
}

func (a *auth) updateAuth(c echo.Context) error {
	payload := AuthResource{}
	err := c.Bind(&payload)
	if err != nil {
		return err
	}

	state := c.Param("state")
	s, err := session.Get(state, c)
	if err != nil {
		return fmt.Errorf("unable to retrieve authentication session %s: %w", state, err)
	}

	au, ok := s.Values[authState].(authentication)
	if !ok {
		return fmt.Errorf("unable to read back authentication information from session")
	}

	p, ok := a.providers[au.Idp]
	if !ok {
		return fmt.Errorf("unable to find idp: %s", au.Idp)
	}

	token, err := p.Exchange(c.Request().Context(), payload.Code, au.Verifier.VerifierOptions()...)
	if err != nil {
		return fmt.Errorf("unable to retrive access token from identity provider: %w", err)
	}
	logger := zerolog.Ctx(c.Request().Context())
	logger.Trace().Str("type", token.Type()).Msg("received token from idp")

	ts, err := session.Get(CookieTokenName, c)
	if err != nil {
		return fmt.Errorf("unable to get session for token: %w", err)
	}
	ts.Options = &sessions.Options{
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Path:     "/",
		MaxAge:   86400 * 30,
	}
	ts.Values[sessionKeyID] = state
	ts.Values[sessionKeyToken] = token
	ts.Values[sessionKeyProvider] = au.Idp
	err = ts.Save(c.Request(), c.Response())
	if err != nil {
		return fmt.Errorf("unable to save token session cookie: %w", err)
	}

	s.Options = &sessions.Options{MaxAge: -1}
	err = s.Save(c.Request(), c.Response())
	if err != nil {
		return fmt.Errorf("unable to remove auth session: %w", err)
	}

	err = c.JSON(http.StatusOK, AuthResource{
		Idp:  au.Idp,
		Path: au.Path,
	})
	if err != nil {
		return err
	}

	return nil
}

func (a *auth) listAuth(c echo.Context) error {
	type list struct {
		Count    int           `json:"count"`
		Auths    []string      `json:"auths"`
		Included []interface{} `json:"@included,omitempty"`
	}

	s, err := session.Get(CookieTokenName, c)
	if err != nil {
		_ = c.NoContent(http.StatusInternalServerError)
		return fmt.Errorf("unable to get session: %w", err)
	}

	l := &list{
		Count: 0,
		Auths: []string{},
	}

	id, ok := s.Values[sessionKeyID].(string)
	if !ok {
		return c.JSON(http.StatusOK, l)
	}

	idp, ok := s.Values[sessionKeyProvider].(string)
	if !ok {
		return c.JSON(http.StatusOK, l)
	}

	l.Count = 1
	l.Auths = append(l.Auths, "id")
	l.Included = []interface{}{
		&AuthResource{
			ID:  id,
			Idp: idp,
		},
	}

	return c.JSON(http.StatusOK, l)
}

func (a *auth) delete(c echo.Context) error {
	state := c.Param("state")

	s, err := session.Get(CookieTokenName, c)
	if err != nil {
		_ = c.NoContent(http.StatusInternalServerError)
		return fmt.Errorf("unable to get session: %w", err)
	}

	id, ok := s.Values[sessionKeyID].(string)
	if !ok {
		return c.NoContent(http.StatusNotFound)
	}

	if id != state {
		return c.NoContent(http.StatusNotFound)
	}

	s.Options = &sessions.Options{MaxAge: -1}
	err = s.Save(c.Request(), c.Response())
	if err != nil {
		return fmt.Errorf("unable to remove auth session: %w", err)
	}

	return c.NoContent(http.StatusNoContent)
}
