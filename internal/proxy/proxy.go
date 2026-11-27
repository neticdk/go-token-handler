// Package proxy forwards browser requests to an upstream API without the
// browser's own credentials.
package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// RequestHeaders are the request headers passed to an upstream. Anything
// else, cookies and forwarding headers included, stays behind. The caller
// sets Authorization.
var RequestHeaders = []string{
	"Accept",
	"Accept-Language",
	"Authorization",
	"Cache-Control",
	"Content-Language",
	"Content-Type",
	"If-Match",
	"If-Modified-Since",
	"If-None-Match",
	"If-Unmodified-Since",
	"Prefer",
	"Range",
	"Traceparent",
	"Tracestate",
	"User-Agent",
	"X-Request-Id",
}

// Upstream is an API reached through the handler.
type Upstream struct {
	// Path is the prefix the upstream is mounted at, such as "/bff/api".
	Path string
	// URL is where requests go.
	URL *url.URL
	// StripPrefix removes Path before forwarding, so "/bff/api/clusters"
	// reaches the upstream as "/clusters".
	StripPrefix bool
}

// New returns a reverse proxy to u.
func New(u Upstream, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			h := http.Header{}
			for _, k := range RequestHeaders {
				if v := pr.In.Header.Values(k); len(v) > 0 {
					h[k] = v
				}
			}
			pr.Out.Header = h
			if u.StripPrefix {
				p := strings.TrimPrefix(pr.In.URL.EscapedPath(), u.Path)
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				pr.Out.URL.RawPath = p
				if unescaped, err := url.PathUnescape(p); err == nil {
					pr.Out.URL.Path = unescaped
				}
			}
			pr.SetURL(u.URL)
		},
		ModifyResponse: func(resp *http.Response) error {
			// An upstream must not set cookies on the application's origin
			// or widen its CORS policy.
			for k := range resp.Header {
				if k == "Set-Cookie" || strings.HasPrefix(k, "Access-Control-") {
					resp.Header.Del(k)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.ErrorContext(r.Context(), "upstream request failed", "upstream", u.URL.Redacted(), "error", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !confined(r.URL) {
			http.Error(w, "path not allowed", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Upgrade") != "" {
			// A protocol upgrade would carry the session past the checks
			// that apply to ordinary requests.
			http.Error(w, "protocol upgrades are not supported", http.StatusBadRequest)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// confined reports whether a path stays below its prefix once the
// upstream decodes it: no dot segments, no encoded slashes or backslashes.
func confined(u *url.URL) bool {
	lower := strings.ToLower(u.EscapedPath())
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(u.Path, "\\") {
		return false
	}
	for seg := range strings.SplitSeq(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
