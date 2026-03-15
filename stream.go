package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// proxySSEStream reads SSE events from upstream and writes them to the client.
// If transformLine is non-nil, each "data: ..." payload is transformed before writing.
// Returns the accumulated data content for logging.
func proxySSEStream(
	w http.ResponseWriter,
	upstreamBody io.ReadCloser,
	transformLine func(data []byte) ([]byte, error),
) (string, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return "", fmt.Errorf("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var accumulated strings.Builder
	scanner := bufio.NewScanner(upstreamBody)
	// Increase buffer size for large SSE chunks
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			// Write non-data lines as-is (empty lines for SSE framing)
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			break
		}

		if transformLine != nil {
			transformed, err := transformLine([]byte(data))
			if err != nil {
				// Skip malformed chunks
				continue
			}
			accumulated.Write(transformed)
			fmt.Fprintf(w, "data: %s\n\n", transformed)
		} else {
			accumulated.WriteString(data)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		flusher.Flush()
	}

	return accumulated.String(), scanner.Err()
}

// setSSEHeaders sets the standard SSE response headers.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}
