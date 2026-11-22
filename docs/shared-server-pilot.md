# Shared server pilot

Model-manager pins mcp-toolkit v0.5.5 and uses its released `httpx`, `health`
and `logging` packages. The application retains the mark3labs/mcp-go transport.
No shared package or SDK conversion representation is added.

## Compatibility

Existing flags, environment names, OAuth providers, custom CA handling,
trusted audiences and backend-independent readiness are retained. Healthy
`/readyz` responses contain `ready` rather than `ok`. Both probe paths accept
GET and HEAD and reject other methods with 405 and `Allow: GET, HEAD`.
Probe responses include `Cache-Control: no-store`.

On cancellation, readiness is revoked before HTTP draining starts. OAuth
resources remain available during the 15-second drain. A drain deadline causes
the application to close remaining HTTP connections and retain the deadline
error. OAuth cleanup then has a separate five-second budget. Closing connections
does not forcibly terminate handlers that ignore cancellation.

Logging retains text on stderr without OTLP configuration and honors `--verbose`.
With OTLP configured, the shared logger exports logs and mirrors JSON to stderr.
Logging initialization failures fail the command; logging flush runs after
command success or failure. Existing tracing and metrics flushes remain separate.
Their combined cleanup budgets and potentially unbounded job waiting need to be
considered against the deployment's termination grace period; this pilot does
not establish a bound on total process shutdown.

## Retained setup and protocol code

Every retained setup block is grouped below. These are the inputs to
[model-manager#437](https://github.com/giantswarm/model-manager/issues/437),
not additional shared-library features justified by this pilot alone.

| Retained block | Reason |
| --- | --- |
| `cmd/serve.go`: all option fields, flag registration, environment helpers and OAuthConfig construction | Cobra flags take precedence over established, mixed-prefix environment names. Released oauthconfig loaders read process environment directly and require different names. Adapting by mutating process environment would introduce global state and weaken this contract. |
| `cmd/root.go`: logging level, format, stderr mirror selection and flush | Application settings preserve verbosity and stderr visibility; the application owns the process shutdown order. |
| `internal/server/http.go`: Config, mux assembly, REST guard, MCP transport/body cap, tracing filter and HTTP timeout fields | Application routes, SDK registration and request limits are not responsibilities of the shared health or HTTP runner. |
| `internal/server/http.go`: backendz | Backend availability is distinct from whether model-manager can serve useful requests. |
| `internal/server/http.go`: readiness cancellation and cleanup sequencing | The application owns readiness, OAuth resource lifetime and connection closure policy; httpx intentionally owns only HTTP serving and graceful draining. |
| `internal/server/oauth.go`: OAuthConfig, validation, provider selection, newOAuth, serverConfig, loadRootCAs and validateHTTPS | Programmatic setup retains existing flags, Google/Dex selection, custom trust roots, CIMD default and explicit audiences. Released oauthconfig environment loaders cannot replace this setup without a configuration migration. Storage remains in-memory. |
| `internal/server/oauth.go`: route registration, protect, identity attachment, bearer extraction, downstream token lookup, challenges and shutdown | These contain application identity and downstream authorization behavior. Their audit findings and shared-helper adoption remain #437 work. |
| `internal/server/github_exchange.go` | Product-specific GitHub token exchange and commit identity are outside shared MCP setup. |

The pilot does not yet call oauthconfig and does not complete model-manager#436.
A compatible configuration migration must be designed and verified before that
criterion can be satisfied. No new loader interface is introduced in mcp-oauth.

Authenticated protocol tests exercise REST, MCP, challenges and resource
metadata through the assembled server. They retain the existing forwarded
ID-token audience behavior as a tracked deviation, not a conformance claim.
Installation rollout and existing external REST consumers still need separate
verification before closing the pilot.
