package main

import (
	"context"
	"fmt"
	"net/http"
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

// checkStatusBudget is a defensive ceiling on the total work CheckStatus may do,
// well under Silo's 60s host deadline. In practice CheckStatus makes one HTTP
// call (per connection, and there is one connection), each already bounded by
// httpTimeout; this guards against pathological multi-connection input.
const checkStatusBudget = 30 * time.Second

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

	// This plugin fronts exactly one Wisp server per installation. Fanning a
	// single request out to multiple Wisp backends is not a supported topology,
	// so reject it explicitly rather than silently routing to connections[0].
	if len(req.GetConnections()) > 1 {
		return &pluginv1.FulfillResponse{Message: "multiple Wisp connections configured; this plugin supports exactly one — remove the extras"}, nil
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

	// The host always requests at least one quality (its allowed-quality set
	// always includes 1080p), so an empty list is a defensive impossible-case.
	// Handle it without an add: a bare monitor with no quality filter would be
	// meaningless, and we must never return zero targets without a message.
	if len(req.GetQualities()) == 0 {
		return &pluginv1.FulfillResponse{Message: "request specified no qualities; nothing to fulfill"}, nil
	}

	extID := externalKey(tmdbID, imdbID)
	body := wispAddRequest{
		MediaType:  desc.GetMediaType(),
		TMDbID:     tmdbID,
		IMDbID:     imdbID,
		TVDbID:     tvdbID,
		Title:      desc.GetTitle(),
		Year:       int(desc.GetYear()),
		IsAnime:    desc.GetIsAnime(),
		Qualities:  toWispQualities(req.GetQualities()),
		RequestRef: extID,
	}

	client, err := newWispClient(wispURL, token, r.http)
	if err != nil {
		return &pluginv1.FulfillResponse{Message: fmt.Sprintf("wisp_url is invalid: %v", err)}, nil
	}
	if err := client.add(ctx, body); err != nil {
		return &pluginv1.FulfillResponse{Message: fmt.Sprintf("Wisp did not accept the request: %v", err)}, nil
	}

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
	// Bound the whole call so a slow or unresponsive Wisp cannot approach Silo's
	// deadline. In practice there is one connection and one HTTP call (each with
	// its own 10s client timeout); this is a defensive total-work ceiling.
	ctx, cancel := context.WithTimeout(ctx, checkStatusBudget)
	defer cancel()

	desc := req.GetRequest()

	connByID := make(map[string]*pluginv1.RouterConnection, len(req.GetConnections()))
	for _, c := range req.GetConnections() {
		connByID[c.GetId()] = c
	}

	// Identity can now vary per target (it may come from the target's own
	// external_id), so the query cache is keyed on the connection AND the title.
	// Keying on the connection alone would serve one title's status for another.
	type cacheKey struct {
		conn  string
		ident titleIdentity
	}
	cache := make(map[cacheKey]queryOutcome)

	statuses := make([]*pluginv1.TargetStatus, 0, len(req.GetTargets()))
	for _, t := range req.GetTargets() {
		cid := t.GetConnectionId()
		ident := identityForTarget(desc, t)
		if !ident.ok() {
			// Never issue an identity-free poll: Wisp answers 400, and a bare
			// query could not identify a title even if it did not.
			statuses = append(statuses, mapTargetStatus(t, queryOutcome{
				permanent: true,
				err:       fmt.Errorf("no tmdb or imdb id for this target; Wisp cannot be queried"),
			}))
			continue
		}

		key := cacheKey{conn: cid, ident: ident}
		res, ok := cache[key]
		if !ok {
			res = r.queryStatus(ctx, connByID[cid], cid, ident)
			cache[key] = res
		}
		statuses = append(statuses, mapTargetStatus(t, res))
	}
	return &pluginv1.CheckStatusResponse{Statuses: statuses}, nil
}

// queryOutcome is the classified result of one Wisp status query, shared by every
// target that resolves to the same connection.
type queryOutcome struct {
	st      *wispStatus
	tracked bool
	err     error

	// permanent marks err as a condition that will not clear on its own: a
	// misconfiguration, or a rejection Wisp will keep making. Transient errors
	// leave the target queued so the host re-polls; permanent ones fail it, so a
	// broken setup is visibly broken instead of being indistinguishable from a
	// healthy in-progress request and wedging forever.
	permanent bool
}

// queryStatus resolves one connection to a Wisp status. conn is nil when a target
// names a connection the host did not send — a distinct condition from a
// connection that is present but unconfigured, and reported as such.
func (r *router) queryStatus(ctx context.Context, conn *pluginv1.RouterConnection, cid string, ident titleIdentity) queryOutcome {
	if conn == nil {
		return queryOutcome{permanent: true, err: fmt.Errorf("no connection %q is configured for this plugin", cid)}
	}
	wispURL, token := connSettings(conn)
	if wispURL == "" {
		return queryOutcome{permanent: true, err: fmt.Errorf("wisp_url is not configured for this connection")}
	}
	client, err := newWispClient(wispURL, token, r.http)
	if err != nil {
		return queryOutcome{permanent: true, err: fmt.Errorf("wisp_url is invalid: %w", err)}
	}
	st, tracked, err := client.status(ctx, ident.mediaType, ident.tmdbID, ident.imdbID)
	if err != nil {
		return queryOutcome{permanent: permanentHTTP(err), err: err}
	}
	return queryOutcome{st: st, tracked: tracked}
}

// titleIdentity is the set of ids Wisp's status endpoint accepts. mediaType is an
// optional filter hint; Wisp requires at least one of tmdbID/imdbID and answers
// HTTP 400 without one.
type titleIdentity struct {
	mediaType string
	tmdbID    string
	imdbID    string
}

func (id titleIdentity) ok() bool { return id.tmdbID != "" || id.imdbID != "" }

// identityForTarget resolves the ids used to poll Wisp for one target.
//
// The descriptor is authoritative when it carries ids, but CheckStatusRequest.
// request is an ordinary optional field — the host is under no obligation to
// populate it, and a nil or id-less descriptor used to produce a poll with an
// empty query string. Fulfill stamps every target's external_id with
// "tmdb:<id>"/"imdb:<id>" precisely so identity can be carried through, so fall
// back to it. media_type is only a filter hint and is kept from the descriptor
// whichever source supplies the ids.
func identityForTarget(desc *pluginv1.RequestDescriptor, t *pluginv1.TargetRef) titleIdentity {
	var id titleIdentity
	if desc != nil {
		ids := desc.GetExternalIds()
		id = titleIdentity{
			mediaType: desc.GetMediaType(),
			tmdbID:    strings.TrimSpace(ids["tmdb"]),
			imdbID:    strings.TrimSpace(ids["imdb"]),
		}
	}
	if id.ok() {
		return id
	}

	scheme, value, found := strings.Cut(t.GetExternalId(), ":")
	if !found {
		return id
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return id
	}
	switch {
	case strings.EqualFold(strings.TrimSpace(scheme), "tmdb"):
		id.tmdbID = value
	case strings.EqualFold(strings.TrimSpace(scheme), "imdb"):
		id.imdbID = value
	}
	return id
}

// mapTargetStatus applies the state mapping for a single target:
//   - permanent error   -> failed (misconfiguration or a 4xx Wisp will repeat)
//   - transient error   -> queued (transport/5xx/429; host re-polls)
//   - 404 untracked     -> queued (Wisp will begin tracking)
//   - Wisp "failed"     -> failed (Detail as message)
//   - Wisp "completed"  -> completed if this quality is pinned, else queued
//   - Wisp "queued"     -> queued
//
// It is pure: the error paths are logged once per query by CheckStatus rather
// than here, which would repeat the same line for every target on a connection.
func mapTargetStatus(t *pluginv1.TargetRef, res queryOutcome) *pluginv1.TargetStatus {
	out := &pluginv1.TargetStatus{
		Quality:      t.GetQuality(),
		ConnectionId: t.GetConnectionId(),
	}
	switch {
	case res.err != nil && res.permanent:
		out.Status = statusFailed
		out.ExternalStatus = "error"
		out.Message = fmt.Sprintf("Wisp cannot be queried: %v", res.err)
	case res.err != nil:
		out.Status = statusQueued
		out.ExternalStatus = "unavailable"
		out.Message = fmt.Sprintf("Wisp status unavailable: %v", res.err)
	case !res.tracked:
		out.Status = statusQueued
		out.ExternalStatus = "untracked"
		out.Message = "Wisp is not yet tracking this title"
	case res.st == nil:
		// Defensive: no error, tracked, but no body. Unreachable via wispClient;
		// treated as transient so a future caller cannot wedge a target on it.
		out.Status = statusQueued
		out.ExternalStatus = "unavailable"
		out.Message = "Wisp returned no status for this title"
	default:
		st := res.st
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
	client, err := newWispClient(wispURL, token, r.http)
	if err != nil {
		return &pluginv1.TestConnectionResponse{Ok: false, Message: fmt.Sprintf("wisp_url must be a valid http(s) URL: %v", err)}, nil
	}
	if err := client.health(ctx); err != nil {
		return &pluginv1.TestConnectionResponse{Ok: false, Message: fmt.Sprintf("Wisp is unreachable: %v", err)}, nil
	}
	return &pluginv1.TestConnectionResponse{Ok: true, Message: "Connected to Wisp"}, nil
}

// Validate sanity-checks the connection config without any network access
// (TestConnection covers reachability). Only wisp_url is validated; wisp_token
// is optional. A present-but-empty wisp_url is a required-field error, not a
// silent fallback (see connSettings).
func (r *router) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	wispURL, _ := connSettings(req.GetConnection())
	fieldErrors := make(map[string]string)
	switch {
	case wispURL == "":
		fieldErrors["wisp_url"] = "Wisp URL is required"
	default:
		if _, err := parseWispBase(wispURL); err != nil {
			fieldErrors["wisp_url"] = fmt.Sprintf("Wisp URL must be a valid http(s) URL: %v", err)
		}
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
// conn.GetConfig().GetFields()[key]).
//
// Precedence is presence-based, not emptiness-based: if the config carries a key
// (even with an empty value) the admin set it explicitly, so it wins over the
// standardized base_url/api_key slot. The fallback to base_url/api_key applies
// only when the key is absent — the case where a host maps a "base_url"/"api_key"
// admin field onto the top-level RouterConnection fields. A present-but-empty
// wisp_url therefore yields "" (a validation error downstream), never a silent
// fallback to base_url.
func connSettings(conn *pluginv1.RouterConnection) (wispURL, token string) {
	if conn == nil {
		return "", ""
	}
	fields := conn.GetConfig().GetFields()

	if v, ok := fields["wisp_url"]; ok {
		wispURL = strings.TrimSpace(v.GetStringValue())
	} else {
		wispURL = strings.TrimSpace(conn.GetBaseUrl())
	}

	if v, ok := fields["wisp_token"]; ok {
		token = strings.TrimSpace(v.GetStringValue())
	} else {
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

func containsFold(list []string, target string) bool {
	for _, v := range list {
		if strings.EqualFold(v, target) {
			return true
		}
	}
	return false
}
