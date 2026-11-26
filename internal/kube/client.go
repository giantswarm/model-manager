// Package kube builds the Kubernetes clients model-manager uses for agent
// wiring and the kserve driver: in-cluster when running as a pod, kubeconfig
// (+ context) otherwise. With downstream OAuth on, a request that carries the
// caller's IdP token gets clients that present that token instead — the
// caller's RBAC governs what the request may do, and the ServiceAccount holds
// no permissions of its own (the chart renders no RBAC for it).
package kube

import (
	"context"
	"fmt"
	"os"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	toolkitk8s "github.com/giantswarm/mcp-toolkit/k8s"

	"github.com/giantswarm/model-manager/internal/requester"
)

// Factory selects explicitly configured application clients or caller clients.
type Factory struct {
	*toolkitk8s.Clients
	users *toolkitk8s.UserClients
}

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
}

// New builds the clients.
func New(cfg Config) (*Factory, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "model-manager"
	return fromRESTConfig(restCfg)
}

func fromRESTConfig(restCfg *rest.Config) (*Factory, error) {
	restCfg = rest.CopyConfig(restCfg)
	if restCfg.QPS == 0 {
		restCfg.QPS = 100
	}
	if restCfg.Burst == 0 {
		restCfg.Burst = 200
	}
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
	users, err := toolkitk8s.NewUserClients(restCfg, restCfg.WrapTransport)
	if err != nil {
		return nil, err
	}
	return &Factory{Clients: &toolkitk8s.Clients{Dynamic: dyn, Discovery: disc, Clientset: cs}, users: users}, nil
}

// For selects caller clients when a token is present, including after expiry.
// Calls without a token use the explicitly configured base clients.
func (c *Factory) For(ctx context.Context) (*toolkitk8s.Clients, error) {
	token, ok := requester.IDTokenFromContext(ctx)
	if !ok {
		return c.Clients, nil
	}
	clients, err := c.users.ForToken(token)
	if err != nil {
		return nil, fmt.Errorf("caller-token Kubernetes clients: %w", err)
	}
	return clients, nil
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
