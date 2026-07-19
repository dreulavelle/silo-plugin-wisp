package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// wispClient is a thin HTTP client for a single Wisp server. It is cheap to
// construct per call: it holds only configuration, reusing the shared
// *http.Client (and thus its connection pool and timeout). The base URL is
// parsed and validated once at construction so every endpoint is built
// structurally rather than by string concatenation.
type wispClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

// newWispClient parses and validates the base URL and returns a ready client.
// An invalid base URL (bad scheme, missing host, or a stray query/fragment) is
// an error, so callers surface it instead of silently building a broken request.
func newWispClient(rawURL, token string, httpClient *http.Client) (*wispClient, error) {
	base, err := parseWispBase(rawURL)
	if err != nil {
		return nil, err
	}
	return &wispClient{base: base, token: strings.TrimSpace(token), http: httpClient}, nil
}

// parseWispBase validates a Wisp base URL: absolute http(s), with a host and no
// query or fragment. It is the single source of truth for URL validity, shared
// by the client and by Validate.
func parseWispBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL must use the http or https scheme")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL must include a host")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("URL must not contain a query or fragment")
	}
	// wisp_url is a non-secret admin field that is echoed back in Validate and
	// TestConnection messages, so it must not be a place to put a password.
	if u.User != nil {
		return nil, fmt.Errorf("URL must not contain credentials; use the Wisp Token field instead")
	}
	return u, nil
}

// httpStatusError is a non-2xx response from Wisp. It carries the status code so
// callers can tell a permanent rejection (4xx — bad token, malformed query) from
// a transient one (5xx, 429), which is the difference between failing a target
// and leaving it queued for the next poll.
type httpStatusError struct {
	op   string
	Code int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("wisp %s returned HTTP %d", e.op, e.Code)
}

// permanentHTTP reports whether err is a Wisp response that will keep recurring
// until a human changes something: a 4xx other than 404 (handled separately as
// "not yet tracked") and 429 (rate limited — retry later). Transport failures
// and decode failures are not permanent: they are exactly the transient
// conditions a re-poll is meant to ride out.
func permanentHTTP(err error) bool {
	var he *httpStatusError
	if !errors.As(err, &he) {
		return false
	}
	return he.Code >= 400 && he.Code < 500 &&
		he.Code != http.StatusNotFound && he.Code != http.StatusTooManyRequests
}

// endpoint joins path elements onto the base URL structurally. JoinPath returns
// a copy, so the returned *url.URL is safe to mutate (e.g. to set a query).
func (c *wispClient) endpoint(elem ...string) *url.URL {
	return c.base.JoinPath(elem...)
}

// wispQuality is one requested tier in a request-shaped add. It mirrors Wisp's
// qualitySpec: id is the resolution label ("1080p"|"2160p"); is4k is a fallback
// hint Wisp uses when id is unrecognized.
type wispQuality struct {
	ID   string `json:"id"`
	Is4K bool   `json:"is4k"`
}

// wispAddRequest is the request-shaped intake body Wisp's POST /api/add accepts.
// is_anime and qualities are always emitted (no omitempty): Silo's descriptor is
// authoritative on anime classification, and the caller guarantees a non-empty
// qualities list, so a present field is always meaningful on the wire.
type wispAddRequest struct {
	MediaType  string        `json:"media_type"`
	TMDbID     string        `json:"tmdb_id,omitempty"`
	IMDbID     string        `json:"imdb_id,omitempty"`
	TVDbID     string        `json:"tvdb_id,omitempty"`
	Title      string        `json:"title,omitempty"`
	Year       int           `json:"year,omitempty"`
	IsAnime    bool          `json:"is_anime"`
	Qualities  []wispQuality `json:"qualities"`
	RequestRef string        `json:"request_ref,omitempty"`
}

// wispStatus is Wisp's GET /api/requests/status response body.
type wispStatus struct {
	State           string   `json:"state"`
	PinnedQualities []string `json:"pinned_qualities"`
	Detail          string   `json:"detail"`
	RequestRef      string   `json:"request_ref"`
}

// add submits a request-shaped intake to Wisp. Wisp's async intake answers with
// exactly 202 Accepted; any other status (including a 200/201/204, which signals
// a proxy or a wrong-path handler rather than the intake endpoint) is an error.
// A 202 means the request was accepted and is now monitored, not that anything
// is pinned yet.
func (c *wispClient) add(ctx context.Context, body wispAddRequest) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode add body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("api", "add").String(), bytes.NewReader(payload))
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

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("wisp add returned HTTP %d (expected 202 Accepted)", resp.StatusCode)
	}
	return nil
}

// status queries a title's fulfillment state. tracked is false when Wisp returns
// 404 (the title is not yet being tracked), which the caller treats as "queued"
// rather than an error. A non-404 non-2xx response, or a transport failure,
// returns an error.
func (c *wispClient) status(ctx context.Context, mediaType, tmdbID, imdbID string) (st *wispStatus, tracked bool, err error) {
	u := c.endpoint("api", "requests", "status")
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
		return nil, false, &httpStatusError{op: "status", Code: resp.StatusCode}
	}
	var out wispStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("decode wisp status: %w", err)
	}
	return &out, true, nil
}

// health checks GET /api/healthz. A non-2xx or transport failure is an error.
func (c *wispClient) health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("api", "healthz").String(), nil)
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
