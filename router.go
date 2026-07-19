package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/hashicorp/go-hclog"
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

// multiConnectionMsg is the single wording for the one-connection invariant,
// shared by Fulfill (which refuses to route) and Validate (which surfaces it at
// config time, before any request can silently fail).
const multiConnectionMsg = "multiple Wisp connections configured; this plugin supports exactly one — remove the extras"

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
	log  hclog.Logger
}

// newRouter builds the capability server. A nil logger is accepted and discarded
// so callers that do not care about output (tests) need not construct one.
func newRouter(log hclog.Logger) *router {
	if log == nil {
		log = hclog.NewNullLogger()
	}
	return &router{http: &http.Client{Timeout: httpTimeout}, log: log}
}

// fulfillFailed logs and returns a rejection. Silo reads zero targets plus a
// message as a submission failure; the log line is what makes the reason
// visible from outside the plugin.
func (r *router) fulfillFailed(reason string) (*pluginv1.FulfillResponse, error) {
	r.log.Warn("fulfill rejected", "reason", reason)
	return &pluginv1.FulfillResponse{Message: reason}, nil
}

// logHost reduces a wisp_url to its host:port for logging. It never echoes the
// raw URL: a path or (rejected, but be defensive) userinfo must not reach the
// log. An unparseable URL logs as "invalid" rather than leaking its text.
func logHost(rawURL string) string {
	u, err := parseWispBase(rawURL)
	if err != nil {
		return "invalid"
	}
	return u.Host
}

// Fulfill hands the request to Wisp via POST /api/add and returns one target per
// requested quality. Wisp accepts asynchronously, so every target starts as
// "queued"; CheckStatus later promotes them. On a missing connection, missing
// wisp_url, missing identity, or any Wisp/transport failure it returns zero
// targets plus a top-level message — Silo treats zero targets as a submission
// failure and will retry.
func (r *router) Fulfill(ctx context.Context, req *pluginv1.FulfillRequest) (*pluginv1.FulfillResponse, error) {
	desc := req.GetRequest()
	r.log.Info("fulfill",
		"capability_id", req.GetCapabilityId(),
		"media_type", desc.GetMediaType(),
		"qualities", len(req.GetQualities()),
		"connections", len(req.GetConnections()))

	if desc == nil {
		return r.fulfillFailed("request descriptor is missing")
	}

	// This plugin fronts exactly one Wisp server per installation. Fanning a
	// single request out to multiple Wisp backends is not a supported topology,
	// so reject it explicitly rather than silently routing to connections[0].
	if len(req.GetConnections()) > 1 {
		return r.fulfillFailed(multiConnectionMsg)
	}
	conn := firstConnection(req.GetConnections())
	if conn == nil {
		return r.fulfillFailed("no Wisp connection configured")
	}
	wispURL, token := connSettings(conn)
	if wispURL == "" {
		return r.fulfillFailed("wisp_url is not configured for this connection")
	}

	ids := desc.GetExternalIds()
	tmdbID, imdbID, tvdbID := ids["tmdb"], ids["imdb"], ids["tvdb"]
	if tmdbID == "" && imdbID == "" {
		return r.fulfillFailed("request has no tmdb or imdb id; Wisp cannot track it")
	}

	// The host always requests at least one quality (its allowed-quality set
	// always includes 1080p), so an empty list is a defensive impossible-case.
	// Handle it without an add: a bare monitor with no quality filter would be
	// meaningless, and we must never return zero targets without a message.
	if len(req.GetQualities()) == 0 {
		return r.fulfillFailed("request specified no qualities; nothing to fulfill")
	}

	// The host matches a status back to a request by (quality, connection_id), so
	// two targets sharing a quality are unresolvable. Dedupe once and use the
	// result for both the Wisp body and the returned targets.
	quals := dedupeQualities(req.GetQualities())

	extID := externalKey(tmdbID, imdbID)
	body := wispAddRequest{
		MediaType:  desc.GetMediaType(),
		TMDbID:     tmdbID,
		IMDbID:     imdbID,
		TVDbID:     tvdbID,
		Title:      desc.GetTitle(),
		Year:       int(desc.GetYear()),
		IsAnime:    desc.GetIsAnime(),
		Qualities:  toWispQualities(quals),
		RequestRef: extID,
	}

	client, err := newWispClient(wispURL, token, r.http)
	if err != nil {
		return r.fulfillFailed(fmt.Sprintf("wisp_url is invalid: %v", err))
	}
	if err := client.add(ctx, body); err != nil {
		return r.fulfillFailed(fmt.Sprintf("Wisp did not accept the request: %v", err))
	}

	r.log.Info("wisp accepted request", "wisp_host", logHost(wispURL), "targets", len(quals))

	const msg = "Wisp accepted the request and is monitoring for a pinnable stream"
	targets := make([]*pluginv1.FulfillmentTarget, 0, len(quals))
	for _, q := range quals {
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

	r.log.Info("check status",
		"capability_id", req.GetCapabilityId(),
		"targets", len(req.GetTargets()),
		"connections", strings.Join(distinctConnectionIDs(req.GetTargets()), ","),
		"has_descriptor", desc != nil)

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
			r.log.Warn("no identity for target", "quality", t.GetQuality(), "connection_id", cid)
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
			if res.err != nil {
				// Logged here, once per query, rather than in mapTargetStatus,
				// which would repeat the line for every target on the connection.
				r.log.Warn("wisp status query failed",
					"connection_id", cid, "permanent", res.permanent, "err", res.err)
			}
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

// distinctConnectionIDs lists the connections the targets refer to, for logging.
func distinctConnectionIDs(targets []*pluginv1.TargetRef) []string {
	seen := make(map[string]bool, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if id := t.GetConnectionId(); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
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

	// siblings exists so a plugin can enforce cross-connection rules at config
	// time. Without this the one-connection invariant is enforced only in
	// Fulfill, where breaking it silently fails every request instead of telling
	// the admin at the moment they add the second connection.
	resp := &pluginv1.ValidateResponse{FieldErrors: fieldErrors}
	if len(req.GetSiblings()) > 0 {
		resp.FormError = multiConnectionMsg
	}
	return resp, nil
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

// dedupeQualities drops repeated quality ids, compared case-insensitively,
// preserving first-seen order. A repeated tier gains Wisp nothing and would
// produce two FulfillmentTargets identical in quality, connection_id and
// external_id — which the host cannot tell apart when matching status back.
func dedupeQualities(quals []*pluginv1.RequestedQuality) []*pluginv1.RequestedQuality {
	out := make([]*pluginv1.RequestedQuality, 0, len(quals))
	seen := make(map[string]bool, len(quals))
	for _, q := range quals {
		key := strings.ToLower(strings.TrimSpace(q.GetId()))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, q)
	}
	return out
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
