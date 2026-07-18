package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Host-normalized status values (see request_router.proto).
const (
	statusQueued    = "queued"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// Wisp reports these request states (see wisp status.go).
const (
	wispStateQueued    = "queued"
	wispStateCompleted = "completed"
	wispStateFailed    = "failed"
)

// httpTimeout bounds every call to Wisp. Fulfill makes exactly one HTTP call, so
// this keeps the plugin far under Silo's 60s fulfillment deadline: Wisp's intake
// is async (202) and never resolves inline.
const httpTimeout = 10 * time.Second

// router implements the RequestRouter capability by delegating every request to
// a Wisp server. It holds no per-request state and no credentials; connections
// (and their wisp_url/wisp_token) are passed in on every call.
type router struct {
	pluginv1.UnimplementedRequestRouterServer
	http *http.Client
}

func newRouter() *router {
	return &router{http: &http.Client{Timeout: httpTimeout}}
}

// Fulfill hands the request to Wisp via POST /api/add and returns one target per
// requested quality. Wisp accepts asynchronously, so every target starts as
// "queued"; CheckStatus later promotes them. On a missing connection, missing
// wisp_url, missing identity, or any Wisp/transport failure it returns zero
// targets plus a top-level message — Silo treats zero targets as a submission
// failure and will retry.
func (r *router) Fulfill(ctx context.Context, req *pluginv1.FulfillRequest) (*pluginv1.FulfillResponse, error) {
	desc := req.GetRequest()
	if desc == nil {
		return &pluginv1.FulfillResponse{Message: "request descriptor is missing"}, nil
	}

	conn := firstConnection(req.GetConnections())
	if conn == nil {
		return &pluginv1.FulfillResponse{Message: "no Wisp connection configured"}, nil
	}
	wispURL, token := connSettings(conn)
	if wispURL == "" {
		return &pluginv1.FulfillResponse{Message: "wisp_url is not configured for this connection"}, nil
	}

	ids := desc.GetExternalIds()
	tmdbID, imdbID, tvdbID := ids["tmdb"], ids["imdb"], ids["tvdb"]
	if tmdbID == "" && imdbID == "" {
		return &pluginv1.FulfillResponse{Message: "request has no tmdb or imdb id; Wisp cannot track it"}, nil
	}

	body := wispAddRequest{
		MediaType:  desc.GetMediaType(),
		TMDbID:     tmdbID,
		IMDbID:     imdbID,
		TVDbID:     tvdbID,
		Title:      desc.GetTitle(),
		Year:       int(desc.GetYear()),
		IsAnime:    desc.GetIsAnime(),
		Qualities:  toWispQualities(req.GetQualities()),
		RequestRef: externalKey(tmdbID, imdbID),
	}

	client := newWispClient(wispURL, token, r.http)
	if err := client.add(ctx, body); err != nil {
		return &pluginv1.FulfillResponse{Message: fmt.Sprintf("Wisp did not accept the request: %v", err)}, nil
	}

	extID := externalKey(tmdbID, imdbID)
	const msg = "Wisp accepted the request and is monitoring for a pinnable stream"
	targets := make([]*pluginv1.FulfillmentTarget, 0, len(req.GetQualities()))
	for _, q := range req.GetQualities() {
		targets = append(targets, &pluginv1.FulfillmentTarget{
			Quality:        q.GetId(),
			ConnectionId:   conn.GetId(),
			ExternalId:     extID,
			ExternalStatus: wispStateQueued,
			Status:         statusQueued,
			Message:        msg,
		})
	}
	return &pluginv1.FulfillResponse{Targets: targets}, nil
}

// CheckStatus maps Wisp's per-title state onto each requested target. Targets are
// grouped by connection so Wisp is queried at most once per connection; all
// targets under the same connection share that title's state.
func (r *router) CheckStatus(ctx context.Context, req *pluginv1.CheckStatusRequest) (*pluginv1.CheckStatusResponse, error) {
	desc := req.GetRequest()
	var mediaType, tmdbID, imdbID string
	if desc != nil {
		mediaType = desc.GetMediaType()
		ids := desc.GetExternalIds()
		tmdbID, imdbID = ids["tmdb"], ids["imdb"]
	}

	connByID := make(map[string]*pluginv1.RouterConnection, len(req.GetConnections()))
	for _, c := range req.GetConnections() {
		connByID[c.GetId()] = c
	}

	type queryResult struct {
		st      *wispStatus
		tracked bool
		err     error
	}
	cache := make(map[string]queryResult)

	statuses := make([]*pluginv1.TargetStatus, 0, len(req.GetTargets()))
	for _, t := range req.GetTargets() {
		cid := t.GetConnectionId()
		res, ok := cache[cid]
		if !ok {
			wispURL, token := connSettings(connByID[cid])
			if wispURL == "" {
				res = queryResult{err: fmt.Errorf("wisp_url is not configured for this connection")}
			} else {
				client := newWispClient(wispURL, token, r.http)
				st, tracked, err := client.status(ctx, mediaType, tmdbID, imdbID)
				res = queryResult{st: st, tracked: tracked, err: err}
			}
			cache[cid] = res
		}
		statuses = append(statuses, mapTargetStatus(t, res.st, res.tracked, res.err))
	}
	return &pluginv1.CheckStatusResponse{Statuses: statuses}, nil
}

// mapTargetStatus applies the state mapping for a single target:
//   - transport/HTTP error (not 404) -> queued (transient; host re-polls)
//   - 404 untracked                  -> queued (Wisp will begin tracking)
//   - Wisp "failed"                  -> failed (Detail as message)
//   - Wisp "completed"               -> completed if this quality is pinned, else queued
//   - Wisp "queued"                  -> queued
func mapTargetStatus(t *pluginv1.TargetRef, st *wispStatus, tracked bool, err error) *pluginv1.TargetStatus {
	out := &pluginv1.TargetStatus{
		Quality:      t.GetQuality(),
		ConnectionId: t.GetConnectionId(),
	}
	switch {
	case err != nil:
		out.Status = statusQueued
		out.ExternalStatus = "unavailable"
		out.Message = fmt.Sprintf("Wisp status unavailable: %v", err)
	case !tracked:
		out.Status = statusQueued
		out.ExternalStatus = "untracked"
		out.Message = "Wisp is not yet tracking this title"
	default:
		out.ExternalStatus = st.State
		out.Message = st.Detail
		switch st.State {
		case wispStateFailed:
			out.Status = statusFailed
		case wispStateCompleted:
			if containsFold(st.PinnedQualities, t.GetQuality()) {
				out.Status = statusCompleted
			} else {
				out.Status = statusQueued
			}
		default: // wispStateQueued and any unknown state
			out.Status = statusQueued
		}
	}
	return out
}

// TestConnection verifies the connection's wisp_url is a valid http(s) URL and
// that Wisp's healthz endpoint responds.
func (r *router) TestConnection(ctx context.Context, req *pluginv1.TestConnectionRequest) (*pluginv1.TestConnectionResponse, error) {
	wispURL, token := connSettings(req.GetConnection())
	if wispURL == "" {
		return &pluginv1.TestConnectionResponse{Ok: false, Message: "wisp_url is not configured"}, nil
	}
	if !isHTTPURL(wispURL) {
		return &pluginv1.TestConnectionResponse{Ok: false, Message: "wisp_url must be a valid http(s) URL"}, nil
	}
	client := newWispClient(wispURL, token, r.http)
	if err := client.health(ctx); err != nil {
		return &pluginv1.TestConnectionResponse{Ok: false, Message: fmt.Sprintf("Wisp is unreachable: %v", err)}, nil
	}
	return &pluginv1.TestConnectionResponse{Ok: true, Message: "Connected to Wisp"}, nil
}

// Validate sanity-checks the connection config without any network access
// (TestConnection covers reachability). Only wisp_url is validated; wisp_token
// is optional.
func (r *router) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	wispURL, _ := connSettings(req.GetConnection())
	fieldErrors := make(map[string]string)
	switch {
	case wispURL == "":
		fieldErrors["wisp_url"] = "Wisp URL is required"
	case !isHTTPURL(wispURL):
		fieldErrors["wisp_url"] = "Wisp URL must be a valid http(s) URL"
	}
	return &pluginv1.ValidateResponse{FieldErrors: fieldErrors}, nil
}

// ListConfigOptions returns no dynamic options: the connection form has only
// free-text fields, so there are no dropdowns to populate.
func (r *router) ListConfigOptions(_ context.Context, _ *pluginv1.ListConfigOptionsRequest) (*pluginv1.ListConfigOptionsResponse, error) {
	return &pluginv1.ListConfigOptionsResponse{}, nil
}

// connSettings extracts wisp_url and wisp_token from a connection. Admin-form
// fields arrive in the config Struct keyed by their field key (verified against
// the reference plugin, which reads custom fields via
// conn.GetConfig().GetFields()[key]). The standardized base_url/api_key slots are
// used as a fallback so the plugin still works if a host maps a "base_url"/
// "api_key" admin field onto the top-level RouterConnection fields.
func connSettings(conn *pluginv1.RouterConnection) (wispURL, token string) {
	if conn == nil {
		return "", ""
	}
	if fields := conn.GetConfig().GetFields(); fields != nil {
		wispURL = strings.TrimSpace(fields["wisp_url"].GetStringValue())
		token = strings.TrimSpace(fields["wisp_token"].GetStringValue())
	}
	if wispURL == "" {
		wispURL = strings.TrimSpace(conn.GetBaseUrl())
	}
	if token == "" {
		token = strings.TrimSpace(conn.GetApiKey())
	}
	return wispURL, token
}

func firstConnection(conns []*pluginv1.RouterConnection) *pluginv1.RouterConnection {
	if len(conns) == 0 {
		return nil
	}
	return conns[0]
}

func toWispQualities(quals []*pluginv1.RequestedQuality) []wispQuality {
	if len(quals) == 0 {
		return nil
	}
	out := make([]wispQuality, 0, len(quals))
	for _, q := range quals {
		out = append(out, wispQuality{ID: q.GetId(), Is4K: q.GetIs4K()})
	}
	return out
}

// externalKey is a stable handle for a title, preferring tmdb over imdb. It is
// echoed back to Silo as FulfillmentTarget.external_id and reused as Wisp's
// request_ref so status responses can carry it through.
func externalKey(tmdbID, imdbID string) string {
	switch {
	case tmdbID != "":
		return "tmdb:" + tmdbID
	case imdbID != "":
		return "imdb:" + imdbID
	default:
		return ""
	}
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func containsFold(list []string, target string) bool {
	for _, v := range list {
		if strings.EqualFold(v, target) {
			return true
		}
	}
	return false
}
