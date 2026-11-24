package kserve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/identity"
)

// callerFixture is a fixture whose calls run with the caller's clients, as
// with downstream OAuth: the fake clientset stands in for the caller's.
func callerFixture(t *testing.T) (*fixture, context.Context) {
	t.Helper()
	f := newFixture(t)
	f.b.opts.ClientsFor = func(context.Context) (kubernetes.Interface, dynamic.Interface, error) {
		return f.cs, f.dyn, nil
	}
	return f, identity.ContextWithToken(t.Context(), "caller-token")
}

// runningDownload creates the download Job for plan with a uid, as the
// apiserver would assign one.
func runningDownload(t *testing.T, f *fixture, plan downloadPlan, uid string) *batchv1.Job {
	t.Helper()
	job, adopted, err := f.b.ensureJob(t.Context(), plan)
	require.NoError(t, err)
	require.False(t, adopted)
	job.UID = types.UID(uid)
	job, err = f.cs.BatchV1().Jobs(testServingNS).Update(t.Context(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	return job
}

func deleteActions(f *fixture) []k8stesting.DeleteActionImpl {
	var out []k8stesting.DeleteActionImpl
	for _, action := range f.cs.Actions() {
		if d, ok := action.(k8stesting.DeleteActionImpl); ok && d.GetResource().Resource == "jobs" {
			out = append(out, d)
		}
	}
	return out
}

func TestStopDownloadDeletesTheSharedJobAsTheCaller(t *testing.T) {
	f, caller := callerFixture(t)
	job := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Preset: "tiny", Node: testCacheNode, Claim: DefaultCacheClaim}, "job-uid")

	stopped, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.NoError(t, err)
	require.Equal(t, backend.JobObject{Group: "batch", Version: "v1", Resource: "jobs", Namespace: testServingNS, Name: job.Name, UID: "job-uid"}, stopped.Object)
	require.Equal(t, tinyRepo, stopped.Model)
	require.Equal(t, testCacheNode, stopped.Node)
	require.Equal(t, "tiny", stopped.Preset)
	require.False(t, stopped.DryRun)

	_, err = f.cs.BatchV1().Jobs(testServingNS).Get(t.Context(), job.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the download Job is deleted: %v", err)
	deletes := deleteActions(f)
	require.Len(t, deletes, 1)
	opts := deletes[0].GetDeleteOptions()
	require.NotNil(t, opts.Preconditions)
	require.Equal(t, types.UID("job-uid"), *opts.Preconditions.UID, "a Job recreated under the same name is never deleted")
	require.Equal(t, metav1.DeletePropagationBackground, *opts.PropagationPolicy)
	require.Empty(t, opts.DryRun)
}

func TestStopDownloadMatchesByPresetRevisionAndNode(t *testing.T) {
	f, caller := callerFixture(t)
	onCache := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Revision: "abc", Preset: "tiny", Node: testCacheNode, Claim: DefaultCacheClaim}, "on-cache")
	onGPU := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Revision: "abc", Preset: "tiny", Node: testGPUNode, Claim: "hf-cache-gpu1"}, "on-gpu")

	_, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.ErrorIs(t, err, backend.ErrInvalid)
	require.ErrorContains(t, err, "matches 2 running downloads (node "+testGPUNode+" preset tiny, node "+testCacheNode+" preset tiny)")

	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo + ":other", Node: testGPUNode})
	require.ErrorIs(t, err, backend.ErrNotFound, "another revision is another download")

	stopped, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: "hf://" + tinyRepo + ":abc", Node: testGPUNode})
	require.NoError(t, err)
	require.Equal(t, onGPU.Name, stopped.Object.Name)

	stopped, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: "tiny"})
	require.NoError(t, err, "a preset name names the download for it")
	require.Equal(t, onCache.Name, stopped.Object.Name)
}

func TestStopDownloadRefusesWhatItCannotStopSafely(t *testing.T) {
	f, caller := callerFixture(t)
	job := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}, "job-uid")

	_, err := f.b.StopDownload(t.Context(), backend.StopDownloadRequest{Ref: tinyRepo})
	require.ErrorIs(t, err, backend.ErrUnsupported, "without a caller token the ServiceAccount would decide")
	require.ErrorContains(t, err, "--downstream-oauth")

	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo, UID: "predecessor"})
	require.ErrorIs(t, err, backend.ErrConflict)

	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: "org/other"})
	require.ErrorIs(t, err, backend.ErrNotFound)

	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: " "})
	require.ErrorIs(t, err, backend.ErrInvalid)

	require.Empty(t, deleteActions(f))
	_, err = f.cs.BatchV1().Jobs(testServingNS).Get(t.Context(), job.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestStopDownloadLeavesFinishedDownloadsAlone(t *testing.T) {
	f, caller := callerFixture(t)
	job := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}, "job-uid")
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}
	_, err := f.cs.BatchV1().Jobs(testServingNS).UpdateStatus(t.Context(), job, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.ErrorIs(t, err, backend.ErrNotFound)
}

func TestStopDownloadDryRunAsksTheClusterWithoutDeleting(t *testing.T) {
	f, caller := callerFixture(t)
	job := runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}, "job-uid")
	f.cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if len(action.(k8stesting.DeleteActionImpl).GetDeleteOptions().DryRun) > 0 {
			return true, nil, nil
		}
		return false, nil, nil
	})

	stopped, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo, DryRun: true})
	require.NoError(t, err)
	require.True(t, stopped.DryRun)
	deletes := deleteActions(f)
	require.Len(t, deletes, 1)
	require.Equal(t, []string{metav1.DryRunAll}, deletes[0].GetDeleteOptions().DryRun)
	_, err = f.cs.BatchV1().Jobs(testServingNS).Get(t.Context(), job.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestStopDownloadReturnsTheClustersRefusal(t *testing.T) {
	f, caller := callerFixture(t)
	runningDownload(t, f, downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}, "job-uid")
	f.cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "download", nil)
	})

	_, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.True(t, apierrors.IsForbidden(err), "the caller's RBAC decides: %v", err)

	f.cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "batch", Resource: "jobs"}, "download", nil)
	})
	_, err = f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.ErrorIs(t, err, backend.ErrConflict, "a failed uid precondition is a replaced Job")
}

func TestStopDownloadFailsEveryObserver(t *testing.T) {
	f, caller := callerFixture(t)
	done := make(chan error, 2)
	seen := make(chan *backend.JobObject, 2)
	var onces [2]sync.Once
	for i := range onces {
		go func() {
			done <- f.b.Pull(caller, backend.PullRequest{Ref: tinyRepo}, func(progress backend.Progress) {
				if progress.Object != nil {
					onces[i].Do(func() { seen <- progress.Object })
				}
			})
		}()
	}
	for range onces {
		select {
		case <-seen:
		case <-time.After(3 * time.Second):
			t.Fatal("an observer did not attach")
		}
	}

	_, err := f.b.StopDownload(caller, backend.StopDownloadRequest{Ref: tinyRepo})
	require.NoError(t, err)
	for range onces {
		select {
		case err := <-done:
			require.ErrorContains(t, err, "download stopped: Job "+testServingNS+"/")
		case <-time.After(3 * time.Second):
			t.Fatal("an observer did not see the download stop")
		}
	}
}
