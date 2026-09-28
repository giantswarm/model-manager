package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/model-manager/internal/identity"
)

// GitHubPin makes the MCP endpoint the App-pinned registration muster
// connects with (MCPServer auth.authorizationServer pinned to the App
// model-manager, auth.forwardIdentity: true): the bearer is the
// person's user token of the App, verified with GET /user and kept for commit
// mode's pull request, and the person's IdP ID token arrives in
// ForwardedIdentityHeader. That token then goes through the same validation
// and the same Kubernetes-as-the-caller path a forwarded bearer takes without
// the pin, so apply mode is unchanged.
type GitHubPin struct {
	// AuthorizationServer is the App's issuer identity muster pins,
	// https://github.com/apps/model-manager: named in the
	// refusals.
	AuthorizationServer string
	// APIURL is the API base URL GET /user goes to (empty: api.github.com).
	APIURL string
	// CacheTTL bounds how long a verified bearer is trusted without asking
	// GitHub again (zero: DefaultGitHubCacheTTL).
	CacheTTL time.Duration
}

// ForwardedIdentityHeader is where muster puts the person's IdP ID token on
// every call to a registration with auth.forwardIdentity.
const ForwardedIdentityHeader = "X-Muster-Id-Token"

// DefaultGitHubAPIURL is GitHub's REST API.
const DefaultGitHubAPIURL = "https://api.github.com"

// DefaultGitHubCacheTTL is how long a verified bearer is trusted without a
// second GET /user: a user token's expiry is not readable from the token,
// and a revoked one must stop working soon.
const DefaultGitHubCacheTTL = 15 * time.Minute

// gitHubCacheMax bounds the verified-token cache.
const gitHubCacheMax = 10_000

// Validate checks required fields.
func (p GitHubPin) Validate() error {
	for what, raw := range map[string]string{"authorization server": p.AuthorizationServer, "API URL": p.apiURL()} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("github pin: %s must be an absolute http(s) URL: %q", what, raw)
		}
	}
	return nil
}

func (p GitHubPin) apiURL() string {
	if p.APIURL == "" {
		return DefaultGitHubAPIURL
	}
	return strings.TrimSuffix(p.APIURL, "/")
}

// gitHubGuard verifies the GitHub bearer and hands the forwarded ID token on
// as the bearer the IdP validation reads.
type gitHubGuard struct {
	pin  GitHubPin
	http *http.Client
	log  *slog.Logger

	mu    sync.Mutex
	cache map[[sha256.Size]byte]gitHubEntry
}

type gitHubEntry struct {
	login string
	until time.Time
}

func newGitHubGuard(pin GitHubPin, log *slog.Logger) (*gitHubGuard, error) {
	if err := pin.Validate(); err != nil {
		return nil, err
	}
	if pin.CacheTTL <= 0 {
		pin.CacheTTL = DefaultGitHubCacheTTL
	}
	log.Info("GitHub App pin enabled", "authorizationServer", pin.AuthorizationServer, "api", pin.apiURL(), "cacheTTL", pin.CacheTTL)
	return &gitHubGuard{pin: pin, http: &http.Client{Timeout: 10 * time.Second}, log: log, cache: map[[sha256.Size]byte]gitHubEntry{}}, nil
}

// signIn is the one way to a bearer this registration accepts.
func (g *gitHubGuard) signIn() string {
	return "connect model-manager in muster (core_auth_login server=model-manager, the consent of " + g.pin.AuthorizationServer + "), then call again"
}

// protect admits a request whose bearer GitHub accepts and that carries the
// forwarded ID token; next sees the ID token as the bearer and the GitHub
// identity on the context.
func (g *gitHubGuard) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			g.refuse(w, "", "no bearer token: "+g.signIn())
			return
		}
		idToken := strings.TrimSpace(r.Header.Get(ForwardedIdentityHeader))
		if idToken == "" {
			g.refuse(w, errInvalidToken, "no "+ForwardedIdentityHeader+" header: this registration acts on Kubernetes with your platform identity, which muster forwards only with auth.forwardIdentity: true on the MCPServer")
			return
		}
		login, status, err := g.verify(r.Context(), token)
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			g.refuse(w, errInvalidToken, fmt.Sprintf("GitHub refused the bearer token (%d): %s", status, g.signIn()))
			return
		case err != nil:
			g.log.Warn("GitHub bearer verification failed", "error", err)
			http.Error(w, "could not verify the bearer token with GitHub: "+err.Error(), http.StatusBadGateway)
			return
		}
		r = r.Clone(identity.ContextWithGitHub(r.Context(), &identity.GitHub{Login: login, Token: token}))
		r.Header.Set("Authorization", "Bearer "+idToken)
		r.Header.Del(ForwardedIdentityHeader)
		next.ServeHTTP(w, r)
	})
}

// verify is the login GET /user answers for token, from the cache while the
// entry lives; status is GitHub's answer when it refused the token.
func (g *gitHubGuard) verify(ctx context.Context, token string) (string, int, error) {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	g.mu.Lock()
	e, ok := g.cache[key]
	g.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.login, 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.pin.apiURL()+"/user", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("GET /user: %s", resp.Status)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil || user.Login == "" {
		return "", 0, fmt.Errorf("GET /user: no login in the answer")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.cache) >= gitHubCacheMax {
		for k, e := range g.cache {
			if !now.Before(e.until) {
				delete(g.cache, k)
			}
		}
	}
	if len(g.cache) < gitHubCacheMax {
		g.cache[key] = gitHubEntry{login: user.Login, until: now.Add(g.pin.CacheTTL)}
	}
	return user.Login, 0, nil
}

// refuse is the 401 with the RFC 6750 challenge and the reason as the body.
func (g *gitHubGuard) refuse(w http.ResponseWriter, code, description string) {
	challenge := `Bearer realm="model-manager"`
	if code != "" {
		challenge += fmt.Sprintf(`, error=%q, error_description=%q`, code, headerQuoted(description))
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, description, http.StatusUnauthorized)
}
