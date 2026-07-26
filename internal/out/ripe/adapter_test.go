package ripe_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Djarvur/http-proxy-local/internal/out/ripe"
)

func TestFetch_Success(t *testing.T) {
	t.Parallel()
	const body = "ripencc|RU|ipv4|1.2.3.0|256|x|x\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := ripe.New(5 * time.Second).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestFetch_Non2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := ripe.New(5 * time.Second).Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestFetch_Timeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := ripe.New(10 * time.Second).Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestFetch_CancelledContext(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	_, err := ripe.New(10 * time.Second).Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected cancelled-context error, got nil")
	}
}
