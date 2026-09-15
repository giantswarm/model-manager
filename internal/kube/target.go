package kube

import (
	"fmt"
	"log/slog"

	"k8s.io/client-go/rest"
)

// NewForTarget builds clients toward another cluster's apiserver with no
// credential of their own: every call goes through For(ctx) with the
// caller's token, which the target apiserver must trust (the cluster chart's
// OIDC / structuredAuthentication values name the installation's Dex). A
// call without a caller token is anonymous there and refused by the target.
func NewForTarget(apiServer string, caBundle []byte, log *slog.Logger) (*Clients, error) {
	if apiServer == "" {
		return nil, fmt.Errorf("target apiserver is empty")
	}
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("target CA bundle is empty")
	}
	cfg := &rest.Config{Host: apiServer, TLSClientConfig: rest.TLSClientConfig{CAData: caBundle}, UserAgent: rest.DefaultKubernetesUserAgent()}
	c, err := fromRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", apiServer, err)
	}
	if log != nil {
		c.log = log
	}
	return c, nil
}
