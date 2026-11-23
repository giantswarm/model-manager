package kserve

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/model-manager/internal/backend"
)

var downloadJobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

// JobScope separates this backend instance and its selected serving namespace.
func (b *Backend) JobScope(ctx context.Context) string {
	return b.jobScope + "/" + b.cfg.settings(ctx).Namespace
}

// CheckJobAccess reads the original backing object as the current caller.
// The API server authorizes a get before it looks the object up, so a
// NotFound answer already proves the caller may read that named resource.
func (b *Backend) CheckJobAccess(ctx context.Context, object *backend.JobObject) error {
	if object == nil {
		return nil
	}
	if object.Namespace == "" || object.Name == "" {
		return backend.ErrForbidden
	}
	resource := schema.GroupVersionResource{Group: object.Group, Version: object.Version, Resource: object.Resource}
	if resource != downloadJobGVR && resource != llmisvcGVR {
		return backend.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	uid, err := b.jobObjectUID(ctx, resource, object)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case object.UID != "" && uid != object.UID:
		return backend.ErrNotFound
	}
	return nil
}

func (b *Backend) jobObjectUID(ctx context.Context, resource schema.GroupVersionResource, object *backend.JobObject) (string, error) {
	if resource == downloadJobGVR {
		client, err := b.k8s(ctx)
		if err != nil {
			return "", err
		}
		current, err := client.BatchV1().Jobs(object.Namespace).Get(ctx, object.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return string(current.UID), nil
	}
	client, err := b.dynamic(ctx)
	if err != nil {
		return "", err
	}
	current, err := client.Resource(llmisvcGVR).Namespace(object.Namespace).Get(ctx, object.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(current.GetUID()), nil
}
