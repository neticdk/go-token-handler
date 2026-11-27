// Package server wires the login endpoints, the session store and the
// upstream proxies into one HTTP server.
package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is the handler's configuration.
type Config struct {
	ListenAddr string `mapstructure:"listenAddr"`
	// BasePath is the prefix the browser reaches the handler at, such as
	// "/bff". Empty means the root.
	BasePath string `mapstructure:"basePath"`
	// Origins are the browser origins allowed to call the handler: the
	// application's own origin, and any other that needs CORS.
	Origins []string `mapstructure:"origins"`
	// RedirectURL is the application's callback page, registered with the
	// identity provider.
	RedirectURL string `mapstructure:"redirectURL"`
	// PostLogoutRedirectURL is where the provider sends the browser after
	// logout.
	PostLogoutRedirectURL string `mapstructure:"postLogoutRedirectURL"`
	// HashKey signs and BlockKey encrypts the session data, both base64.
	HashKey  string `mapstructure:"hashKey"`
	BlockKey string `mapstructure:"blockKey"`
	// SessionPath is the directory session files are kept in.
	SessionPath string `mapstructure:"sessionPath"`
	// SessionMaxAge is the absolute lifetime of a session.
	SessionMaxAge time.Duration             `mapstructure:"sessionMaxAge"`
	Providers     map[string]ProviderConfig `mapstructure:"providers"`
	Upstreams     []UpstreamConfig          `mapstructure:"upstreams"`
}

// ProviderConfig is an OpenID Connect provider.
type ProviderConfig struct {
	Issuer       string   `mapstructure:"issuer"`
	ClientID     string   `mapstructure:"clientID"`
	ClientSecret string   `mapstructure:"clientSecret"`
	Scopes       []string `mapstructure:"scopes"`
}

// UpstreamConfig is an API proxied under BasePath + Path.
type UpstreamConfig struct {
	Path        string `mapstructure:"path"`
	URL         string `mapstructure:"url"`
	StripPrefix bool   `mapstructure:"stripPrefix"`
}

// keys decodes and checks the session keys.
func (c Config) keys() (hash, block []byte, err error) {
	hash, err = base64.StdEncoding.DecodeString(c.HashKey)
	if err != nil {
		return nil, nil, fmt.Errorf("hash key: %w", err)
	}
	if len(hash) < 32 {
		return nil, nil, fmt.Errorf("hash key must be at least 32 bytes, got %d", len(hash))
	}
	block, err = base64.StdEncoding.DecodeString(c.BlockKey)
	if err != nil {
		return nil, nil, fmt.Errorf("block key: %w", err)
	}
	if len(block) != 32 {
		return nil, nil, fmt.Errorf("block key must be 32 bytes for AES-256, got %d", len(block))
	}
	return hash, block, nil
}

// Validate reports every problem with the configuration at once.
func (c Config) Validate() error {
	var errs []error
	if _, _, err := c.keys(); err != nil {
		errs = append(errs, err)
	}
	if c.BasePath != "" && (!strings.HasPrefix(c.BasePath, "/") || strings.HasSuffix(c.BasePath, "/")) {
		errs = append(errs, fmt.Errorf("basePath %q must start with / and not end with /", c.BasePath))
	}
	if len(c.Origins) == 0 {
		errs = append(errs, errors.New("origins: at least the application's own origin is required"))
	}
	for _, o := range c.Origins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			errs = append(errs, fmt.Errorf("origin %q must be scheme://host[:port]", o))
		}
	}
	if c.RedirectURL == "" {
		errs = append(errs, errors.New("redirectURL is required"))
	}
	if c.SessionPath == "" {
		errs = append(errs, errors.New("sessionPath is required"))
	}
	if c.SessionMaxAge <= 0 {
		errs = append(errs, errors.New("sessionMaxAge must be positive"))
	}
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("at least one provider is required"))
	}
	for name, p := range c.Providers {
		if p.Issuer == "" || p.ClientID == "" || p.ClientSecret == "" {
			errs = append(errs, fmt.Errorf("provider %s: issuer, clientID and clientSecret are required", name))
		}
	}
	for _, u := range c.Upstreams {
		if !strings.HasPrefix(u.Path, "/") || strings.HasSuffix(u.Path, "/") {
			errs = append(errs, fmt.Errorf("upstream path %q must start with / and not end with /", u.Path))
		}
		if pu, err := url.Parse(u.URL); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			errs = append(errs, fmt.Errorf("upstream url %q must be an absolute http or https URL", u.URL))
		}
		if u.Path == "/auths" || strings.HasPrefix(u.Path, "/auths/") || u.Path == "/healthz" {
			errs = append(errs, fmt.Errorf("upstream path %q collides with the handler's own endpoints", u.Path))
		}
	}
	return errors.Join(errs...)
}
