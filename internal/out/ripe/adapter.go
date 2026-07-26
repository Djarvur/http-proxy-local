// Package ripe implements port.GeoSource: it fetches the raw delegation text
// from a configurable HTTPS URL (default: RIPE's public statistics file).
package ripe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Adapter fetches geo source text over HTTP(S). The zero value is NOT usable;
// construct with New.
type Adapter struct {
	client *http.Client
}

// New returns an Adapter whose HTTP client has the given timeout. The
// timeout bounds a full request including body read.
func New(timeout time.Duration) *Adapter {
	return &Adapter{
		client: &http.Client{Timeout: timeout},
	}
}

// Fetch retrieves the body at url as bytes. A non-2xx status, network error,
// or context cancellation (including timeout) yields an error.
func (a *Adapter) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ripe: build request for %q: %w", url, err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ripe: fetch %q: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ripe: %q returned status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ripe: read body from %q: %w", url, err)
	}
	return body, nil
}
