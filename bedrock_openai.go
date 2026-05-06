package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

func handleBedrockOpenAIProvider(w http.ResponseWriter, r *http.Request, cfg *Config, logger *RequestLogger, req OpenAIChatRequest, model string, bodyBytes []byte, reqID string, start time.Time, perModelCfg ModelConfig) {
	region := perModelCfg.Region
	if region == "" {
		region = cfg.BedrockRegion
	}
	if region == "" {
		writeErrorJSON(w, http.StatusInternalServerError, "bedrock region not configured", "server_error")
		return
	}

	_, strippedBody := extractAndStripMetadata(bodyBytes)
	modifiedBody := mergeExtraParams(rewriteModelField(strippedBody, model), perModelCfg.ExtraParams)

	baseURL := perModelCfg.APIBase
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	}
	targetURL := strings.TrimRight(baseURL, "/") + "/openai/v1/chat/completions"

	upstreamCtx, cancel := withUpstreamTimeout(r.Context(), !req.Stream)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(upstreamCtx, awsconfig.WithRegion(region))
	if err != nil {
		slog.Error("failed to load AWS config", "request_id", reqID, "region", region, "error", err)
		writeErrorJSON(w, http.StatusInternalServerError, "failed to load AWS configuration", "server_error")
		return
	}
	creds, err := awsCfg.Credentials.Retrieve(upstreamCtx)
	if err != nil {
		slog.Error("failed to retrieve AWS credentials", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusInternalServerError, "failed to retrieve AWS credentials", "server_error")
		return
	}

	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, "POST", targetURL, bytes.NewReader(modifiedBody))
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to create upstream request", "server_error")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")

	h := sha256.New()
	h.Write(modifiedBody)
	bodyHash := hex.EncodeToString(h.Sum(nil))

	signer := v4.NewSigner()
	if err := signer.SignHTTP(upstreamCtx, creds, upstreamReq, bodyHash, "bedrock", region, time.Now()); err != nil {
		slog.Error("failed to sign bedrock openai request", "request_id", reqID, "error", err)
		writeErrorJSON(w, http.StatusInternalServerError, "failed to sign request", "server_error")
		return
	}

	slog.Info("passthrough", "provider", "bedrock_openai", "model", model, "region", region)

	resp, err := httpClient.Do(upstreamReq)
	if err != nil {
		slog.Error("bedrock openai upstream error", "request_id", reqID, "error", sanitizeUpstreamError(err))
		writeErrorJSON(w, http.StatusBadGateway, "upstream connection failed", "server_error")
		return
	}
	defer resp.Body.Close()

	handleOpenAICompatResponse(w, r, cfg, logger, req, "bedrock_openai", model, resp, bodyBytes, reqID, start)
}
