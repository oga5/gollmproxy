package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatCompletions_ConcurrencyControl_StreamHoldsSlotUntilDone(t *testing.T) {
	upstreamStarted := make(chan struct{}, 4)
	release := make(chan struct{}, 4)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamStarted <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
			f.Flush()
		}
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	controller, err := NewKeyedConcurrencyController(1, 1)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	cfg := &Config{
		OpenAIAPIKey:              "k",
		OpenAIBaseURL:             upstream.URL,
		ConcurrencyControlEnabled: true,
		ConcurrencyControlScope:   "model",
		ConcurrencyMaxConcurrency: 1,
		ConcurrencyMaxQueue:       1,
		ConcurrencyMaxWait:        time.Second,
		ConcurrencyController:     controller,
		ModelAliases: map[string]string{
			"gpt-4o": "openai/gpt-4o",
		},
		ModelConfigs: map[string]ModelConfig{
			"gpt-4o": {APIKey: "k", APIBase: upstream.URL},
		},
	}

	server := httptest.NewServer(newHandlerWithConfig(t, cfg))
	defer server.Close()

	post := func() int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model":    "gpt-4o",
			"stream":   true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	done1 := make(chan int, 1)
	go func() {
		done1 <- post()
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach upstream")
	}

	done2 := make(chan int, 1)
	go func() {
		done2 <- post()
	}()

	select {
	case <-upstreamStarted:
		t.Fatal("second request should be queued while first stream is running")
	case <-time.After(150 * time.Millisecond):
	}

	release <- struct{}{}

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("second request did not start after first stream finished")
	}

	release <- struct{}{}

	if code := <-done1; code != http.StatusOK {
		t.Fatalf("first status=%d", code)
	}
	if code := <-done2; code != http.StatusOK {
		t.Fatalf("second status=%d", code)
	}
}
