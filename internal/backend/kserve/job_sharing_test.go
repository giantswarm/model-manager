package kserve

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestDownloadIdentityIncludesCachePlacement(t *testing.T) {
	f := newFixture(t)
	base := downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}
	original, adopted, err := f.b.ensureJob(t.Context(), base)
	require.NoError(t, err)
	require.False(t, adopted)
	for _, test := range []struct {
		name string
		plan downloadPlan
	}{
		{"other node", downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testGPUNode, Claim: DefaultCacheClaim}},
		{"other claim", downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: "other-cache"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			other, adopted, err := f.b.ensureJob(t.Context(), test.plan)
			require.NoError(t, err)
			require.False(t, adopted)
			require.NotEqual(t, original.Name, other.Name)
		})
	}
	joined, adopted, err := f.b.ensureJob(t.Context(), base)
	require.NoError(t, err)
	require.True(t, adopted)
	require.Equal(t, original.Name, joined.Name)
}

func TestDownloadIdentityPreservesPlacementForLongNamesAndSharedCaches(t *testing.T) {
	plan := downloadPlan{Dir: strings.Repeat("a", maxNameLength), Node: "node", Claim: "cache"}
	name := plan.jobName()
	require.Len(t, name, maxNameLength)
	otherDirectory := plan
	otherDirectory.Dir = strings.Repeat("a", maxNameLength-1) + "b"
	require.NotEqual(t, name, otherDirectory.jobName())
	otherNode := plan
	otherNode.Node = "other-node"
	require.NotEqual(t, name, otherNode.jobName())
	plan.Node = ""
	otherClaim := plan
	otherClaim.Claim = "other-cache"
	require.NotEqual(t, plan.jobName(), otherClaim.jobName())
}

func TestDownloadAdoptsMatchingExistingNameWithoutInferringUserOwnership(t *testing.T) {
	f := newFixture(t)
	plan := downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}
	previous := f.b.buildJob(plan, f.b.cfg.settings(t.Context()))
	previous.Name = prefixed(jobPrefix, plan.Dir)
	_, err := f.cs.BatchV1().Jobs(testServingNS).Create(t.Context(), previous, metav1.CreateOptions{})
	require.NoError(t, err)
	joined, adopted, err := f.b.ensureJob(t.Context(), plan)
	require.NoError(t, err)
	require.True(t, adopted)
	require.Equal(t, previous.Name, joined.Name)
	different := plan
	different.Node = testGPUNode
	other, adopted, err := f.b.ensureJob(t.Context(), different)
	require.NoError(t, err)
	require.False(t, adopted)
	require.NotEqual(t, previous.Name, other.Name)
}

func TestConcurrentDownloadCreationJoinsOnlyMatchingResource(t *testing.T) {
	f := newFixture(t)
	plan := downloadPlan{Dir: "tiny", Repo: tinyRepo, Node: testCacheNode, Claim: DefaultCacheClaim}
	f.cs.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		creation := action.(k8stesting.CreateAction)
		object := creation.GetObject().(*batchv1.Job)
		require.NoError(t, f.cs.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), object, testServingNS))
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, object.Name)
	})
	joined, adopted, err := f.b.ensureJob(t.Context(), plan)
	require.NoError(t, err)
	require.True(t, adopted)
	require.Equal(t, plan.jobName(), joined.Name)
}

func TestOneObserverCancellationDoesNotStopAnotherDownloadObserver(t *testing.T) {
	f := newFixture(t)
	firstContext, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	firstSeen, secondSeen := make(chan *backend.JobObject, 1), make(chan *backend.JobObject, 1)
	var firstOnce, secondOnce sync.Once
	go func() {
		firstDone <- f.b.Pull(firstContext, backend.PullRequest{Ref: tinyRepo}, func(progress backend.Progress) {
			if progress.Object != nil {
				firstOnce.Do(func() { firstSeen <- progress.Object })
			}
		})
	}()
	var first *backend.JobObject
	select {
	case first = <-firstSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("first observer did not attach")
	}
	go func() {
		secondDone <- f.b.Pull(t.Context(), backend.PullRequest{Ref: tinyRepo}, func(progress backend.Progress) {
			if progress.Object != nil {
				secondOnce.Do(func() { secondSeen <- progress.Object })
			}
		})
	}()
	select {
	case second := <-secondSeen:
		require.Equal(t, first.Name, second.Name)
	case <-time.After(3 * time.Second):
		t.Fatal("second observer did not attach")
	}
	cancelFirst()
	select {
	case err := <-firstDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("first observer did not cancel")
	}
	job, err := f.cs.BatchV1().Jobs(testServingNS).Get(t.Context(), first.Name, metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}
	_, err = f.cs.BatchV1().Jobs(testServingNS).UpdateStatus(t.Context(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("second observer did not complete")
	}
}
