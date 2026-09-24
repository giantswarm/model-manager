package kserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// stepOf is the named step of a loaded model's timeline.
func stepOf(t *testing.T, lm backend.LoadedModel, name string) backend.Step {
	t.Helper()
	for _, s := range lm.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %s in %+v", name, lm.Steps)
	return backend.Step{}
}

// expireServerReads lets the next list read the servers again.
func expireServerReads(f *fixture) {
	f.b.apiMu.Lock()
	for k, a := range f.b.apiCache {
		a.retryAt = time.Now().Add(-time.Second)
		f.b.apiCache[k] = a
	}
	f.b.apiMu.Unlock()
}

// A generate model is Ready once it answered one chat completion of one token
// under its served name; the ready step records when.
func TestReadyOnceTheModelAnsweredAChatCompletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, devVersion, generateDoc)
	f.serve("tiny", server)
	readyAt := time.Now().Add(-time.Minute)
	f.readyLLMISVC(ctx, "tiny", readyAt)

	lm := loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseReady, lm.Phase)
	assert.Equal(t, statusReady, lm.Status)
	ready := stepOf(t, lm, backend.PhaseReady)
	assert.Equal(t, backend.StepDone, ready.State)
	assert.Equal(t, "answered POST /v1/chat/completions", ready.Message)
	require.NotNil(t, ready.FinishedAt)
	assert.True(t, ready.FinishedAt.After(readyAt), "the step ends when the model answered, not when the object turned Ready")

	require.Len(t, server.asked, 1)
	assert.Equal(t, "/v1/chat/completions", server.asked[0].Path)
	assert.Equal(t, tinyRepo, server.asked[0].Body["model"], "the served name, spec.model.name")
	assert.EqualValues(t, 1, server.asked[0].Body["max_tokens"])
}

// A pooling model is asked for one embedding instead.
func TestPoolingModelIsAskedForAnEmbedding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, devVersion, poolingDoc)
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseReady, lm.Phase)
	require.Len(t, server.asked, 1)
	assert.Equal(t, "/v1/embeddings", server.asked[0].Path)
	assert.Equal(t, "OK", server.asked[0].Body["input"])
}

// A runtime that passed its readiness and answers its first request with an
// error is not Ready: the ready step fails with the status and the first line
// of the error, the phase is failed, and it is not asked again until the
// object turns Ready anew. WaitReady returns the failure instead of waiting
// for its timeout.
func TestFailedFirstRequestFailsTheReadyStep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, devVersion, generateDoc)
	server.answerStatus = 500
	server.answerBody = `{"error":{"message":"openai_harmony.HarmonyError: error downloading or loading vocab file\nTraceback (most recent call last):","type":"InternalServerError","code":500}}`
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseFailed, lm.Phase)
	assert.Equal(t, statusNotReady, lm.Status)
	assert.Equal(t, reasonFirstRequestFailed, lm.Reason)
	ready := stepOf(t, lm, backend.PhaseReady)
	assert.Equal(t, backend.StepFailed, ready.State)
	assert.Equal(t, reasonFirstRequestFailed, ready.Reason)
	assert.Equal(t, "the model's first request failed: POST /v1/chat/completions answered 500 Internal Server Error: openai_harmony.HarmonyError: error downloading or loading vocab file", ready.Message)
	assert.Equal(t, backend.StepDone, stepOf(t, lm, backend.PhaseRouting).State, "the route resolved; the model did not answer on it")

	loadedOne(t, f.b)
	assert.Len(t, server.asked, 1, "one request per Ready transition")

	err := f.b.WaitReady(ctx, tinyRepo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500 Internal Server Error: openai_harmony.HarmonyError")
}

// A model that gives no answer yet is still routing, and is asked again
// later; its answer then makes it Ready.
func TestUnansweredModelIsRoutingUntilItAnswers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := vllmServer(t, devVersion, generateDoc)
	server.silent = true
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseRouting, lm.Phase)
	assert.Equal(t, statusNotReady, lm.Status)
	routing := stepOf(t, lm, backend.PhaseRouting)
	assert.Equal(t, backend.StepInProgress, routing.State)
	assert.Equal(t, reasonAwaitingAnswer, routing.Reason)
	assert.Contains(t, routing.Message, "did not answer POST /v1/chat/completions")
	assert.Equal(t, backend.StepPending, stepOf(t, lm, backend.PhaseReady).State)

	server.silent = false
	expireServerReads(f)
	lm = loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseReady, lm.Phase)
	assert.Equal(t, statusReady, lm.Status)
}

// A server whose route list names nothing to ask is Ready as its object says,
// the ready step saying why no request was sent.
func TestModelWithoutRouteListIsNotAsked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	server := &modelServer{version: "0.23.0"}
	f.serve("tiny", server)
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, backend.PhaseReady, lm.Phase)
	assert.Empty(t, server.asked)
	ready := stepOf(t, lm, backend.PhaseReady)
	assert.Equal(t, backend.StepDone, ready.State)
	assert.Contains(t, ready.Message, "no request was sent")
	assert.Contains(t, ready.Message, "--disable-fastapi-docs")
}

func TestFirstErrorLine(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"first\nsecond"}}`: "first",
		`{"error":"plain"}`:                     "plain",
		`{"detail":"Not Found"}`:                "Not Found",
		`{"message":"bad"}`:                     "bad",
		"upstream connect error\nmore":          "upstream connect error",
		"":                                      "the answer carried no error",
	}
	for body, want := range cases {
		assert.Equal(t, want, firstErrorLine([]byte(body)), body)
	}
}
