package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
)

// bedrockClient is the subset of bedrockruntime.Client methods used by the proxy.
// It exists so tests can inject a fake client without contacting AWS.
type bedrockClient interface {
	InvokeModel(ctx context.Context, params *bedrockruntime.InvokeModelInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
	InvokeModelWithResponseStream(ctx context.Context, params *bedrockruntime.InvokeModelWithResponseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error)
}

// newBedrockClient constructs a Bedrock client for the given region.
// Tests override this variable to return a fake client.
var newBedrockClient = func(ctx context.Context, region string) (bedrockClient, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return bedrockruntime.NewFromConfig(awsCfg), nil
}

func handleBedrockProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model, logModelName string, metadata map[string]any, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	_, strippedBody := extractAndStripMetadata(bodyBytes)
	modifiedBody := mergeExtraParams(rewriteModelField(strippedBody, model), perModelCfg.ExtraParams)
	upstreamCtx, cancel := withUpstreamTimeout(r.Context(), !req.Stream, perModelCfg.UpstreamNonStreamTimeout)
	defer cancel()

	region := perModelCfg.Region
	if region == "" {
		region = cfg.BedrockRegion
	}
	if region == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "bedrock region not configured", "server_error")
		return
	}

	client, err := newBedrockClient(upstreamCtx, region)
	if err != nil {
		slog.Error("failed to load AWS config", "request_id", reqID, "region", region, "error", err)
		writeErrorJSON(w, http.StatusInternalServerError, "failed to load AWS configuration", "server_error")
		return
	}

	if req.Stream {
		handleBedrockStream(w, r, cfg, logger, req, model, logModelName, metadata, modifiedBody, reqID, start, client, region)
		return
	}

	resp, err := client.InvokeModel(upstreamCtx, &bedrockruntime.InvokeModelInput{
		ModelId:     &model,
		Body:        modifiedBody,
		ContentType: stringPtr("application/json"),
		Accept:      stringPtr("application/json"),
	})
	if err != nil {
		statusCode := http.StatusBadGateway
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			slog.Error("bedrock API error", "request_id", reqID, "region", region, "model", model, "code", apiErr.ErrorCode(), "message", apiErr.ErrorMessage())
			statusCode = http.StatusBadGateway
		} else {
			slog.Error("bedrock invoke failed", "request_id", reqID, "region", region, "model", model, "error", err)
		}
		writeErrorJSON(w, statusCode, "bedrock API error", "server_error")
		return
	}

	respBody := sanitizeBedrockResponseBody(resp.Body, cfg.BedrockIncludeReasoning)
	if !json.Valid(respBody) {
		slog.Error("bedrock response was not valid JSON", "request_id", reqID, "region", region, "model", model)
		writeErrorJSON(w, http.StatusBadGateway, "bedrock returned invalid JSON", "server_error")
		return
	}

	contentType := "application/json"
	if resp.ContentType != nil && *resp.ContentType != "" {
		contentType = *resp.ContentType
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write(respBody)

	var usage *OpenAIUsage
	var openaiResp OpenAIChatResponse
	if json.Unmarshal(respBody, &openaiResp) == nil {
		usage = openaiResp.Usage
	}

	logRequest(logger, cfg, reqID, r, "bedrock", logModelName, false, http.StatusOK, start, string(modifiedBody), string(respBody), req.User, metadata, usage)
}

func handleBedrockStream(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model, logModelName string, metadata map[string]any, modifiedBody []byte, reqID string, start time.Time, client bedrockClient, region string) {
	resp, err := client.InvokeModelWithResponseStream(r.Context(), &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     &model,
		Body:        modifiedBody,
		ContentType: stringPtr("application/json"),
		Accept:      stringPtr("application/json"),
	})
	if err != nil {
		statusCode := http.StatusBadGateway
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			slog.Error("bedrock stream API error", "request_id", reqID, "region", region, "model", model, "code", apiErr.ErrorCode(), "message", apiErr.ErrorMessage())
		} else {
			slog.Error("bedrock stream invoke failed", "request_id", reqID, "region", region, "model", model, "error", err)
		}
		writeErrorJSON(w, statusCode, "bedrock API error", "server_error")
		return
	}
	defer resp.GetStream().Close()

	setSSEHeaders(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorJSON(w, http.StatusInternalServerError, "streaming not supported", "server_error")
		return
	}

	var filter *ReasoningStripper
	if !cfg.BedrockIncludeReasoning {
		filter = NewReasoningStripper()
	}

	var usage *OpenAIUsage
	var usageChunk []byte
	var chunks []string
	for event := range resp.GetStream().Events() {
		switch v := event.(type) {
		case *brtypes.ResponseStreamMemberChunk:
			result := sanitizeBedrockStreamChunk(v.Value.Bytes, filter)
			if result.Usage != nil {
				usage = result.Usage
				usageChunk = buildUsageStreamChunk(result.Body, result.Usage)
			}
			if result.Skip {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", result.Body)
			flusher.Flush()
			if cfg.LogResponseBody {
				chunks = append(chunks, string(result.Body))
			}
		default:
			slog.Warn("unexpected bedrock stream event", "request_id", reqID, "event_type", fmt.Sprintf("%T", v))
		}
	}

	if err := resp.GetStream().Err(); err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			slog.Error("bedrock stream read error", "request_id", reqID, "region", region, "model", model, "code", apiErr.ErrorCode(), "message", apiErr.ErrorMessage())
		} else {
			slog.Error("bedrock stream read error", "request_id", reqID, "region", region, "model", model, "error", err)
		}
		logRequest(logger, cfg, reqID, r, "bedrock", logModelName, true, http.StatusBadGateway, start, string(modifiedBody), "", req.User, metadata, nil)
		return
	}

	if len(usageChunk) > 0 {
		fmt.Fprintf(w, "data: %s\n\n", usageChunk)
		flusher.Flush()
		if cfg.LogResponseBody {
			chunks = append(chunks, string(usageChunk))
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	respBody := strings.Join(chunks, "\n")
	logRequest(logger, cfg, reqID, r, "bedrock", logModelName, true, http.StatusOK, start, string(modifiedBody), respBody, req.User, metadata, usage)
}

func stringPtr(s string) *string {
	return &s
}
