package main

import (
	"encoding/json"
	"fmt"
	"math"
)

// applyPoolingIfNeeded checks whether the upstream embedding response contains
// 2D (T×K) SAE activations and, if so, pools them across the token dimension (T)
// to produce a 1D K-dimensional vector per item.
//
// poolingMethod: "logsumexp" (default), "sum", "mean", "max"
//
// If the upstream already returns 1D embeddings the response is returned unchanged.
func applyPoolingIfNeeded(respBytes []byte, poolingMethod string) ([]byte, error) {
	var raw upstreamEmbeddingResponse
	if err := json.Unmarshal(respBytes, &raw); err != nil {
		// Not parseable — return as-is.
		return respBytes, nil
	}

	needs2D := false
	for _, item := range raw.Data {
		if is2DEmbedding(item.Embedding) {
			needs2D = true
			break
		}
	}
	if !needs2D {
		return respBytes, nil
	}

	if poolingMethod == "" {
		poolingMethod = "logsumexp"
	}

	out := &OpenAIEmbeddingResponse{
		Object: raw.Object,
		Model:  raw.Model,
		Usage:  raw.Usage,
	}
	for _, item := range raw.Data {
		vec, err := poolItem(item.Embedding, poolingMethod)
		if err != nil {
			return nil, fmt.Errorf("pooling item %d: %w", item.Index, err)
		}
		out.Data = append(out.Data, OpenAIEmbedding{
			Object:    item.Object,
			Embedding: vec,
			Index:     item.Index,
		})
	}
	return json.Marshal(out)
}

// is2DEmbedding returns true if v looks like [][]float64.
func is2DEmbedding(v any) bool {
	switch v.(type) {
	case [][]float64:
		return true
	case []any:
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			return false
		}
		_, isSlice := arr[0].([]any)
		return isSlice
	default:
		return false
	}
}

// poolItem converts item.Embedding (1D or 2D) to a pooled 1D vector.
func poolItem(raw any, method string) ([]float64, error) {
	switch v := raw.(type) {
	case []float64:
		return v, nil
	case [][]float64:
		return poolVectors(v, method)
	case []any:
		// Unmarshal from JSON numbers.
		if len(v) == 0 {
			return nil, fmt.Errorf("empty embedding")
		}
		if _, ok := v[0].([]any); ok {
			// 2D: []any of []any
			vecs, err := to2DFloat64(v)
			if err != nil {
				return nil, err
			}
			return poolVectors(vecs, method)
		}
		// 1D
		vec, err := to1DFloat64(v)
		if err != nil {
			return nil, err
		}
		return vec, nil
	default:
		return nil, fmt.Errorf("unexpected embedding type %T", raw)
	}
}

// poolVectors aggregates a T×K matrix across T using the given method.
func poolVectors(vecs [][]float64, method string) ([]float64, error) {
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no token vectors to pool")
	}
	k := len(vecs[0])
	for i, v := range vecs {
		if len(v) != k {
			return nil, fmt.Errorf("token %d has length %d, expected %d", i, len(v), k)
		}
	}

	switch method {
	case "sum":
		return poolSum(vecs, k), nil
	case "mean":
		return poolMean(vecs, k), nil
	case "max":
		return poolMax(vecs, k), nil
	case "logsumexp", "":
		return poolLogsumexp(vecs, k), nil
	default:
		return nil, fmt.Errorf("unknown pooling method %q (supported: logsumexp, sum, mean, max)", method)
	}
}

func poolSum(vecs [][]float64, k int) []float64 {
	out := make([]float64, k)
	for _, v := range vecs {
		for j := range v {
			out[j] += v[j]
		}
	}
	return out
}

func poolMean(vecs [][]float64, k int) []float64 {
	out := poolSum(vecs, k)
	t := float64(len(vecs))
	for j := range out {
		out[j] /= t
	}
	return out
}

func poolMax(vecs [][]float64, k int) []float64 {
	out := make([]float64, k)
	for j := range out {
		out[j] = math.Inf(-1)
	}
	for _, v := range vecs {
		for j := range v {
			if v[j] > out[j] {
				out[j] = v[j]
			}
		}
	}
	return out
}

// poolLogsumexp computes log(Σ_t exp(v[t][k])) for each feature k,
// using the max-subtraction trick for numerical stability.
func poolLogsumexp(vecs [][]float64, k int) []float64 {
	// Find per-feature max across T for numerical stability.
	maxVals := make([]float64, k)
	for j := range maxVals {
		maxVals[j] = math.Inf(-1)
	}
	for _, v := range vecs {
		for j := range v {
			if v[j] > maxVals[j] {
				maxVals[j] = v[j]
			}
		}
	}

	// Σ_t exp(v[t][k] - max[k])
	sumExp := make([]float64, k)
	for _, v := range vecs {
		for j := range v {
			sumExp[j] += math.Exp(v[j] - maxVals[j])
		}
	}

	out := make([]float64, k)
	for j := range out {
		if math.IsInf(maxVals[j], -1) {
			out[j] = math.Inf(-1)
		} else {
			out[j] = maxVals[j] + math.Log(sumExp[j])
		}
	}
	return out
}

// --- helpers ---

func to1DFloat64(arr []any) ([]float64, error) {
	out := make([]float64, len(arr))
	for i, v := range arr {
		f, ok := toFloat64(v)
		if !ok {
			return nil, fmt.Errorf("element %d is not a number: %T", i, v)
		}
		out[i] = f
	}
	return out, nil
}

func to2DFloat64(arr []any) ([][]float64, error) {
	out := make([][]float64, len(arr))
	for i, row := range arr {
		rowArr, ok := row.([]any)
		if !ok {
			return nil, fmt.Errorf("row %d is not an array", i)
		}
		vec, err := to1DFloat64(rowArr)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		out[i] = vec
	}
	return out, nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
