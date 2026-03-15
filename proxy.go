package main

import (
	"io"
	"net/http"
	"strings"
)

// forwardRequest forwards an HTTP request to targetURL.
// modifyReq is called to inject auth headers/query params before sending.
// Returns the upstream response status code.
func forwardRequest(w http.ResponseWriter, r *http.Request, targetURL string, modifyReq func(*http.Request)) (int, error) {
	// Read the original request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}

	// Copy relevant headers
	for _, h := range []string{"Content-Type", "Accept", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// Let caller inject auth
	if modifyReq != nil {
		modifyReq(req)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body (supports SSE if upstream sends it)
	if isSSE(resp) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			io.Copy(w, resp.Body)
			return resp.StatusCode, nil
		}
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}

	return resp.StatusCode, nil
}

func isSSE(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

// writeErrorJSON writes an OpenAI-format error response.
func writeErrorJSON(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	io.WriteString(w, `{"error":{"message":"`+escapeJSON(message)+`","type":"`+errType+`"}}`)
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
