package proxy

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readSSE reads data-until-blank-line chunks from a streaming response,
// recording arrival timestamps.
func readSSE(t *testing.T, resp *http.Response, n int) []string {
	t.Helper()
	var chunks []string
	reader := bufio.NewReader(resp.Body)
	for len(chunks) < n {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v (got %d chunks)", err, len(chunks))
		}
		if strings.HasPrefix(line, "data:") {
			chunks = append(chunks, line)
		}
	}
	return chunks
}

func TestSSEStreamPassthrough(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range strings.Split(sseBody, "\n\n") {
			_, _ = w.Write([]byte(chunk + "\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	chunks := readSSE(t, resp, 3)
	if !strings.Contains(chunks[0], "\"he\"") || !strings.Contains(chunks[1], "\"llo\"") {
		t.Fatalf("chunks out of order: %v", chunks)
	}
	if !strings.Contains(chunks[2], "[DONE]") {
		t.Fatalf("missing [DONE] terminator: %v", chunks[2])
	}
}

func TestStreamUpstreamDisconnectMidStream(t *testing.T) {
	// Upstream sends one chunk then dies without terminating the stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // sever the connection mid-stream
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	// The first chunk must arrive, then the client must see the stream
	// close (an error/EOF) rather than hang forever.
	reader := bufio.NewReader(resp.Body)
	done := make(chan error, 1)
	go func() {
		for {
			_, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-done: // stream terminated: good
	case <-time.After(5 * time.Second):
		t.Fatal("stream hung after upstream disconnect")
	}
}
