package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKeyedConcurrencyController_AllowsMaxConcurrencyAndWaits(t *testing.T) {
	ctrl, err := NewKeyedConcurrencyController(2, 1)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	h1, _, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
	if err != nil {
		t.Fatalf("acquire #1: %v", err)
	}
	defer h1.Release()
	h2, _, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
	if err != nil {
		t.Fatalf("acquire #2: %v", err)
	}
	defer h2.Release()

	type result struct {
		handle   *admissionHandle
		decision admissionDecision
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		h, d, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
		ch <- result{handle: h, decision: d, err: err}
	}()

	time.Sleep(50 * time.Millisecond)
	h1.Release()

	res := <-ch
	if res.err != nil {
		t.Fatalf("waiting acquire failed: %v", res.err)
	}
	defer res.handle.Release()
	if !res.decision.Waited {
		t.Fatalf("expected waited=true")
	}
}

func TestKeyedConcurrencyController_RejectsWhenQueueFull(t *testing.T) {
	ctrl, err := NewKeyedConcurrencyController(1, 1)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	h1, _, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
	if err != nil {
		t.Fatalf("acquire #1: %v", err)
	}
	defer h1.Release()

	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		h, _, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
		if err == nil && h != nil {
			h.Release()
		}
	}()
	time.Sleep(50 * time.Millisecond)

	_, decision, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
	if !errors.Is(err, ErrConcurrencyQueueFull) {
		t.Fatalf("expected queue full, got err=%v decision=%+v", err, decision)
	}

	h1.Release()
	<-waiterDone
}

func TestKeyedConcurrencyController_RejectsOnWaitTimeout(t *testing.T) {
	ctrl, err := NewKeyedConcurrencyController(1, 1)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	h1, _, err := ctrl.Acquire(context.Background(), "app:model", time.Second)
	if err != nil {
		t.Fatalf("acquire #1: %v", err)
	}
	defer h1.Release()

	start := time.Now()
	_, decision, err := ctrl.Acquire(context.Background(), "app:model", 120*time.Millisecond)
	if !errors.Is(err, ErrConcurrencyWaitTimeout) {
		t.Fatalf("expected wait timeout, got err=%v decision=%+v", err, decision)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("expected request to wait, elapsed=%s", elapsed)
	}
}

func TestResolveConcurrencyKey(t *testing.T) {
	metadata := map[string]any{"app_id": "app-1"}

	tests := []struct {
		name      string
		scope     string
		metadata  map[string]any
		modelName string
		want      string
		wantErr   error
	}{
		{name: "app scope", scope: "app", metadata: metadata, modelName: "gpt-4o", want: "app-1"},
		{name: "model scope", scope: "model", metadata: metadata, modelName: "gpt-4o", want: "gpt-4o"},
		{name: "app_model scope", scope: "app_model", metadata: metadata, modelName: "gpt-4o", want: "app-1:gpt-4o"},
		{name: "scope alias app_id", scope: "app_id", metadata: metadata, modelName: "gpt-4o", want: "app-1"},
		{name: "scope alias model_name", scope: "model_name", metadata: metadata, modelName: "gpt-4o", want: "gpt-4o"},
		{name: "scope alias app+model", scope: "app_id+model_name", metadata: metadata, modelName: "gpt-4o", want: "app-1:gpt-4o"},
		{name: "missing app_id", scope: "app", metadata: map[string]any{}, modelName: "gpt-4o", wantErr: ErrConcurrencyAppIDRequired},
		{name: "missing model", scope: "model", metadata: metadata, modelName: "", wantErr: ErrConcurrencyModelRequired},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConcurrencyKey(tc.scope, tc.metadata, tc.modelName)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected err %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("key=%q want=%q", got, tc.want)
			}
		})
	}
}
