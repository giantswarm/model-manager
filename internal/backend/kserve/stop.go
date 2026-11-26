package kserve

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/requester"
)

// StopDownload deletes the running download Job req names, as the caller.
// The Job is shared by every task observing it, so the cluster's RBAC on
// batch/jobs decides who may stop it; without a caller token the call would
// run with the ServiceAccount's permissions and is refused instead.
func (b *Backend) StopDownload(ctx context.Context, req backend.StopDownloadRequest) (*backend.StoppedDownload, error) {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: model reference is required", backend.ErrInvalid)
	}
	if _, ok := requester.IDTokenFromContext(ctx); !ok {
		return nil, fmt.Errorf("%w: stopping a shared download runs only with the caller's own Kubernetes credentials (--downstream-oauth); delete the download Job through the Kubernetes API instead", backend.ErrUnsupported)
	}
	s := b.cfg.settings(ctx)
	client, err := b.k8s(ctx)
	if err != nil {
		return nil, err
	}
	jobs := client.BatchV1().Jobs(s.Namespace)
	list, err := jobs.List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue + "," + ComponentLabel + "=" + componentDownload,
	})
	if err != nil {
		return nil, fmt.Errorf("list download Jobs in %s: %w", s.Namespace, err)
	}
	var matches []*batchv1.Job
	for i := range list.Items {
		if j := &list.Items[i]; !jobFinished(j) && j.DeletionTimestamp == nil && downloadNamed(j, ref, req) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: no running download of %s", backend.ErrNotFound, describeDownload(ref, req))
	case 1:
	default:
		return nil, fmt.Errorf("%w: %s matches %d running downloads (%s); name the node or the preset", backend.ErrInvalid, describeDownload(ref, req), len(matches), strings.Join(downloadPlacements(matches), ", "))
	}
	job := matches[0]
	if req.UID != "" && string(job.UID) != req.UID {
		return nil, fmt.Errorf("%w: the running download Job %s/%s has uid %s, not %s", backend.ErrConflict, job.Namespace, job.Name, job.UID, req.UID)
	}
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation, Preconditions: metav1.NewUIDPreconditions(string(job.UID))}
	if req.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if err := jobs.Delete(ctx, job.Name, opts); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("%w: download Job %s/%s is already gone", backend.ErrNotFound, job.Namespace, job.Name)
		case apierrors.IsConflict(err):
			return nil, fmt.Errorf("%w: download Job %s/%s was replaced while stopping it", backend.ErrConflict, job.Namespace, job.Name)
		}
		return nil, fmt.Errorf("delete download Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	if !req.DryRun {
		b.inv.invalidate()
		b.log.Info("download stopped", "job", job.Name, "namespace", job.Namespace, "model", job.Annotations[ModelAnnotation], "node", job.Annotations[NodeAnnotation], requester.LogAttr(ctx))
	}
	return &backend.StoppedDownload{
		Object: backend.JobObject{Group: downloadJobGVR.Group, Version: downloadJobGVR.Version, Resource: downloadJobGVR.Resource, Namespace: job.Namespace, Name: job.Name, UID: string(job.UID)},
		Model:  job.Annotations[ModelAnnotation],
		Node:   job.Annotations[NodeAnnotation],
		Preset: job.Annotations[PresetAnnotation],
		DryRun: req.DryRun,
	}, nil
}

// downloadNamed reports whether the download Job is the one ref and req name:
// ref is a repository (with an optional revision) or a preset name, the way
// pull_model takes it.
func downloadNamed(job *batchv1.Job, ref string, req backend.StopDownloadRequest) bool {
	if req.Node != "" && job.Annotations[NodeAnnotation] != req.Node {
		return false
	}
	if req.Preset != "" && job.Annotations[PresetAnnotation] != req.Preset {
		return false
	}
	if !isRepoID(ref) && !strings.ContainsAny(ref, "/:@") {
		return job.Annotations[PresetAnnotation] == ref
	}
	repo, revision := splitRevision(ref)
	return job.Annotations[ModelAnnotation] == repo && (revision == "" || job.Annotations[RevisionAnnotation] == revision)
}

func describeDownload(ref string, req backend.StopDownloadRequest) string {
	out := ref
	if req.Preset != "" {
		out += " with preset " + req.Preset
	}
	if req.Node != "" {
		out += " on node " + req.Node
	}
	return out
}

func downloadPlacements(jobs []*batchv1.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		place := "node " + cmp.Or(j.Annotations[NodeAnnotation], "unpinned")
		if p := j.Annotations[PresetAnnotation]; p != "" {
			place += " preset " + p
		}
		out = append(out, place)
	}
	slices.Sort(out)
	return out
}
