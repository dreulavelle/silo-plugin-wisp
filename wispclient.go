package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// wispClient is a thin HTTP client for a single Wisp server. It is cheap to
// construct per call: it holds only configuration, reusing the shared
// *http.Client (and thus its connection pool and timeout).
type wispClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newWispClient(baseURL, token string, httpClient *http.Client) *wispClient {
	return &wispClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    httpClient,
	}
}

// wispQuality is one requested tier in a request-shaped add. It mirrors Wisp's
// qualitySpec: id is the resolution label ("1080p"|"2160p"); is4k is a fallback
// hint Wisp uses when id is unrecognized.
type wispQuality struct {
	ID   string `json:"id"`
	Is4K bool   `json:"is4k"`
}

// wispAddRequest is the request-shaped intake body Wisp's POST /api/add accepts.
// is_anime is always sent (never omitted): Silo's descriptor is authoritative on
// anime classification, and a present flag routes Wisp to the correct root.
type wispAddRequest struct {
	MediaType  string        `json:"media_type"`
	TMDbID     string        `json:"tmdb_id,omitempty"`
	IMDbID     string        `json:"imdb_id,omitempty"`
	TVDbID     string        `json:"tvdb_id,omitempty"`
	Title      string        `json:"title,omitempty"`
	Year       int           `json:"year,omitempty"`
	IsAnime    bool          `json:"is_anime"`
	Qualities  []wispQuality `json:"qualities,omitempty"`
	RequestRef string        `json:"request_ref,omitempty"`
}

// wispStatus is Wisp's GET /api/requests/status response body.
type wispStatus struct {
	State           string   `json:"state"`
	PinnedQualities []string `json:"pinned_qualities"`
	Detail          string   `json:"detail"`
	RequestRef      string   `json:"request_ref"`
}

// add submits a request-shaped intake to Wisp. Wisp is async and idempotent: a
// 2xx means the request was accepted and is now monitored, not that anything is
// pinned yet. Any non-2xx (or transport failure) is returned as an error.
func (c *wispClient) add(ctx context.Context, body wispAddRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode add body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/add", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wisp add returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// status queries a title's fulfillment state. tracked is false when Wisp returns
// 404 (the title is not yet being tracked), which the caller treats as "queued"
// rather than an error. A non-404 non-2xx response, or a transport failure,
// returns an error.
func (c *wispClient) status(ctx context.Context, mediaType, tmdbID, imdbID string) (st *wispStatus, tracked bool, err error) {
	u, err := url.Parse(c.baseURL + "/api/requests/status")
	if err != nil {
		return nil, false, err
	}
	q := u.Query()
	if mediaType != "" {
		q.Set("media_type", mediaType)
	}
	if tmdbID != "" {
		q.Set("tmdb_id", tmdbID)
	}
	if imdbID != "" {
		q.Set("imdb_id", imdbID)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer drain(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("wisp status returned HTTP %d", resp.StatusCode)
	}
	var out wispStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("decode wisp status: %w", err)
	}
	return &out, true, nil
}

// health checks GET /api/healthz. A non-2xx or transport failure is an error.
func (c *wispClient) health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/healthz", nil)
	if err != nil {
		return err
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wisp healthz returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *wispClient) auth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}
