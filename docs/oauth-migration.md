# OAuth configuration migration

Remove `--allow-private-oauth-urls`, `MODEL_MANAGER_OAUTH_ALLOW_PRIVATE_URLS` and `oauth.dex.allowPrivateURLs` from your configuration. Setting them to true is refused with an actionable error. False remains accepted during migration, and an explicit CLI flag takes precedence over its environment fallback.

Dex discovery and token requests use mcp-oauth's default HTTP client, which reaches private addresses and honors `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`. Use `--dex-ca-file` or `oauth.dex.caSecret` for an internal CA. TLS verification remains enabled.

The planned removal target for the obsolete flag, environment variable and chart value is model-manager v1.21.0, with a migration deadline of 2027-02-17.

The independent `--sso-allow-private-ips` setting applies to forwarded-ID-token JWKS fetching and is unchanged.
