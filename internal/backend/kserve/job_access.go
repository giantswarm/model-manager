package kserve

import (
	"context"
	"fmt"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	toolkitk8s "github.com/giantswarm/mcp-toolkit/k8s"

	"github.com/giantswarm/model-manager/internal/backend"
)

// JobScope separates this backend instance and its selected serving namespace.
func (b *Backend) JobScope(ctx context.Context) string {
	return b.jobScope + "/" + b.cfg.settings(ctx).Namespace
}

// CheckJobAccess evaluates current caller access to the original backing object.
func (b *Backend) CheckJobAccess(ctx context.Context, object *backend.JobObject) error {
	if object == nil {
		return nil
	}
	if object.Namespace == "" || object.Name == "" {
		return backend.ErrForbidden
	}
	resource := schema.GroupVersionResource{Group: object.Group, Version: object.Version, Resource: object.Resource}
	if resource != (schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}) && resource != llmisvcGVR {
		return backend.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	uid, err := b.jobObjectUID(ctx, object)
	if err == nil {
		if object.UID != "" && uid != object.UID {
			return backend.ErrNotFound
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	typed, err := b.k8s(ctx)
	if err != nil {
		return err
	}
	allowed, err := toolkitk8s.ReviewAccess(ctx, typed, authorizationv1.ResourceAttributes{Group: resource.Group, Version: resource.Version, Resource: resource.Resource, Namespace: object.Namespace, Name: object.Name, Verb: "get"})
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: backing resource access denied", backend.ErrForbidden)
	}
	return nil
}

func (b *Backend) jobObjectUID(ctx context.Context, object *backend.JobObject) (string, error) {
	if object.Group == "batch" {
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
