package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"

	"github.com/giantswarm/model-manager/internal/identity"
)

// NewForTarget builds clients toward another cluster's apiserver with no
// credential of their own: every call goes through For(ctx) with the
// caller's token, which the target apiserver must trust (the cluster chart's
// OIDC / structuredAuthentication values name the installation's Dex). A
// call without a caller token is anonymous there and refused by the target.
// A 401 or 403 the target answers carries the precondition the call missed
// (explainAuth), on every client these clients hand out.
func NewForTarget(apiServer string, caBundle []byte, log *slog.Logger) (*Clients, error) {
	if apiServer == "" {
		return nil, fmt.Errorf("target apiserver is empty")
	}
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("target CA bundle is empty")
	}
	wrap := explainAuth(apiServer)
	cfg := &rest.Config{Host: apiServer, TLSClientConfig: rest.TLSClientConfig{CAData: caBundle}, UserAgent: rest.DefaultKubernetesUserAgent(), WrapTransport: wrap}
	c, err := fromRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", apiServer, err)
	}
	c.wrap = wrap
	if log != nil {
		c.log = log
	}
	return c, nil
}

// explainAuth rewrites the Status of a 401 or 403 from the target apiserver so
// the error a tool call reports names what to fix — the target's trust in the
// installation's Dex, a caller token that ran out, the caller's RBAC there, a
// call made without a caller — instead of a bare "Unauthorized". The code and
// reason stay, so apierrors.IsUnauthorized / IsForbidden still hold.
func explainAuth(apiServer string) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		return authExplainer{next: rt, apiServer: apiServer}
	}
}

type authExplainer struct {
	next      http.RoundTripper
	apiServer string
}

// maxStatusBody bounds what is read of a refusal's body.
const maxStatusBody = 64 << 10

func (a authExplainer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := a.next.RoundTrip(req)
	if err != nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return resp, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	_ = resp.Body.Close()
	var status metav1.Status
	if json.Unmarshal(body, &status) != nil || status.Kind != "Status" {
		status = metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: strings.TrimSpace(string(body))}
		if resp.StatusCode == http.StatusUnauthorized {
			status.Code, status.Reason = http.StatusUnauthorized, metav1.StatusReasonUnauthorized
		}
	}
	status.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	token, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	status.Message = authFailure(a.apiServer, resp.StatusCode, status.Message, token, time.Now())
	out, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
	return resp, nil
}

// authFailure is the message of a refusal by the target apiserver: the
// apiserver's own, then the precondition it points at.
func authFailure(apiServer string, code int, message, token string, now time.Time) string {
	if message == "" {
		message = http.StatusText(code)
	}
	anonymous := token == "" || strings.Contains(message, `"system:anonymous"`)
	switch {
	case anonymous:
		return fmt.Sprintf("%s — the call reached %s without a caller's token: model-manager holds no credential for a remote target, so only a request carrying the caller's token (--downstream-oauth) may act there", message, apiServer)
	case code == http.StatusForbidden:
		return fmt.Sprintf("%s — %s authenticated the caller, whose RBAC there does not allow this: model-manager acts on the target as the caller, with no permissions of its own", message, apiServer)
	}
	c := identity.TokenClaims(token)
	if !c.Expires.IsZero() && now.After(c.Expires) {
		return fmt.Sprintf("%s — the caller's token expired at %s; the operation outlived it, a new request carries a fresh one", message, c.Expires.UTC().Format(time.RFC3339))
	}
	issuer, audience := "the installation's Dex", "the client ID the caller's token is issued for"
	if c.Issuer != "" {
		issuer = c.Issuer
	}
	if len(c.Audience) > 0 {
		audience = strings.Join(c.Audience, ", ")
	}
	return fmt.Sprintf("%s — %s does not accept the caller's token: the target cluster's apiserver must trust the installation's Dex as an OIDC issuer (structuredAuthentication jwt issuer %s, audience %s)", message, apiServer, issuer, audience)
}
