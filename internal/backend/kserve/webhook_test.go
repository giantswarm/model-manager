package kserve

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

const testWebhook = "llminferenceservice.kserve-webhook-server.v1alpha2.defaulter"

// webhookDialError is what the API server answers a create with while the
// llmisvc webhook Service has no ready endpoint.
func webhookDialError(failure string) error {
	return apierrors.NewInternalError(errors.New(`failed calling webhook "` + testWebhook + `": failed to call webhook: Post "https://llmisvc-webhook-server-service.kserve.svc:443/mutate-serving-kserve-io-v1alpha2-llminferenceservice?timeout=10s": dial tcp 172.31.0.10:443: connect: ` + failure))
}

// unreachableFor makes the first n creates of an LLMInferenceService fail as
// an unreachable webhook; it counts every create.
func unreachableFor(f *fixture, n int32) *atomic.Int32 {
	var creates atomic.Int32
	f.dyn.PrependReactor("create", llmisvcGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		if creates.Add(1) <= n {
			return true, nil, webhookDialError("no route to host")
		}
		return false, nil, nil
	})
	return &creates
}

func TestServeWaitsForAnUnreachableWebhook(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	creates := unreachableFor(f, 3)

	res, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "tiny", Node: testGPUNode})
	require.NoError(t, err, "the create is retried once the webhook answers, never a backend error")
	assert.False(t, res.AlreadyServing)
	assert.EqualValues(t, 4, creates.Load(), "three refused creates, then the one that lands")
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err, "the serving object exists")
}

func TestServeAnswersUnavailableWhileTheWebhookStaysUnreachable(t *testing.T) {
	f := newFixture(t)
	f.b.webhookWait = 50 * time.Millisecond
	ctx := context.Background()
	creates := unreachableFor(f, 1<<30)

	_, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "tiny", Node: testGPUNode})
	require.ErrorIs(t, err, backend.ErrUnavailable, "serving is still starting, not a broken backend")
	assert.ErrorContains(t, err, "serving is still starting: the admission webhook "+testWebhook+" has no ready endpoint yet")
	assert.Greater(t, creates.Load(), int32(1), "the create was retried")
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "nothing was created")

	// The call's own deadline ends the wait sooner.
	f.b.webhookWait = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = f.b.Serve(ctx, backend.LoadRequest{Preset: "tiny", Node: testGPUNode})
	require.ErrorIs(t, err, backend.ErrUnavailable)
}

func TestUnreachableWebhook(t *testing.T) {
	for _, failure := range []string{"no route to host", "connection refused", "no endpoints available for service \"llmisvc-webhook-server-service\""} {
		assert.Equal(t, testWebhook, unreachableWebhook(webhookDialError(failure)), failure)
	}
	for name, err := range map[string]error{
		"none":                   nil,
		"denied":                 apierrors.NewForbidden(llmisvcGVR.GroupResource(), "tiny", errors.New(`admission webhook "`+testWebhook+`" denied the request: spec.model.uri is required`)),
		"other internal error":   apierrors.NewInternalError(errors.New(`failed calling webhook "` + testWebhook + `": the server could not find the requested resource`)),
		"dial without a webhook": apierrors.NewInternalError(errors.New("dial tcp 172.31.0.10:443: connect: connection refused")),
	} {
		assert.Empty(t, unreachableWebhook(err), name)
	}
}
