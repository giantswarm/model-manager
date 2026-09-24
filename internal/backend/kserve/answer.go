package kserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/model-manager/internal/backend"
)

// A served model is Ready only once it has answered a request
// (giantswarm/model-manager#177). The LLMInferenceService's Ready condition
// follows the runtime's readiness, and vLLM's readiness is GET /health: a
// runtime can pass it and fail every request — both gpt-oss presets turned
// Ready, then answered 500 to every chat completion, their harmony renderer
// loading its tokenizer encodings only at the first request. So once each
// time the object turns Ready, the driver asks the model one request on the
// workload Service, beside the reads of its interfaces (interfaces.go): a
// chat completion of one token for a generate model, an embedding of one word
// for a pooling one. An answer finishes the ready step; a failing answer fails
// it with the runtime's status and the first line of its error; no answer
// keeps the serve routing and asks again after serverReadRetry.

const (
	// answerTimeout bounds the first request: one token, sent once per Ready
	// transition.
	answerTimeout = 15 * time.Second
	// answerBodyLimit caps what is read of an answer.
	answerBodyLimit = 64 << 10

	reasonAwaitingAnswer     = "AwaitingFirstAnswer"
	reasonFirstRequestFailed = "FirstRequestFailed"
)

// firstAnswer is what the model said to its first request. The zero value is
// "not asked".
type firstAnswer struct {
	// Path is the route asked.
	Path string
	// At is when the model answered; zero until it did.
	At time.Time
	// Failure is the runtime's answer to a request that failed: its status and
	// the first line of its error.
	Failure string
	// Waiting says why no answer is known yet: the request got none.
	Waiting string
	// Skipped says why no request was sent: the route list names neither chat
	// completions nor embeddings, so what to ask is unknown.
	Skipped string
}

// firstRequest is the request a model is asked first, by the interfaces its
// server registered: empty when it registered neither.
func firstRequest(api serverAPI, model string) (path string, body any) {
	for _, i := range api.Interfaces {
		if i.Type == backend.InterfaceCompletions {
			return i.Path, map[string]any{
				"model":      model,
				"messages":   []map[string]string{{"role": "user", "content": "Say OK."}},
				"max_tokens": 1,
			}
		}
	}
	for _, i := range api.Interfaces {
		if i.Type == backend.InterfaceEmbeddings {
			return i.Path, map[string]any{"model": model, "input": "OK"}
		}
	}
	return "", nil
}

// askFirst sends the model its first request at origin. A server that did not
// answer the interface reads is not asked: it is waited for.
func (b *Backend) askFirst(ctx context.Context, origin, model string, api serverAPI) firstAnswer {
	if !api.retryAt.IsZero() {
		return firstAnswer{Waiting: api.Reason}
	}
	path, body := firstRequest(api, model)
	if path == "" {
		reason := api.Reason
		if reason == "" {
			reason = "the model answers neither chat completions nor embeddings"
		}
		return firstAnswer{Skipped: "no request was sent: " + reason}
	}
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return firstAnswer{Path: path, Failure: fmt.Sprintf("encode the request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, bytes.NewReader(raw))
	if err != nil {
		return firstAnswer{Path: path, Failure: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.serverHTTP.Do(req)
	if err != nil {
		return firstAnswer{Path: path, Waiting: fmt.Sprintf("the model did not answer POST %s (%v); asked again after %s", path, err, serverReadRetry)}
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, answerBodyLimit))
	if resp.StatusCode/100 != 2 {
		return firstAnswer{Path: path, Failure: resp.Status + ": " + firstErrorLine(answer)}
	}
	return firstAnswer{Path: path, At: time.Now()}
}

// firstErrorLine is the first line of an error answer: the OpenAI error's
// message, FastAPI's detail, or the body itself.
func firstErrorLine(body []byte) string {
	var doc struct {
		Error   json.RawMessage `json:"error"`
		Detail  json.RawMessage `json:"detail"`
		Message string          `json:"message"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &doc) == nil {
		var nested struct {
			Message string `json:"message"`
		}
		var plain string
		switch {
		case json.Unmarshal(doc.Error, &nested) == nil && nested.Message != "":
			text = nested.Message
		case json.Unmarshal(doc.Error, &plain) == nil && plain != "":
			text = plain
		case json.Unmarshal(doc.Detail, &plain) == nil && plain != "":
			text = plain
		case doc.Message != "":
			text = doc.Message
		}
	}
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "the answer carried no error"
	}
	if len(line) > crashLineMax {
		line = line[:crashLineMax] + "…"
	}
	return line
}

// applyAnswer holds a Ready object's ready step to the model's first answer:
// done when it answered, failed with the runtime's error when it answered
// with one — the object is then not Ready and its phase failed —, and while
// no answer is known the serve is still routing. A model whose server named
// nothing to ask is Ready as the object says, its ready step saying why.
func (sv *served) applyAnswer() {
	if !sv.Ready || sv.Deleting || len(sv.Steps) != len(backend.ServePhases) {
		return
	}
	a := sv.API.Answer
	t := &timeline{steps: sv.Steps}
	switch {
	case a.Skipped != "":
		t.steps[stepReady].Message = a.Skipped
		return
	case !a.At.IsZero():
		t.done(stepReady, a.At, "answered POST "+a.Path)
		return
	case a.Failure != "":
		message := fmt.Sprintf("the model's first request failed: POST %s answered %s", a.Path, a.Failure)
		t.fail(stepReady, reasonFirstRequestFailed, message)
		sv.Reason, sv.Message = reasonFirstRequestFailed, message
	default:
		routing := &t.steps[stepRouting]
		routing.State, routing.FinishedAt = backend.StepInProgress, nil
		t.note(stepRouting, reasonAwaitingAnswer, "waiting for the model's first answer: "+a.Waiting)
		t.steps[stepReady] = backend.Step{Name: backend.PhaseReady, State: backend.StepPending}
		sv.Reason, sv.Message = reasonAwaitingAnswer, routing.Message
	}
	sv.Ready, sv.Status = false, statusNotReady
	sv.Phase = t.phase(sv.Deleting)
}

// answerFailure is the failed first request of a Ready transition; empty when
// the model answered, was not asked, or has not answered yet.
func (sv served) answerFailure() string {
	if sv.Reason == reasonFirstRequestFailed {
		return sv.Message
	}
	return ""
}
