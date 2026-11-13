package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownDrainsActiveStreamBeyondFiveSeconds(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "partial\n")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("HTTP response does not support flushing")
			return
		}
		flusher.Flush()
		close(started)
		<-finish
		_, _ = io.WriteString(w, "complete\n")
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	<-started
	timer := time.AfterFunc(5500*time.Millisecond, func() { close(finish) })
	defer timer.Stop()
	if err := shutdownHTTPServer(server); err != nil {
		t.Fatalf("active response was cut off instead of drained: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "partial\ncomplete\n" {
		t.Fatalf("incomplete response: body=%q error=%v", body, err)
	}
}
