# Token Handler

A backend for a single page application. It signs users in with OpenID Connect, keeps their tokens in a server-side session and proxies the application's API calls with the access token added. The browser only ever holds an HttpOnly session cookie, so a script running on the page cannot read a token.

Run it on the application's own host under a path prefix such as `/bff`. The browser then talks to one origin, and the APIs behind it need no CORS.

## How the application uses it

All paths are relative to `basePath`.

| Request | Does |
| :--- | :--- |
| `POST /auths` with `{"idp": "netic", "path": "/clusters"}` | Starts a login. Returns `authorizationUrl`; send the browser there. `path` is handed back after login and must be a local path. |
| `PUT /auths/{state}` with `{"code": "...", "iss": "..."}` | Completes the login from the callback page, with `state`, `code` and, if the provider sends it, `iss` from the callback URL. Returns the session `id` and `path`. |
| `GET /auths` | Lists the current login, if any. |
| `DELETE /auths/{id}` | Logs out. The refresh token is revoked, and the response's `logoutUrl` ends the session at the provider; send the browser there. |
| `GET /healthz` | Liveness, without authentication. |
| Anything under an upstream's `path` | Proxied with `Authorization: Bearer <access token>`. Returns 401 without a live session. |

## Security behaviour

- **Cookies.** `__Host-token` refers to the session and `__Host-state_*` carries a login in progress. Both are `Secure`, `HttpOnly`, `SameSite=Strict` and `Path=/`, and the `__Host-` prefix stops a sibling host from planting one. Every login gets a new session ID.
- **Cross-site requests.** Unsafe methods are refused unless they come from the same origin or a configured origin, using Go's `http.CrossOriginProtection`. A page on another host of the same site, such as another `*.example.com` application, is refused too. Only configured origins get CORS headers.
- **Tokens.** The ID token is verified, nonce included, and so is `iss` when the application passes it on (RFC 9207). An expired access token is refreshed once per session, however many requests arrive at the same time, and the new tokens are saved. A failed refresh ends the session. A session ends after `sessionMaxAge` whatever the provider allows.
- **Scopes.** `openid` plus the configured `scopes`. `offline_access` is not requested unless you add it, so a session cannot outlive the user's session at the provider.
- **Proxying.** Only a fixed list of request headers reaches the upstream (see `internal/proxy`); cookies and `X-Forwarded-*` do not. `Set-Cookie` and `Access-Control-*` from the upstream are dropped. Paths with dot segments or encoded slashes or backslashes are refused, and so are protocol upgrades such as WebSockets.
- **Disk.** Logins in progress live only in their encrypted cookie, so unauthenticated requests write nothing to disk. Request bodies are capped at 16 KiB and sessions at 32 KiB. Session files older than `sessionMaxAge` are removed every ten minutes.
- **Errors** return a short message. The detail goes to the log, which never contains tokens or query strings.

## Sessions and replicas

Sessions are files in `sessionPath`, signed with `HASH_KEY` and encrypted with `BLOCK_KEY`. That suits one replica. A second replica needs the same directory on shared storage, or sticky sessions. Losing the directory, for example on a restart without a volume, logs everyone out. A size-limited `emptyDir` is enough for a single replica.

## Configuration

Configuration comes from `config.yaml` in the working directory, the file given with `-c`, flags and environment variables. Providers and upstreams can only be set in the file. `${VAR}` in a string is replaced from the environment, which is how the client secret gets in. See [config.sample.yaml](config.sample.yaml).

| Key | Environment | Meaning |
| :--- | :--- | :--- |
| `listenAddr` | `LISTEN_ADDRESS` | Default `:8080`. |
| `basePath` | `BASE_PATH` | The prefix the browser uses, such as `/bff`. Empty for the root. |
| `origins` | `ORIGINS` | Browser origins allowed to call the handler, comma separated in the environment. Include the application's own origin. Required. |
| `redirectURL` | `REDIRECT_URL` | The application's callback page, registered with the provider. Required. |
| `postLogoutRedirectURL` | `POST_LOGOUT_REDIRECT_URL` | Where the provider sends the browser after logout. Register it with the provider. |
| `hashKey` | `HASH_KEY` | Base64, at least 32 bytes. Required. |
| `blockKey` | `BLOCK_KEY` | Base64, exactly 32 bytes. Required. |
| `sessionPath` | `SESSION_PATH` | Directory for session files. Required. |
| `sessionMaxAge` | `SESSION_MAX_AGE` | Absolute session lifetime. Default `10h`, Keycloak's default SSO session maximum. |
| `logLevel` | `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. Default `info`. |
| `providers.<name>` | | `issuer`, `clientID`, `clientSecret`, `scopes`. Names are lowercased. |
| `upstreams[]` | | `path` (below `basePath`), `url`, and `stripPrefix` to remove the path before forwarding. |

Generate keys with `openssl rand -base64 64` and `openssl rand -base64 32`. Changing either key logs everyone out.

The provider needs a confidential client with the standard flow and PKCE S256, `redirectURL` as a redirect URI, and `postLogoutRedirectURL` as a post-logout redirect URI.

## Development

```sh
go test -race ./...
golangci-lint run
```

The tests run the whole flow against a fake OpenID Connect provider, including concurrent refreshes, logout and the cross-site cases.

## Releases

Pushing a tag `vX.Y.Z` builds `ghcr.io/neticdk/go-token-handler` for amd64 and arm64 through `neticdk/shared-github-actions`, with SLSA build provenance and a keyless cosign signature, and creates the GitHub release.
