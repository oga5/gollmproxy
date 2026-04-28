package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// fakeBedrockClient records calls to InvokeModel so tests can inspect the input
// that the proxy handed to the AWS SDK.
type fakeBedrockClient struct {
	invokeInput  *bedrockruntime.InvokeModelInput
	invokeOutput []byte
	invokeErr    error
}

func (f *fakeBedrockClient) InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	// Clone the input so asserting in the test isn't affected by the SDK's
	// later handling or by body reuse.
	cloned := &bedrockruntime.InvokeModelInput{}
	if in.ModelId != nil {
		id := *in.ModelId
		cloned.ModelId = &id
	}
	if in.Body != nil {
		cloned.Body = append([]byte(nil), in.Body...)
	}
	if in.ContentType != nil {
		ct := *in.ContentType
		cloned.ContentType = &ct
	}
	if in.Accept != nil {
		ac := *in.Accept
		cloned.Accept = &ac
	}
	f.invokeInput = cloned
	if f.invokeErr != nil {
		return nil, f.invokeErr
	}
	ct := "application/json"
	return &bedrockruntime.InvokeModelOutput{
		Body:        f.invokeOutput,
		ContentType: &ct,
	}, nil
}

func (f *fakeBedrockClient) InvokeModelWithResponseStream(ctx context.Context, _ *bedrockruntime.InvokeModelWithResponseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error) {
	return nil, errors.New("fake bedrock client: streaming not implemented")
}

// withFakeBedrockClient swaps newBedrockClient for the duration of the test so
// the real AWS SDK is never invoked. Returns a pointer to the regions that the
// handler asked the factory for, so tests can assert on per-model overrides.
func withFakeBedrockClient(t *testing.T, client bedrockClient) *string {
	t.Helper()
	prev := newBedrockClient
	t.Cleanup(func() { newBedrockClient = prev })

	var gotRegion string
	newBedrockClient = func(_ context.Context, region string) (bedrockClient, error) {
		gotRegion = region
		return client, nil
	}
	return &gotRegion
}

func TestChatCompletionsBedrockForwardsOpenAIBodyWithStrippedPrefix(t *testing.T) {
	fake := &fakeBedrockClient{
		invokeOutput: []byte(`{"id":"x","object":"chat.completion","created":1,"model":"openai.gpt-oss-20b-1:0","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
	}
	gotRegion := withFakeBedrockClient(t, fake)

	cfg := &Config{
		ModelAliases: map[string]string{"gpt-oss-20b": "bedrock/openai.gpt-oss-20b-1:0"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-oss-20b": {Region: "ap-northeast-1"},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model": "gpt-oss-20b",
		"messages": []map[string]string{
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "hi"},
		},
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	if *gotRegion != "ap-northeast-1" {
		t.Fatalf("expected per-model region override 'ap-northeast-1', got %q", *gotRegion)
	}
	if fake.invokeInput == nil {
		t.Fatal("bedrock InvokeModel was not called")
	}
	if fake.invokeInput.ModelId == nil || *fake.invokeInput.ModelId != "openai.gpt-oss-20b-1:0" {
		t.Fatalf("unexpected ModelId: %v", fake.invokeInput.ModelId)
	}
	if fake.invokeInput.ContentType == nil || *fake.invokeInput.ContentType != "application/json" {
		t.Fatalf("unexpected ContentType: %v", fake.invokeInput.ContentType)
	}
	if fake.invokeInput.Accept == nil || *fake.invokeInput.Accept != "application/json" {
		t.Fatalf("unexpected Accept: %v", fake.invokeInput.Accept)
	}

	var forwarded map[string]any
	if err := json.Unmarshal(fake.invokeInput.Body, &forwarded); err != nil {
		t.Fatalf("failed to unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "openai.gpt-oss-20b-1:0" {
		t.Fatalf("expected body model to be 'openai.gpt-oss-20b-1:0', got %v", forwarded["model"])
	}
	msgs, ok := forwarded["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages in forwarded body, got %#v", forwarded["messages"])
	}

	// The upstream Bedrock response should flow back through to the client.
	var openaiResp OpenAIChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v body=%s", err, rr.Body.String())
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message == nil ||
		openaiResp.Choices[0].Message.Content != "hi" {
		t.Fatalf("unexpected converted choices: %+v", openaiResp.Choices)
	}
	if openaiResp.Usage == nil || openaiResp.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected usage: %+v", openaiResp.Usage)
	}
}

func TestChatCompletionsBedrockExtraParamsMergedIntoBody(t *testing.T) {
	fake := &fakeBedrockClient{
		invokeOutput: []byte(`{"choices":[]}`),
	}
	withFakeBedrockClient(t, fake)

	cfg := &Config{
		ModelAliases: map[string]string{"gpt-oss-20b": "bedrock/openai.gpt-oss-20b-1:0"},
		ModelConfigs: map[string]ModelConfig{
			"gpt-oss-20b": {
				Region:      "ap-northeast-1",
				ExtraParams: map[string]interface{}{"service_tier": "flex"},
			},
		},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-oss-20b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}

	var forwarded map[string]any
	if err := json.Unmarshal(fake.invokeInput.Body, &forwarded); err != nil {
		t.Fatalf("failed to unmarshal forwarded body: %v", err)
	}
	if forwarded["service_tier"] != "flex" {
		t.Fatalf("expected service_tier=flex in forwarded body, got %v", forwarded["service_tier"])
	}
}

func TestChatCompletionsBedrockFallsBackToGlobalRegion(t *testing.T) {
	fake := &fakeBedrockClient{
		invokeOutput: []byte(`{"choices":[]}`),
	}
	gotRegion := withFakeBedrockClient(t, fake)

	cfg := &Config{
		BedrockRegion: "us-west-2",
		ModelAliases:  map[string]string{"claude": "bedrock/anthropic.claude-3"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "claude",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	if *gotRegion != "us-west-2" {
		t.Fatalf("expected fallback to global BedrockRegion, got %q", *gotRegion)
	}
	if fake.invokeInput == nil || fake.invokeInput.ModelId == nil ||
		*fake.invokeInput.ModelId != "anthropic.claude-3" {
		t.Fatalf("unexpected ModelId: %+v", fake.invokeInput)
	}
}

func TestChatCompletionsBedrockWithoutRegionReturnsError(t *testing.T) {
	prev := newBedrockClient
	t.Cleanup(func() { newBedrockClient = prev })

	called := false
	newBedrockClient = func(_ context.Context, _ string) (bedrockClient, error) {
		called = true
		return nil, errors.New("should not be called")
	}

	cfg := &Config{
		ModelAliases: map[string]string{"claude": "bedrock/anthropic.claude-3"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "claude",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("bedrock client factory should not be called when no region is configured")
	}
}

func TestChatCompletionsBedrockSanitizesReasoningInResponse(t *testing.T) {
	fake := &fakeBedrockClient{
		invokeOutput: []byte(`{"id":"x","object":"chat.completion","created":1,"model":"openai.gpt-oss-20b-1:0","choices":[{"index":0,"message":{"role":"assistant","content":"<reasoning>secret thoughts</reasoning>public answer"},"finish_reason":"stop"}]}`),
	}
	withFakeBedrockClient(t, fake)

	cfg := &Config{
		BedrockRegion:           "us-east-1",
		BedrockIncludeReasoning: false,
		ModelAliases:            map[string]string{"gpt-oss-20b": "bedrock/openai.gpt-oss-20b-1:0"},
	}
	handler := newHandlerWithConfig(t, cfg)

	rr := postJSON(t, handler, "/v1/chat/completions", map[string]any{
		"model":    "gpt-oss-20b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", rr.Code, rr.Body.String())
	}
	var openaiResp OpenAIChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v body=%s", err, rr.Body.String())
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message == nil {
		t.Fatalf("unexpected choices: %+v", openaiResp.Choices)
	}
	content := openaiResp.Choices[0].Message.Content
	if content != "public answer" {
		t.Fatalf("expected reasoning to be stripped, got %q", content)
	}
}
