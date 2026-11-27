package server

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func validConfig() Config {
	return Config{
		BasePath:      "/bff",
		Origins:       []string{"https://inventory.example"},
		RedirectURL:   "https://inventory.example/callback",
		HashKey:       base64.StdEncoding.EncodeToString(make([]byte, 64)),
		BlockKey:      base64.StdEncoding.EncodeToString(make([]byte, 32)),
		SessionPath:   "/tmp/sessions",
		SessionMaxAge: time.Hour,
		Providers: map[string]ProviderConfig{
			"netic": {Issuer: "https://idp.example", ClientID: "c", ClientSecret: "s"},
		},
		Upstreams: []UpstreamConfig{{Path: "/api", URL: "http://api:8080", StripPrefix: true}},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"short hash key", func(c *Config) { c.HashKey = base64.StdEncoding.EncodeToString(make([]byte, 16)) }, "hash key"},
		{"wrong block key size", func(c *Config) { c.BlockKey = base64.StdEncoding.EncodeToString(make([]byte, 16)) }, "block key"},
		{"no origins", func(c *Config) { c.Origins = nil }, "origins"},
		{"origin with path", func(c *Config) { c.Origins = []string{"https://a.example/x"} }, "origin"},
		{"base path with trailing slash", func(c *Config) { c.BasePath = "/bff/" }, "basePath"},
		{"no session path", func(c *Config) { c.SessionPath = "" }, "sessionPath"},
		{"no max age", func(c *Config) { c.SessionMaxAge = 0 }, "sessionMaxAge"},
		{"no providers", func(c *Config) { c.Providers = nil }, "provider"},
		{"provider without secret", func(c *Config) {
			c.Providers["netic"] = ProviderConfig{Issuer: "https://idp.example", ClientID: "c"}
		}, "clientSecret"},
		{"relative upstream URL", func(c *Config) { c.Upstreams[0].URL = "api:8080" }, "upstream url"},
		{"upstream on the login path", func(c *Config) { c.Upstreams[0].Path = "/auths" }, "collides"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.True(t, strings.Contains(err.Error(), tt.wantErr), err.Error())
			}
		})
	}
}
