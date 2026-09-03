// Package kube builds the Kubernetes clients model-manager uses for agent
// wiring and the kserve driver: in-cluster when running as a pod, kubeconfig
// (+ context) otherwise. With downstream OAuth on, a request that carries the
// caller's IdP token gets clients that present that token instead — the
// caller's RBAC governs what the request may do.
package kube

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/model-manager/internal/identity"
)

// Clients bundles what the wiring layer and the kserve driver need.
type Clients struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	// Clientset is the typed client (Jobs, Pods, Nodes, ConfigMaps, ...).
	Clientset kubernetes.Interface

	restCfg *rest.Config
	log     *slog.Logger

	mu     sync.Mutex
	byUser map[[32]byte]*callerEntry
}

// callerEntry caches the clients built for one caller token until it expires.
type callerEntry struct {
	clients *Clients
	expires time.Time
	// expiredLogged: the fallback to the ServiceAccount is logged once per
	// token, not on every call a long job makes after the token ran out.
	expiredLogged bool
}

// maxCallerClients bounds the per-token cache; entries expire with their token
// anyway, this only guards against a flood of distinct tokens.
const maxCallerClients = 256

// Config selects how to reach the API server.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path; empty uses the default
	// loading rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context.
	Context string
	// InCluster forces in-cluster auth. When false and no kubeconfig is
	// found, in-cluster auth is still tried if the pod environment is present.
	InCluster bool
	// Logger for the caller-token fallbacks (nil: slog.Default()).
	Logger *slog.Logger
}

// New builds the clients.
func New(cfg Config) (*Clients, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "model-manager"
	c, err := fromRESTConfig(restCfg)
	if err != nil {
		return nil, err
	}
	if cfg.Logger != nil {
		c.log = cfg.Logger
	}
	return c, nil
}

func fromRESTConfig(restCfg *rest.Config) (*Clients, error) {
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &Clients{Dynamic: dyn, Discovery: disc, Clientset: cs, restCfg: restCfg, log: slog.Default(), byUser: map[[32]byte]*callerEntry{}}, nil
}

// ForToken returns clients that authenticate to the API server with token (an
// OIDC id_token the apiserver trusts) instead of the ServiceAccount. Server
// address, CA and TLS settings are kept; every credential of the base config
// is dropped so nothing but the caller's token is presented.
func (c *Clients) ForToken(token string) (*Clients, error) {
	if token == "" {
		return nil, fmt.Errorf("caller token is empty")
	}
	cfg := rest.AnonymousClientConfig(c.restCfg)
	cfg.BearerToken = token
	cfg.UserAgent = c.restCfg.UserAgent
	user, err := fromRESTConfig(cfg)
	if err != nil {
		return nil, err
	}
	user.log = c.log
	return user, nil
}

// For returns the clients a request should use: the caller's when ctx carries
// a still-valid caller token (downstream OAuth), else the ServiceAccount's.
// A token that has expired since the request came in — a job following a
// download for longer than the token lives — falls back to the ServiceAccount,
// logged once; the operation stays attributed to the caller on the job.
func (c *Clients) For(ctx context.Context) *Clients {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return c
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	e, cached := c.byUser[key]
	if !cached {
		if len(c.byUser) >= maxCallerClients {
			c.evictLocked(now)
		}
		clients, err := c.ForToken(token)
		if err != nil {
			c.log.Warn("caller-token Kubernetes clients unavailable, using the ServiceAccount", "error", err, identity.LogAttr(ctx))
			return c
		}
		e = &callerEntry{clients: clients, expires: identity.TokenExpiry(token)}
		c.byUser[key] = e
	}
	if !e.expires.IsZero() && now.After(e.expires) {
		if !e.expiredLogged {
			e.expiredLogged = true
			c.log.Info("caller token expired; continuing as the ServiceAccount", identity.LogAttr(ctx), "expired", e.expires.UTC().Format(time.RFC3339))
		}
		return c
	}
	return e.clients
}

// evictLocked drops expired entries, and when nothing expired the whole cache
// (a token flood is not worth an LRU).
func (c *Clients) evictLocked(now time.Time) {
	for k, e := range c.byUser {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(c.byUser, k)
		}
	}
	if len(c.byUser) >= maxCallerClients {
		c.byUser = map[[32]byte]*callerEntry{}
	}
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.InCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return c, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return c, nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if ic, icErr := rest.InClusterConfig(); icErr == nil {
			return ic, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig: %w", err)
}
