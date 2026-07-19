package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"
)

// testRouter returns a router whose HTTP client has a short timeout so a hung
// stub fails the test rather than the suite.
func testRouter() *router {
	r := newRouter(hclog.NewNullLogger())
	r.http = &http.Client{Timeout: 5 * time.Second}
	return r
}

// connFor builds a RouterConnection carrying wisp_url/wisp_token the way Silo's
// admin form delivers them: inside the config Struct keyed by field name.
func connFor(id, wispURL, token string) *pluginv1.RouterConnection {
	fields := map[string]*structpb.Value{
		"wisp_url": structpb.NewStringValue(wispURL),
	}
	if token != "" {
		fields["wisp_token"] = structpb.NewStringValue(token)
	}
	return &pluginv1.RouterConnection{
		Id:     id,
		Config: &structpb.Struct{Fields: fields},
	}
}

func decodeAdd(t *testing.T, body io.Reader) wispAddRequest {
	t.Helper()
	var got wispAddRequest
	if err := json.NewDecoder(body).Decode(&got); err != nil {
		t.Fatalf("decode add body: %v", err)
	}
	return got
}

func TestManifestEmbedsAndValidates(t *testing.T) {
	m, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("embedded manifest failed to load: %v", err)
	}
	if m.GetPluginId() != "wisp" {
		t.Errorf("pluginId = %q, want wisp", m.GetPluginId())
	}
	caps := m.GetCapabilities()
	if len(caps) != 1 || caps[0].GetType() != "request_router.v1" || caps[0].GetId() != "wisp-requests" {
		t.Fatalf("capabilities = %+v, want single request_router.v1/wisp-requests", caps)
	}
	cs := caps[0].GetConfigSchema()
	if len(cs) != 1 || cs[0].GetKey() != "connection" {
		t.Fatalf("configSchema = %+v, want single connection", cs)
	}
	keys := map[string]bool{}
	for _, f := range cs[0].GetAdminForm().GetFields() {
		keys[f.GetKey()] = true
	}
	if !keys["wisp_url"] || !keys["wisp_token"] {
		t.Errorf("admin form fields = %v, want wisp_url and wisp_token", keys)
	}
}

func TestFulfillMapsDescriptorToAddBody(t *testing.T) {
	var got wispAddRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/add" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		got = decodeAdd(t, r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"monitoring":true,"state":"queued"}`)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request: &pluginv1.RequestDescriptor{
			MediaType:   "series",
			Title:       "Frieren",
			Year:        2023,
			IsAnime:     true,
			ExternalIds: map[string]string{"tmdb": "209867", "imdb": "tt22248376", "tvdb": "424536"},
		},
		Qualities: []*pluginv1.RequestedQuality{
			{Id: "1080p"},
			{Id: "2160p", Is4K: true},
		},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}

	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}

	// Body mapping.
	if got.MediaType != "series" || got.Title != "Frieren" || got.Year != 2023 {
		t.Errorf("add body core = %+v", got)
	}
	if got.TMDbID != "209867" || got.IMDbID != "tt22248376" || got.TVDbID != "424536" {
		t.Errorf("add body ids = %+v", got)
	}
	if !got.IsAnime {
		t.Errorf("is_anime = false, want true (authoritative from descriptor)")
	}
	if len(got.Qualities) != 2 || got.Qualities[0].ID != "1080p" || got.Qualities[1].ID != "2160p" || !got.Qualities[1].Is4K {
		t.Errorf("qualities = %+v", got.Qualities)
	}
	if got.RequestRef != "tmdb:209867" {
		t.Errorf("request_ref = %q, want tmdb:209867", got.RequestRef)
	}

	// Response: one queued target per requested quality, stable external id.
	if len(resp.GetTargets()) != 2 {
		t.Fatalf("targets = %d, want 2", len(resp.GetTargets()))
	}
	for _, tgt := range resp.GetTargets() {
		if tgt.GetStatus() != statusQueued {
			t.Errorf("target %s status = %q, want queued", tgt.GetQuality(), tgt.GetStatus())
		}
		if tgt.GetExternalId() != "tmdb:209867" {
			t.Errorf("external_id = %q, want tmdb:209867", tgt.GetExternalId())
		}
		if tgt.GetConnectionId() != "c1" {
			t.Errorf("connection_id = %q, want c1", tgt.GetConnectionId())
		}
	}
}

func TestFulfillIsAnimeFalseIsSent(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request: &pluginv1.RequestDescriptor{
			MediaType:   "movie",
			Title:       "Inception",
			IsAnime:     false,
			ExternalIds: map[string]string{"tmdb": "27205"},
		},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}
	if _, err := testRouter().Fulfill(context.Background(), req); err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	// is_anime must be PRESENT in the wire JSON (no omitempty), so a false is sent
	// as an authoritative value rather than dropped. Assert on the raw bytes — a
	// struct decode would default a missing field to false and mask the bug.
	if !strings.Contains(string(raw), `"is_anime"`) {
		t.Fatalf("is_anime field absent from wire body: %s", raw)
	}
	var got wispAddRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode add body: %v", err)
	}
	if got.IsAnime {
		t.Errorf("is_anime = true, want false")
	}
}

func TestFulfillSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "secret-token")},
	}
	if _, err := testRouter().Fulfill(context.Background(), req); err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
}

func TestFulfillZeroTargetsOnWisp5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}
	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill should not return a gRPC error on wisp failure: %v", err)
	}
	if len(resp.GetTargets()) != 0 {
		t.Errorf("targets = %d, want 0 on wisp 5xx", len(resp.GetTargets()))
	}
	if resp.GetMessage() == "" {
		t.Errorf("expected a top-level failure message")
	}
}

// A non-202 2xx (e.g. 200/204 from a proxy or the wrong wisp path) must be
// treated as a failure, not a silent success — wisp's add contract is exactly
// 202 Accepted.
func TestFulfillRejectsNon202Success(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			req := &pluginv1.FulfillRequest{
				Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
				Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
				Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
			}
			resp, err := testRouter().Fulfill(context.Background(), req)
			if err != nil {
				t.Fatalf("Fulfill should not return a gRPC error: %v", err)
			}
			if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
				t.Errorf("HTTP %d: want zero targets + message, got %d targets / msg %q",
					code, len(resp.GetTargets()), resp.GetMessage())
			}
		})
	}
}

func TestFulfillReturnsFastDespiteLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}
	start := time.Now()
	resp, err := testRouter().Fulfill(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if len(resp.GetTargets()) != 1 {
		t.Errorf("targets = %d, want 1", len(resp.GetTargets()))
	}
	if elapsed > 2*time.Second {
		t.Errorf("Fulfill took %s, well under the 60s deadline expected", elapsed)
	}
}

func TestFulfillNoConnection(t *testing.T) {
	req := &pluginv1.FulfillRequest{
		Request:   &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities: []*pluginv1.RequestedQuality{{Id: "1080p"}},
	}
	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("want zero targets + message, got %+v", resp)
	}
}

func TestFulfillRejectsMultipleConnections(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request:   &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities: []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{
			connFor("c1", srv.URL, ""),
			connFor("c2", srv.URL, ""),
		},
	}
	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("want zero targets + message for multiple connections, got %+v", resp)
	}
	if called {
		t.Errorf("Wisp must not be called when multiple connections are configured")
	}
}

func TestFulfillEmptyQualities(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	req := &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities:   nil,
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}
	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("want zero targets + message for empty qualities, got %+v", resp)
	}
	if called {
		t.Errorf("Wisp must not be called when no qualities are requested")
	}
}

// Duplicate quality ids would produce two FulfillmentTargets identical in
// quality, connection_id and external_id — the host matches status back by
// (quality, connection_id), so it could never resolve them.
func TestFulfillDedupesQualities(t *testing.T) {
	var got wispAddRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeAdd(t, r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	resp, err := testRouter().Fulfill(context.Background(), &pluginv1.FulfillRequest{
		Request: &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Qualities: []*pluginv1.RequestedQuality{
			{Id: "1080p"},
			{Id: "2160p", Is4K: true},
			{Id: "1080p"},
			{Id: "1080P"}, // case-folded duplicate
		},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	})
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}

	if len(got.Qualities) != 2 {
		t.Errorf("wisp qualities = %+v, want 2 deduped", got.Qualities)
	}
	// First-seen order is preserved, and the first entry's flags win.
	if len(got.Qualities) == 2 && (got.Qualities[0].ID != "1080p" || got.Qualities[1].ID != "2160p") {
		t.Errorf("wisp qualities = %+v, want [1080p 2160p] in first-seen order", got.Qualities)
	}

	seen := map[string]int{}
	for _, tgt := range resp.GetTargets() {
		seen[tgt.GetQuality()]++
	}
	if len(resp.GetTargets()) != 2 {
		t.Errorf("targets = %d, want 2", len(resp.GetTargets()))
	}
	for q, n := range seen {
		if n > 1 {
			t.Errorf("quality %q emitted %d times; targets must be unique per (quality, connection)", q, n)
		}
	}
}

func TestFulfillNoIdentity(t *testing.T) {
	req := &pluginv1.FulfillRequest{
		Request:     &pluginv1.RequestDescriptor{MediaType: "movie"},
		Qualities:   []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", "http://wisp:8080", "")},
	}
	resp, err := testRouter().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Errorf("want zero targets + message when no ids, got %+v", resp)
	}
}

func TestCheckStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		pinned     []string
		notFound   bool
		fail       bool // stub returns 500
		reqQuals   []string
		wantStatus map[string]string // quality -> host status
	}{
		{
			name:       "completed pins one quality",
			state:      "completed",
			pinned:     []string{"1080p"},
			reqQuals:   []string{"1080p", "2160p"},
			wantStatus: map[string]string{"1080p": statusCompleted, "2160p": statusQueued},
		},
		{
			name:       "completed pins all",
			state:      "completed",
			pinned:     []string{"1080p", "2160p"},
			reqQuals:   []string{"1080p", "2160p"},
			wantStatus: map[string]string{"1080p": statusCompleted, "2160p": statusCompleted},
		},
		{
			name:       "failed marks all failed",
			state:      "failed",
			reqQuals:   []string{"1080p", "2160p"},
			wantStatus: map[string]string{"1080p": statusFailed, "2160p": statusFailed},
		},
		{
			name:       "queued stays queued",
			state:      "queued",
			reqQuals:   []string{"1080p"},
			wantStatus: map[string]string{"1080p": statusQueued},
		},
		{
			name:       "untracked 404 is queued",
			notFound:   true,
			reqQuals:   []string{"1080p"},
			wantStatus: map[string]string{"1080p": statusQueued},
		},
		{
			name:       "transport error is queued",
			fail:       true,
			reqQuals:   []string{"1080p"},
			wantStatus: map[string]string{"1080p": statusQueued},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/requests/status" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				switch {
				case tc.fail:
					http.Error(w, "boom", http.StatusInternalServerError)
				case tc.notFound:
					http.NotFound(w, r)
				default:
					_ = json.NewEncoder(w).Encode(wispStatus{
						State: tc.state, PinnedQualities: tc.pinned, Detail: "detail text",
					})
				}
			}))
			defer srv.Close()

			targets := make([]*pluginv1.TargetRef, 0, len(tc.reqQuals))
			for _, q := range tc.reqQuals {
				targets = append(targets, &pluginv1.TargetRef{Quality: q, ConnectionId: "c1", ExternalId: "tmdb:1"})
			}
			req := &pluginv1.CheckStatusRequest{
				Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
				Targets:     targets,
				Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
			}
			resp, err := testRouter().CheckStatus(context.Background(), req)
			if err != nil {
				t.Fatalf("CheckStatus error: %v", err)
			}
			if len(resp.GetStatuses()) != len(tc.reqQuals) {
				t.Fatalf("statuses = %d, want %d", len(resp.GetStatuses()), len(tc.reqQuals))
			}
			for _, s := range resp.GetStatuses() {
				want := tc.wantStatus[s.GetQuality()]
				if s.GetStatus() != want {
					t.Errorf("quality %s status = %q, want %q", s.GetQuality(), s.GetStatus(), want)
				}
			}
		})
	}
}

func TestCheckStatusQueriesOncePerConnection(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(wispStatus{State: "completed", PinnedQualities: []string{"1080p", "2160p"}})
	}))
	defer srv.Close()

	req := &pluginv1.CheckStatusRequest{
		Request: &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
		Targets: []*pluginv1.TargetRef{
			{Quality: "1080p", ConnectionId: "c1"},
			{Quality: "2160p", ConnectionId: "c1"},
		},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	}
	if _, err := testRouter().CheckStatus(context.Background(), req); err != nil {
		t.Fatalf("CheckStatus error: %v", err)
	}
	if calls != 1 {
		t.Errorf("wisp queried %d times, want 1 (deduped per connection)", calls)
	}
}

// The status query string is the whole point of the call — a poll that reaches
// the right path with the wrong (or no) identity is silently useless. Assert on
// the query itself, not just the path.
func TestCheckStatusSendsIdentityQuery(t *testing.T) {
	tests := []struct {
		name       string
		desc       *pluginv1.RequestDescriptor
		externalID string
		want       map[string]string
	}{
		{
			name: "descriptor supplies tmdb",
			desc: &pluginv1.RequestDescriptor{
				MediaType:   "movie",
				ExternalIds: map[string]string{"tmdb": "27205"},
			},
			externalID: "tmdb:27205",
			want:       map[string]string{"tmdb_id": "27205", "media_type": "movie"},
		},
		{
			name: "descriptor supplies imdb",
			desc: &pluginv1.RequestDescriptor{
				MediaType:   "series",
				ExternalIds: map[string]string{"imdb": "tt22248376"},
			},
			externalID: "imdb:tt22248376",
			want:       map[string]string{"imdb_id": "tt22248376", "media_type": "series"},
		},
		{
			// The host is not obliged to populate CheckStatusRequest.request.
			// Fulfill stamps external_id for exactly this case.
			name:       "nil descriptor falls back to target external_id",
			desc:       nil,
			externalID: "tmdb:209867",
			want:       map[string]string{"tmdb_id": "209867"},
		},
		{
			name:       "id-less descriptor falls back to target external_id",
			desc:       &pluginv1.RequestDescriptor{MediaType: "movie"},
			externalID: "imdb:tt1375666",
			want:       map[string]string{"imdb_id": "tt1375666", "media_type": "movie"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/requests/status" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				got = r.URL.Query()
				_ = json.NewEncoder(w).Encode(wispStatus{State: "queued", Detail: "resolving stream"})
			}))
			defer srv.Close()

			resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     tc.desc,
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: tc.externalID}},
				Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
			})
			if err != nil {
				t.Fatalf("CheckStatus error: %v", err)
			}
			if got == nil {
				t.Fatalf("Wisp was never queried (status %+v)", resp.GetStatuses())
			}
			for k, want := range tc.want {
				if got.Get(k) != want {
					t.Errorf("query %s = %q, want %q (full query: %v)", k, got.Get(k), want, got)
				}
			}
			if got.Get("tmdb_id") == "" && got.Get("imdb_id") == "" {
				t.Errorf("identity-free query issued: %v", got)
			}
		})
	}
}

// With no identity from either source, the plugin must not issue a bare query —
// Wisp answers 400, which would previously have been mapped to "queued" forever.
func TestCheckStatusNoIdentityDoesNotQuery(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(wispStatus{State: "queued"})
	}))
	defer srv.Close()

	resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
		Request:     nil,
		Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1"}},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	})
	if err != nil {
		t.Fatalf("CheckStatus error: %v", err)
	}
	if called {
		t.Errorf("Wisp must not be queried without an identity")
	}
	got := resp.GetStatuses()[0]
	if got.GetStatus() != statusFailed {
		t.Errorf("status = %q, want failed", got.GetStatus())
	}
	if got.GetMessage() == "" {
		t.Errorf("want a message explaining the missing identity")
	}
}

// Identity varies per target once external_id is a source, so the query cache
// must be keyed on (connection, identity). Keying on connection alone would
// report one title's status for another — worse than the bug it dedupes.
func TestCheckStatusDoesNotCacheAcrossTitles(t *testing.T) {
	// Wisp reports each title as completed with a different pinned tier, so a
	// cache collision shows up as the wrong tier being marked completed.
	pinnedFor := map[string]string{"1": "1080p", "2": "2160p"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tmdb := r.URL.Query().Get("tmdb_id")
		_ = json.NewEncoder(w).Encode(wispStatus{
			State:           "completed",
			PinnedQualities: []string{pinnedFor[tmdb]},
		})
	}))
	defer srv.Close()

	resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
		Targets: []*pluginv1.TargetRef{
			{Quality: "1080p", ConnectionId: "c1", ExternalId: "tmdb:1"},
			{Quality: "2160p", ConnectionId: "c1", ExternalId: "tmdb:2"},
		},
		Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
	})
	if err != nil {
		t.Fatalf("CheckStatus error: %v", err)
	}
	for _, s := range resp.GetStatuses() {
		if s.GetStatus() != statusCompleted {
			t.Errorf("quality %s status = %q, want completed — a cache keyed only on "+
				"connection would serve the other title's pinned tier",
				s.GetQuality(), s.GetStatus())
		}
	}
}

// A permanently broken setup must be visibly broken. Mapping these to "queued"
// makes them indistinguishable from a healthy in-progress request, and the host
// then polls a doomed target forever — the live wedge this suite guards against.
func TestCheckStatusPermanentFailuresAreNotQueued(t *testing.T) {
	for _, code := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "tmdb:1"}},
				Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
			})
			if err != nil {
				t.Fatalf("CheckStatus error: %v", err)
			}
			got := resp.GetStatuses()[0]
			if got.GetStatus() != statusFailed {
				t.Errorf("HTTP %d status = %q, want failed", code, got.GetStatus())
			}
			if got.GetMessage() == "" {
				t.Errorf("HTTP %d: want a message explaining the failure", code)
			}
		})
	}
}

// 5xx and 429 are the conditions a re-poll is meant to ride out — they must stay
// queued, or a Wisp restart would fail every in-flight request.
func TestCheckStatusTransientFailuresStayQueued(t *testing.T) {
	for _, code := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "tmdb:1"}},
				Connections: []*pluginv1.RouterConnection{connFor("c1", srv.URL, "")},
			})
			if err != nil {
				t.Fatalf("CheckStatus error: %v", err)
			}
			if got := resp.GetStatuses()[0].GetStatus(); got != statusQueued {
				t.Errorf("HTTP %d status = %q, want queued", code, got)
			}
		})
	}
}

// "no such connection" and "connection present but unconfigured" are different
// operator problems and must not collapse to the same message.
func TestCheckStatusMisconfigurationIsDistinguishable(t *testing.T) {
	tests := []struct {
		name  string
		conns []*pluginv1.RouterConnection
		want  string // substring the message must contain
	}{
		{
			name:  "connection absent from request",
			conns: nil,
			want:  `no connection "c1"`,
		},
		{
			name:  "connection present but no wisp_url",
			conns: []*pluginv1.RouterConnection{{Id: "c1"}},
			want:  "wisp_url is not configured",
		},
		{
			name:  "connection present but wisp_url invalid",
			conns: []*pluginv1.RouterConnection{connFor("c1", "not-a-url", "")},
			want:  "wisp_url is invalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := testRouter().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
				Request:     &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}},
				Targets:     []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "tmdb:1"}},
				Connections: tc.conns,
			})
			if err != nil {
				t.Fatalf("CheckStatus error: %v", err)
			}
			got := resp.GetStatuses()[0]
			if got.GetStatus() != statusFailed {
				t.Errorf("status = %q, want failed (a misconfiguration never clears on its own)", got.GetStatus())
			}
			if !strings.Contains(got.GetMessage(), tc.want) {
				t.Errorf("message = %q, want it to contain %q", got.GetMessage(), tc.want)
			}
		})
	}
}

// The plugin must be audible ("was CheckStatus even called?") without ever
// putting a credential in the host's log.
func TestLoggingIsAudibleAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/add" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(wispStatus{State: "queued", Detail: "resolving stream"})
	}))
	defer srv.Close()

	var logs bytes.Buffer
	r := newRouter(hclog.New(&hclog.LoggerOptions{
		Level: hclog.Debug, Output: &logs, JSONFormat: true,
	}))

	const token = "super-secret-token"
	conns := []*pluginv1.RouterConnection{connFor("c1", srv.URL, token)}
	desc := &pluginv1.RequestDescriptor{MediaType: "movie", ExternalIds: map[string]string{"tmdb": "1"}}

	if _, err := r.Fulfill(context.Background(), &pluginv1.FulfillRequest{
		CapabilityId: "wisp-requests",
		Request:      desc,
		Qualities:    []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections:  conns,
	}); err != nil {
		t.Fatalf("Fulfill error: %v", err)
	}
	if _, err := r.CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
		CapabilityId: "wisp-requests",
		Request:      desc,
		Targets:      []*pluginv1.TargetRef{{Quality: "1080p", ConnectionId: "c1", ExternalId: "tmdb:1"}},
		Connections:  conns,
	}); err != nil {
		t.Fatalf("CheckStatus error: %v", err)
	}

	out := logs.String()
	// Audible: both entry points announce themselves with the capability id.
	for _, want := range []string{"fulfill", "check status", "wisp-requests"} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q — the plugin must be able to answer \"was I called\"\n%s", want, out)
		}
	}
	// Redacted: never the token.
	if strings.Contains(out, token) {
		t.Errorf("wisp_token leaked into the log:\n%s", out)
	}
}

// logHost reduces a wisp_url to host:port so a path (or an unparseable string)
// never reaches the log verbatim.
func TestLogHostRedacts(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"http://wisp:8080", "wisp:8080"},
		{"https://wisp.example.com/base/path", "wisp.example.com"},
		{"http://user:hunter2@wisp:8080", "invalid"},
		{"not-a-url", "invalid"},
	}
	for _, tc := range tests {
		got := logHost(tc.raw)
		if got != tc.want {
			t.Errorf("logHost(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if strings.Contains(got, "hunter2") {
			t.Errorf("logHost(%q) leaked a password", tc.raw)
		}
	}
}

func TestTestConnection(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/healthz" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer okSrv.Close()

	t.Run("ok", func(t *testing.T) {
		resp, err := testRouter().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{
			Connection: connFor("c1", okSrv.URL, ""),
		})
		if err != nil {
			t.Fatalf("TestConnection error: %v", err)
		}
		if !resp.GetOk() {
			t.Errorf("ok = false, want true (message: %q)", resp.GetMessage())
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		resp, err := testRouter().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{
			Connection: connFor("c1", "http://127.0.0.1:1", ""),
		})
		if err != nil {
			t.Fatalf("TestConnection error: %v", err)
		}
		if resp.GetOk() {
			t.Errorf("ok = true, want false for unreachable host")
		}
	})

	t.Run("missing url", func(t *testing.T) {
		resp, _ := testRouter().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{
			Connection: &pluginv1.RouterConnection{Id: "c1"},
		})
		if resp.GetOk() {
			t.Errorf("ok = true, want false when wisp_url missing")
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		resp, _ := testRouter().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{
			Connection: connFor("c1", "not-a-url", ""),
		})
		if resp.GetOk() {
			t.Errorf("ok = true, want false for invalid url")
		}
	})
}

func TestValidate(t *testing.T) {
	r := testRouter()

	t.Run("valid", func(t *testing.T) {
		resp, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{
			Connection: connFor("c1", "http://wisp:8080", ""),
		})
		if err != nil {
			t.Fatalf("Validate error: %v", err)
		}
		if len(resp.GetFieldErrors()) != 0 {
			t.Errorf("field errors = %v, want none", resp.GetFieldErrors())
		}
	})

	// The plugin fronts exactly one Wisp server. Caught at config time, an admin
	// sees the problem when adding the second connection; caught only in Fulfill,
	// every request silently fails afterwards.
	t.Run("sibling connection is a form error", func(t *testing.T) {
		resp, err := r.Validate(context.Background(), &pluginv1.ValidateRequest{
			Connection: connFor("c2", "http://wisp:8080", ""),
			Siblings:   []*pluginv1.RouterConnection{connFor("c1", "http://wisp:8080", "")},
		})
		if err != nil {
			t.Fatalf("Validate error: %v", err)
		}
		if resp.GetFormError() == "" {
			t.Errorf("expected a form error when a sibling Wisp connection exists")
		}
		if len(resp.GetFieldErrors()) != 0 {
			t.Errorf("field errors = %v, want none (the URL itself is valid)", resp.GetFieldErrors())
		}
	})

	t.Run("no siblings is not a form error", func(t *testing.T) {
		resp, _ := r.Validate(context.Background(), &pluginv1.ValidateRequest{
			Connection: connFor("c1", "http://wisp:8080", ""),
		})
		if resp.GetFormError() != "" {
			t.Errorf("form error = %q, want none for a lone connection", resp.GetFormError())
		}
	})

	t.Run("missing url", func(t *testing.T) {
		resp, _ := r.Validate(context.Background(), &pluginv1.ValidateRequest{
			Connection: &pluginv1.RouterConnection{Id: "c1"},
		})
		if resp.GetFieldErrors()["wisp_url"] == "" {
			t.Errorf("expected wisp_url error, got %v", resp.GetFieldErrors())
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		resp, _ := r.Validate(context.Background(), &pluginv1.ValidateRequest{
			Connection: connFor("c1", "ftp://wisp", ""),
		})
		if resp.GetFieldErrors()["wisp_url"] == "" {
			t.Errorf("expected wisp_url error for non-http scheme, got %v", resp.GetFieldErrors())
		}
	})

	// wisp_url is echoed back in Validate/TestConnection messages, so it must not
	// be usable as a credential store.
	t.Run("url with credentials", func(t *testing.T) {
		for _, bad := range []string{"http://user:hunter2@wisp:8080", "http://user@wisp:8080"} {
			resp, _ := r.Validate(context.Background(), &pluginv1.ValidateRequest{
				Connection: connFor("c1", bad, ""),
			})
			msg := resp.GetFieldErrors()["wisp_url"]
			if msg == "" {
				t.Errorf("expected wisp_url error for %q (embedded credentials)", bad)
			}
			if strings.Contains(msg, "hunter2") {
				t.Errorf("validation message echoed the password: %q", msg)
			}
		}
	})

	t.Run("url with query or fragment", func(t *testing.T) {
		for _, bad := range []string{"http://wisp:8080/?x=1", "http://wisp:8080/#frag"} {
			resp, _ := r.Validate(context.Background(), &pluginv1.ValidateRequest{
				Connection: connFor("c1", bad, ""),
			})
			if resp.GetFieldErrors()["wisp_url"] == "" {
				t.Errorf("expected wisp_url error for %q (query/fragment), got %v", bad, resp.GetFieldErrors())
			}
		}
	})
}

func TestListConfigOptionsEmpty(t *testing.T) {
	resp, err := testRouter().ListConfigOptions(context.Background(), &pluginv1.ListConfigOptionsRequest{})
	if err != nil {
		t.Fatalf("ListConfigOptions error: %v", err)
	}
	if len(resp.GetOptionsByField()) != 0 {
		t.Errorf("options = %v, want empty", resp.GetOptionsByField())
	}
}

func TestConnSettingsFallbackToBaseURL(t *testing.T) {
	// A host that maps the admin field onto the top-level base_url/api_key slots.
	conn := &pluginv1.RouterConnection{Id: "c1", BaseUrl: "http://wisp:8080", ApiKey: "tok"}
	url, token := connSettings(conn)
	if url != "http://wisp:8080" || token != "tok" {
		t.Errorf("connSettings fallback = %q/%q", url, token)
	}
}

func TestConnSettingsConfigWins(t *testing.T) {
	conn := connFor("c1", "http://from-config:8080", "cfgtok")
	conn.BaseUrl = "http://from-baseurl:9090"
	conn.ApiKey = "baseurltok"
	url, token := connSettings(conn)
	if url != "http://from-config:8080" || token != "cfgtok" {
		t.Errorf("config values should win: got %q/%q", url, token)
	}
}

// An admin who clears wisp_token in the form (present-but-empty) means "no
// token" — it must NOT resurrect a stale api_key fallback. Precedence is by
// field presence, not by emptiness.
func TestConnSettingsEmptyTokenOverridesApiKey(t *testing.T) {
	conn := &pluginv1.RouterConnection{
		Id:     "c1",
		ApiKey: "staletok",
		Config: &structpb.Struct{Fields: map[string]*structpb.Value{
			"wisp_url":   structpb.NewStringValue("http://wisp:8080"),
			"wisp_token": structpb.NewStringValue(""),
		}},
	}
	url, token := connSettings(conn)
	if url != "http://wisp:8080" {
		t.Errorf("url = %q, want http://wisp:8080", url)
	}
	if token != "" {
		t.Errorf("token = %q, want empty (present-but-empty config must win over api_key)", token)
	}
}
