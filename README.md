# Token Handler

This project is basically a generic BFF designed to keep OAuth 2.0 access token and other
sensitive information such as refresh token, server side to reduce the risk of leaking.

The service exposes a simple REST API to allow clients to start an authentication flow and
when authentication is done it allows for proxying requests to backend services adding
OAuth 2.0 bearer token in authorization header.

## Configuration

Most configuration options are available in both configuration file, command line flags and
environment variables. However, configuration of identity providers and upstream servers may
only be done through the configuration file. The configuration file supports substitution of
environment variables for the identity providers such that client secrets may be passed
through environment variables.

The configuration file has the following format.

```yaml
listenAddr: ':8081'
hashKey: 'bCXgBjNPIeAUDzTYKf4E2xXNZaznkyTjQT7zh/UXJcz3CsPMu3FFoxG4WqcQY3foPmKtAdexMLXJ5L3vJkn1og=='
blockKey: 'Cl/c1FWNiCDp32/FhpGgzgqUIcLdYScHa+AiLG2gWFI='
providers:
  netic:
    clientID: inventory
    clientSecret: ${NETIC_SECRET}
    issuer: http://localhost:8080/realms/test
upstreams:
  api: http://localhost:8086
origins:
  - http://localhost:3000
redirectURL: ''
```
