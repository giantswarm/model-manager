package kserve

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// DefaultWebhookStartWait is how long a create waits for the llmisvc
// admission webhook to answer: right after a serving slice is installed its
// HelmReleases are Ready while the webhook Service has no ready endpoint for
// a minute or two.
const DefaultWebhookStartWait = 2 * time.Minute

// webhookDialFailures are the connection failures that say the webhook's
// endpoint is not ready yet, as opposed to a webhook that answered and
// refused the object.
var webhookDialFailures = []string{"no route to host", "connection refused", "no endpoints available"}

var webhookName = regexp.MustCompile(`failed calling webhook "([^"]+)"`)

// unreachableWebhook names the admission webhook the API server could not
// reach behind err; "" when err is anything else, a webhook's own denial
// included.
func unreachableWebhook(err error) string {
	if !apierrors.IsInternalError(err) {
		return ""
	}
	msg := err.Error()
	m := webhookName.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	for _, f := range webhookDialFailures {
		if strings.Contains(msg, f) {
			return m[1]
		}
	}
	return ""
}

// createServing creates the serving object. A webhook that cannot be reached
// yet is serving still starting: the create is retried every poll interval
// until the webhook answers, the call's deadline or webhookWait, whichever
// comes first, and then answered as unavailable with the webhook named,
// never as a backend error (giantswarm/model-manager#241).
func (b *Backend) createServing(ctx context.Context, obj *unstructured.Unstructured) error {
	deadline := time.Now().Add(b.webhookWait)
	for {
		err := b.createOnce(ctx, obj)
		hook := unreachableWebhook(err)
		if hook == "" {
			return err
		}
		starting := fmt.Errorf("%w: serving is still starting: the admission webhook %s has no ready endpoint yet (%v); load the model again in a minute", backend.ErrUnavailable, hook, err)
		if time.Now().Add(b.opts.PollInterval).After(deadline) {
			return starting
		}
		b.log.Info("admission webhook not reachable yet; retrying the create", "webhook", hook, "name", obj.GetName(), "namespace", obj.GetNamespace())
		select {
		case <-ctx.Done():
			return starting
		case <-time.After(b.opts.PollInterval):
		}
	}
}
