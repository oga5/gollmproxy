package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// onChunkFunc is called after each SSE data chunk is sent to the client.
type onChunkFunc func(index int, data []byte)

// proxySSEStream reads SSE events from upstream and writes them to the client.
// If transformLine is non-nil, each "data: ..." payload is transformed before writing.
// If onChunk is non-nil, it is called after each data chunk is sent to the client.
func proxySSEStream(
	w http.ResponseWriter,
	upstreamBody io.ReadCloser,
	transformLine func(data []byte) ([]byte, error),
	onChunk onChunkFunc,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	scanner := bufio.NewScanner(upstreamBody)
	// Increase buffer size for large SSE chunks
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	chunkIndex := 0
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

		var outData []byte
		if transformLine != nil {
			transformed, err := transformLine([]byte(data))
			if err != nil {
				// Skip malformed chunks
				continue
			}
			outData = transformed
		} else {
			outData = []byte(data)
		}

		fmt.Fprintf(w, "data: %s\n\n", outData)
		flusher.Flush()

		if onChunk != nil {
			onChunk(chunkIndex, outData)
		}
		chunkIndex++
	}

	return scanner.Err()
}

// setSSEHeaders sets the standard SSE response headers.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}
