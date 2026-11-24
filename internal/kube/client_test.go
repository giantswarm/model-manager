package kube

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/giantswarm/model-manager/internal/requester"
)

func TestForSelectsExplicitClientsOrCaller(t *testing.T) {
	clients, err := fromRESTConfig(&rest.Config{Host: "https://api.example.test", BearerToken: "application-token"})
	require.NoError(t, err)
	application, err := clients.For(t.Context())
	require.NoError(t, err)
	require.Same(t, clients.Clients, application)
	callerContext := requester.ContextWithToken(t.Context(), jwt(t, time.Now().Add(time.Hour)))
	caller, err := clients.For(callerContext)
	require.NoError(t, err)
	require.NotSame(t, application, caller)
	cached, err := clients.For(callerContext)
	require.NoError(t, err)
	require.Same(t, caller, cached)
	expiredContext := requester.ContextWithToken(t.Context(), jwt(t, time.Now().Add(-time.Minute)))
	expired, err := clients.For(expiredContext)
	require.NoError(t, err)
	require.NotSame(t, application, expired)
	uncached, err := clients.For(expiredContext)
	require.NoError(t, err)
	require.NotSame(t, expired, uncached)
}

func TestForConstructionFailureSendsNoServiceAccountRequest(t *testing.T) {
	var requests atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"kind":"ConfigMapList","apiVersion":"v1","items":[]}`))
	}))
	t.Cleanup(api.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0o600))
	clients, err := fromRESTConfig(&rest.Config{Host: api.URL, BearerToken: "application-token", TLSClientConfig: rest.TLSClientConfig{CAFile: caFile}})
	require.NoError(t, err)
	require.NoError(t, os.Remove(caFile))
	caller, err := clients.For(requester.ContextWithToken(t.Context(), "caller-token"))
	require.ErrorContains(t, err, "caller-token Kubernetes clients")
	require.Nil(t, caller)
	require.Zero(t, requests.Load())
}

func TestForExpiredTokenReachesAPIAsCaller(t *testing.T) {
	token := jwt(t, time.Now().Add(-time.Minute))
	var authorization atomic.Value
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization.Store(request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"expired caller token"}`))
	}))
	t.Cleanup(api.Close)
	clients, err := fromRESTConfig(&rest.Config{Host: api.URL, BearerToken: "application-token"})
	require.NoError(t, err)
	caller, err := clients.For(requester.ContextWithToken(t.Context(), token))
	require.NoError(t, err)
	_, err = caller.Clientset.CoreV1().ConfigMaps("default").List(t.Context(), metav1.ListOptions{})
	require.True(t, apierrors.IsUnauthorized(err), "%v", err)
	require.Equal(t, "Bearer "+token, authorization.Load())
}

func jwt(t *testing.T, expiry time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": expiry.Unix()})
	require.NoError(t, err)
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestClientRateLimitsPreserveOverrides(t *testing.T) {
	for _, test := range []struct {
		name      string
		qps       float32
		burst     int
		wantQPS   float32
		wantBurst int
	}{
		{"defaults", 0, 0, 100, 200},
		{"configured", 7, 3, 7, 3},
		{"unlimited", -1, 0, -1, 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := &rest.Config{Host: "https://api.example.test", QPS: test.qps, Burst: test.burst}
			factory, err := fromRESTConfig(configuration)
			require.NoError(t, err)
			require.Equal(t, test.qps, configuration.QPS)
			require.Equal(t, test.burst, configuration.Burst)
			caller, err := factory.For(requester.ContextWithToken(t.Context(), "caller-token"))
			require.NoError(t, err)
			for _, client := range []rest.Interface{factory.Clientset.CoreV1().RESTClient(), caller.Clientset.CoreV1().RESTClient()} {
				limiter := client.GetRateLimiter()
				if test.wantQPS < 0 {
					require.Nil(t, limiter)
					continue
				}
				require.Equal(t, test.wantQPS, limiter.QPS())
				for range test.wantBurst {
					require.True(t, limiter.TryAccept(), test.name)
				}
				require.False(t, limiter.TryAccept(), test.name)
			}
		})
	}
	limiter := flowcontrol.NewFakeAlwaysRateLimiter()
	factory, err := fromRESTConfig(&rest.Config{Host: "https://api.example.test", RateLimiter: limiter})
	require.NoError(t, err)
	caller, err := factory.For(requester.ContextWithToken(t.Context(), "caller-token"))
	require.NoError(t, err)
	require.Same(t, limiter, factory.Clientset.CoreV1().RESTClient().GetRateLimiter())
	require.Same(t, limiter, caller.Clientset.CoreV1().RESTClient().GetRateLimiter())
}
