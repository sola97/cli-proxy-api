package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type streamQuotaResultSignalHook struct {
	NoopHook
	results chan Result
}

func (h *streamQuotaResultSignalHook) OnResult(_ context.Context, result Result) {
	if result.Error != nil {
		h.results <- result
	}
}

type streamQuotaError struct {
	customStatusError
	credentialScoped bool
}

func (e streamQuotaError) IsCredentialScoped() bool { return e.credentialScoped }

func TestExecuteStreamQuotaFailurePreservesCooldownAndScope(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, credentialScoped := range []bool{true, false} {
		for _, weighted := range []bool{false, true} {
			name := fmt.Sprintf("credential_scope=%t/weighted=%t", credentialScoped, weighted)
			t.Run(name, func(t *testing.T) {
				var selector Selector = &RoundRobinSelector{}
				if weighted {
					selector = &WeightedRoundRobinSelector{}
				}
				manager := NewManager(nil, selector, nil)
				manager.SetRetryConfig(3, 30*time.Second, 0)
				model := "stream-quota-model"
				siblingModel := "stream-quota-sibling"
				highID := "stream-quota-high-" + name
				lowID := "stream-quota-low-" + name
				for _, candidate := range []*Auth{
					{ID: highID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "4", AttributeWeight: "1"}},
					{ID: lowID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "3", AttributeWeight: "1"}},
				} {
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}, {ID: siblingModel}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
					if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
				}

				retryAfter := time.Hour
				quotaErr := streamQuotaError{
					customStatusError: customStatusError{
						code: http.StatusTooManyRequests, msg: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`, retryAfter: &retryAfter,
					},
					credentialScoped: credentialScoped,
				}
				if !credentialScoped {
					quotaErr.msg = `{"error":{"type":"rate_limit_error","message":"Model rate limit exceeded"}}`
				}
				var attempts []string
				manager.RegisterExecutor(&customStreamMockExecutor{
					identifier: "codex",
					streamFn: func(_ context.Context, selected *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						attempts = append(attempts, selected.ID)
						chunks := make(chan cliproxyexecutor.StreamChunk, 2)
						chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\"}\n\n")}
						chunks <- cliproxyexecutor.StreamChunk{Err: quotaErr}
						close(chunks)
						return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
					},
				})
				before := time.Now()
				result, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				var payloads, failures int
				for chunk := range result.Chunks {
					if len(chunk.Payload) != 0 {
						payloads++
					}
					if chunk.Err != nil {
						failures++
						if chunk.Err != quotaErr {
							t.Fatalf("stream error = %v, want original quota error", chunk.Err)
						}
					}
				}
				if payloads != 1 || failures != 1 || len(attempts) != 1 || attempts[0] != highID {
					t.Fatalf("started stream must retain its payload and error without replay: payloads=%d failures=%d attempts=%v", payloads, failures, attempts)
				}
				high, _ := manager.GetByID(highID)
				state := high.ModelStates[model]
				if state == nil || state.NextRetryAfter.Before(before.Add(retryAfter)) {
					t.Errorf("model cooldown lost upstream RetryAfter: state=%+v", state)
				}
				if credentialScoped && (high.Quota.Reason != "credential_quota" || high.Quota.NextRecoverAt.Before(before.Add(retryAfter))) {
					t.Errorf("credential cooldown lost scope or RetryAfter: quota=%+v", high.Quota)
				}
				for _, requestedModel := range []string{model, siblingModel} {
					wantID := lowID
					if requestedModel == siblingModel && !credentialScoped {
						wantID = highID
					}
					selected, _, _, errSelect := manager.pickNextMixed(context.Background(), []string{"codex"}, requestedModel, cliproxyexecutor.Options{}, nil)
					if errSelect != nil {
						t.Fatalf("pickNextMixed(%s) error = %v", requestedModel, errSelect)
					}
					if selected.ID != wantID {
						t.Errorf("pickNextMixed(%s) = %s, want %s", requestedModel, selected.ID, wantID)
					}
				}
				if blocked, _, _ := isAuthBlockedForModel(high, model, before.Add(time.Minute)); !blocked {
					t.Error("exhausted model becomes selectable before the upstream reset")
				}
			})
		}
	}
}

func TestExecuteStreamCancellationRecordsOnlyConfirmedCredentialQuota(t *testing.T) {
	withQuotaCooldownEnabled(t)
	retryAfter := time.Hour

	for _, test := range []struct {
		name        string
		err         error
		quota       bool
		markAttempt bool
	}{
		{
			name: "upstream quota error",
			err: streamQuotaError{
				customStatusError: customStatusError{
					code:       http.StatusTooManyRequests,
					msg:        `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`,
					retryAfter: &retryAfter,
				},
				credentialScoped: true,
			},
			quota: true, markAttempt: true,
		},
		{name: "client cancellation", err: context.Canceled},
		{name: "upstream EOF", err: markUpstreamExecutionAttempt(io.EOF), markAttempt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := "stream-cancel-quota-" + strings.ReplaceAll(test.name, " ", "-")
			highID := model + "-high"
			lowID := model + "-low"
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			for _, candidate := range []*Auth{
				{ID: highID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "4", AttributeWeight: "1"}},
				{ID: lowID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "3", AttributeWeight: "1"}},
			} {
				registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
					t.Fatal(errRegister)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			manager.RegisterExecutor(&customStreamMockExecutor{
				identifier: "codex",
				streamFn: func(execCtx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					if test.markAttempt {
						cliproxyexecutor.MarkUpstreamAttempt(execCtx)
					}
					cancel()
					return nil, test.err
				},
			})
			_, errStream := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
			if !errors.Is(errStream, context.Canceled) {
				t.Fatalf("ExecuteStream() error = %v, want context cancellation", errStream)
			}

			high, _ := manager.GetByID(highID)
			blocked, _, _ := isAuthBlockedForModel(high, model, time.Now())
			if blocked != test.quota {
				t.Fatalf("credential blocked = %t, want quota=%t; auth=%+v", blocked, test.quota, high)
			}
			if test.quota {
				if high.Quota.Reason != "credential_quota" || high.Quota.NextRecoverAt.Before(time.Now().Add(55*time.Minute)) {
					t.Fatalf("confirmed upstream quota cooldown missing or too short: quota=%+v", high.Quota)
				}
				selected, _, _, errSelect := manager.pickNextMixed(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, nil)
				if errSelect != nil || selected.ID != lowID {
					t.Fatalf("healthy credential selection = %v, %v; want %s", selected, errSelect, lowID)
				}
			} else if high.Quota.Reason != "" || !high.Quota.NextRecoverAt.IsZero() {
				t.Fatalf("client cancellation/EOF created quota state: %+v", high.Quota)
			}
		})
	}
}

func TestExecuteStreamCancellationDrainsBufferedAndPostPayloadQuota(t *testing.T) {
	withQuotaCooldownEnabled(t)
	retryAfter := time.Hour
	for _, timing := range []string{"bootstrap", "after_payload"} {
		for _, credentialScoped := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/credential_scope=%t", timing, credentialScoped), func(t *testing.T) {
				model := fmt.Sprintf("stream-cancel-drain-%s-%t", timing, credentialScoped)
				highID := model + "-high"
				lowID := model + "-low"
				hook := &streamQuotaResultSignalHook{results: make(chan Result, 1)}
				manager := NewManager(nil, &RoundRobinSelector{}, hook)
				for _, candidate := range []*Auth{
					{ID: highID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "4", AttributeWeight: "1"}},
					{ID: lowID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "3", AttributeWeight: "1"}},
				} {
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
					if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
				}

				quotaErr := streamQuotaError{
					customStatusError: customStatusError{
						code:       http.StatusTooManyRequests,
						msg:        `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`,
						retryAfter: &retryAfter,
					},
					credentialScoped: credentialScoped,
				}
				ctx, cancel := context.WithCancel(context.Background())
				releaseQuota := make(chan struct{})
				manager.RegisterExecutor(&customStreamMockExecutor{
					identifier: "codex",
					streamFn: func(execCtx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						cliproxyexecutor.MarkUpstreamAttempt(execCtx)
						chunks := make(chan cliproxyexecutor.StreamChunk, 2)
						if timing == "bootstrap" {
							cancel()
							chunks <- cliproxyexecutor.StreamChunk{Err: quotaErr}
							close(chunks)
							return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
						}
						chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: response.created\n\n")}
						go func() {
							<-releaseQuota
							cancel()
							chunks <- cliproxyexecutor.StreamChunk{Err: quotaErr}
							close(chunks)
						}()
						return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
					},
				})

				result, errStream := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if timing == "bootstrap" {
					if !errors.Is(errStream, context.Canceled) {
						t.Fatalf("ExecuteStream() error = %v, want bootstrap cancellation", errStream)
					}
				} else {
					if errStream != nil {
						t.Fatalf("ExecuteStream() error = %v", errStream)
					}
					for chunk := range result.Chunks {
						if len(chunk.Payload) > 0 {
							close(releaseQuota)
						}
					}
				}
				select {
				case recorded := <-hook.results:
					if recorded.RetryAfter == nil || statusCodeFromError(recorded.Error) != http.StatusTooManyRequests || !strings.Contains(strings.ToLower(recorded.Error.Error()), "usage_limit_reached") {
						t.Fatalf("recorded cancellation result = %+v, want confirmed usage quota", recorded)
					}
				case <-time.After(time.Second):
					t.Fatal("quota cancellation result was not recorded")
				}

				high, _ := manager.GetByID(highID)
				state := high.ModelStates[model]
				if state == nil || state.NextRetryAfter.Before(time.Now().Add(55*time.Minute)) {
					t.Fatalf("canceled stream lost model cooldown: state=%+v", state)
				}
				if credentialScoped {
					if high.Quota.Reason != "credential_quota" || high.Quota.NextRecoverAt.Before(time.Now().Add(55*time.Minute)) {
						t.Fatalf("canceled stream lost credential quota cooldown: quota=%+v", high.Quota)
					}
				} else if high.Quota.Reason == "credential_quota" {
					t.Fatalf("model-level quota error created credential-scoped cooldown: quota=%+v", high.Quota)
				}
				selected, _, _, errSelect := manager.pickNextMixed(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, nil)
				if errSelect != nil || selected.ID != lowID {
					t.Fatalf("healthy credential selection = %v, %v; want %s", selected, errSelect, lowID)
				}
			})
		}
	}
}

func TestExecuteStreamCancellationClosesOutputWhileDrainingHeldOpenSource(t *testing.T) {
	withQuotaCooldownEnabled(t)
	model := "stream-cancel-held-open-quota"
	highID := model + "-high"
	lowID := model + "-low"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	for _, candidate := range []*Auth{
		{ID: highID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "4", AttributeWeight: "1"}},
		{ID: lowID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "3", AttributeWeight: "1"}},
	} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	retryAfter := time.Hour
	quotaErr := streamQuotaError{
		customStatusError: customStatusError{
			code:       http.StatusTooManyRequests,
			msg:        `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`,
			retryAfter: &retryAfter,
		},
		credentialScoped: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := make(chan cliproxyexecutor.StreamChunk, 2)
	manager.RegisterExecutor(&customStreamMockExecutor{
		identifier: "codex",
		streamFn: func(execCtx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			cliproxyexecutor.MarkUpstreamAttempt(execCtx)
			upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("data: response.created\n\n")}
			return &cliproxyexecutor.StreamResult{Chunks: upstream}, nil
		},
	})
	result, errStream := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if chunk, ok := <-result.Chunks; !ok || len(chunk.Payload) == 0 {
		t.Fatalf("first stream chunk = %+v, open=%t; want payload", chunk, ok)
	}
	cancel()

	select {
	case _, ok := <-result.Chunks:
		if ok {
			t.Fatal("result stream emitted a chunk after cancellation")
		}
	case <-time.After(time.Second):
		close(upstream)
		t.Fatal("result stream stayed open while its upstream source remained open")
	}

	// The asynchronous drain must continue observing upstream after the consumer-facing stream closes.
	upstream <- cliproxyexecutor.StreamChunk{Err: quotaErr}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		high, _ := manager.GetByID(highID)
		if high.Quota.Reason == "credential_quota" && high.ModelStates[model] != nil {
			close(upstream)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(upstream)
	high, _ := manager.GetByID(highID)
	t.Fatalf("late quota chunk was not recorded after output closed: quota=%+v model=%+v", high.Quota, high.ModelStates[model])
}
