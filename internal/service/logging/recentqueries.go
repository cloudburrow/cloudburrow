package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Recent queries (#2031): projects.locations.recentQueries.list, GET
// v2/{parent=*/*/locations/*}/recentQueries, "Lists the RecentQueries that
// were created by the user making the request"
// (https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.locations.recentQueries/list,
// read 2026-11-20). Like saved queries, no published google.logging.v2
// proto carries it, so the JSON API serves it by hand and gRPC does not.
//
// Google's API has no create method: a recent query is one its console ran.
// Here the console records each Logs Explorer and Log Analytics query it
// runs (RecordRecentQuery), as the user it serves. Google keeps "the last
// 10,000 unique queries over a 30-day period"
// (https://docs.cloud.google.com/logging/docs/view/building-queries): a
// query run again moves to the top with its new lastRunTime, one older
// than 30 days is no longer listed, and past 10,000 the oldest goes.
//
// The user making the request is the account a bearer token this instance
// minted names; a request with no token, or with one it did not mint, is
// the instance's own account, as everywhere else that a caller is named
// (SetCallerResolver). The list's filter is `type("Logging")` or
// `type("OpsAnalytics")`, the only values the reference documents; any
// other is INVALID_ARGUMENT. The list is most recent first (UNVERIFIED:
// the reference names no order).

// recentQuery is a RecentQuery resource.
type recentQuery struct {
	Name              string             `json:"name,omitempty"`
	LoggingQuery      *loggingQuery      `json:"loggingQuery,omitempty"`
	OpsAnalyticsQuery *opsAnalyticsQuery `json:"opsAnalyticsQuery,omitempty"`
	LastRunTime       string             `json:"lastRunTime,omitempty"`
}

const (
	maxRecentQueries = 10000
	recentQueryTTL   = 30 * 24 * time.Hour
)

// callers names a request's user: resolve maps a bearer token to the
// account it was minted for, and owner is the instance's own account.
var callers struct {
	mu      sync.Mutex
	resolve func(bearer string) (string, bool)
	owner   string
}

// SetCallerResolver tells Cloud Logging how to name the user making a
// request from its bearer token, and the instance's own account, which a
// request with no token (and the console) is served as.
func SetCallerResolver(resolve func(bearer string) (string, bool), owner string) {
	callers.mu.Lock()
	defer callers.mu.Unlock()
	callers.resolve, callers.owner = resolve, strings.ToLower(owner)
}

// Owner is the account the console and token-less requests are served as.
func Owner() string {
	callers.mu.Lock()
	defer callers.mu.Unlock()
	if callers.owner == "" {
		return "owner"
	}
	return callers.owner
}

// callerOf is the user making r.
func callerOf(r *http.Request) string {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	callers.mu.Lock()
	resolve := callers.resolve
	callers.mu.Unlock()
	if ok && resolve != nil {
		if email, ok := resolve(strings.TrimSpace(bearer)); ok && email != "" {
			return strings.ToLower(email)
		}
	}
	return Owner()
}

func recentPrefix(p parent, user string) string {
	return "logging/recentqueries/" + p.seg() + "/" + hex.EncodeToString([]byte(user)) + "/"
}

func recentKey(p parent, user, loc, id string) string {
	return recentPrefix(p, user) + hex.EncodeToString([]byte(loc)) + "/" + id
}

// recentQueryID is a query's ID: the same query text of the same type is
// the same recent query, which a new run moves to the top.
func recentQueryID(sql bool, text string) string {
	kind := "logging"
	if sql {
		kind = "opsanalytics"
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + text))
	return hex.EncodeToString(sum[:12])
}

// RecordRecentQuery records that user ran query (a Logging query
// language filter, or Log Analytics SQL when sql) under parentName
// (projects/P, folders/F, organizations/O or billingAccounts/B) in
// location. An empty query is not recorded.
func (a *Admin) RecordRecentQuery(parentName, location, user, query string, sql bool) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	p, tail, err := cutParent("parent", parentName)
	if err != nil {
		return err
	}
	if tail != "" {
		return apierror.InvalidArgument("parent %q is not a project, folder, organization or billing account", parentName)
	}
	if !locationRe.MatchString(location) {
		return apierror.InvalidArgument("location %q is not a location", location)
	}
	if user == "" {
		user = Owner()
	}
	user = strings.ToLower(user)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.check(p); err != nil {
		return err
	}
	id := recentQueryID(sql, query)
	q := recentQuery{Name: p.String() + "/locations/" + location + "/recentQueries/" + id,
		LastRunTime: a.now().UTC().Format(time.RFC3339Nano)}
	if sql {
		q.OpsAnalyticsQuery = &opsAnalyticsQuery{SQLQueryText: query}
	} else {
		q.LoggingQuery = &loggingQuery{Filter: query}
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return apierror.Internal(err, "encoding the recent query")
	}
	if err := a.db.Put(recentKey(p, user, location, id), raw); err != nil {
		return apierror.Internal(err, "writing the recent query")
	}
	return a.pruneRecent(p, user)
}

// recentEntry is a stored recent query and its key.
type recentEntry struct {
	key string
	q   recentQuery
	raw []byte
	at  time.Time
}

// recentQueries are user's recent queries under p (in loc, or in every
// location for "-"), the expired ones removed. The caller holds a.mu.
func (a *Admin) recentQueries(p parent, user, loc string) ([]recentEntry, error) {
	prefix := recentPrefix(p, user)
	if loc != "-" {
		prefix += hex.EncodeToString([]byte(loc)) + "/"
	}
	keys, err := a.db.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "listing recent queries")
	}
	cutoff := a.now().Add(-recentQueryTTL)
	var out []recentEntry
	for _, k := range keys {
		raw, err := a.db.Get(k)
		if err != nil {
			continue
		}
		var q recentQuery
		if json.Unmarshal(raw, &q) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, q.LastRunTime)
		if err != nil || at.Before(cutoff) {
			_ = a.db.Delete(k)
			continue
		}
		out = append(out, recentEntry{key: k, q: q, raw: raw, at: at})
	}
	// Most recent first; the name breaks a tie, so the order is stable.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.After(out[j].at)
		}
		return out[i].q.Name < out[j].q.Name
	})
	return out, nil
}

// pruneRecent keeps user's 10,000 most recent queries under p. The caller
// holds a.mu.
func (a *Admin) pruneRecent(p parent, user string) error {
	all, err := a.recentQueries(p, user, "-")
	if err != nil {
		return err
	}
	for _, e := range all[min(len(all), maxRecentQueries):] {
		if err := a.db.Delete(e.key); err != nil {
			return apierror.Internal(err, "removing an old recent query")
		}
	}
	return nil
}

// serveRecent serves {parent}/locations/{location}/recentQueries.
func (a *Admin) serveRecent(w http.ResponseWriter, r *http.Request, parentName, id string) error {
	if id != "" || r.Method != http.MethodGet {
		// list is the resource's only method.
		return apierror.NotFound("no recentQueries method answers %s %s", r.Method, r.URL.Path)
	}
	p, tail, err := cutParent("parent", parentName)
	if err != nil {
		return err
	}
	loc, ok := strings.CutPrefix(tail, "locations/")
	if !ok || loc == "" || strings.Contains(loc, "/") || (loc != "-" && !locationRe.MatchString(loc)) {
		return apierror.InvalidArgument("parent %q is not {parent}/locations/{location}", parentName)
	}
	q := r.URL.Query()
	var keep func(recentQuery) bool
	switch f := strings.TrimSpace(q.Get("filter")); f {
	case "":
	case `type("Logging")`:
		keep = func(x recentQuery) bool { return x.LoggingQuery != nil }
	case `type("OpsAnalytics")`:
		keep = func(x recentQuery) bool { return x.OpsAnalyticsQuery != nil }
	default:
		return apierror.InvalidArgument(`filter %q is not type("Logging") or type("OpsAnalytics")`, q.Get("filter"))
	}
	size := 0
	if s := q.Get("pageSize"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return apierror.InvalidArgument("pageSize %q is not a number", s)
		}
		// "Non-positive values are ignored."
		size = max(n, 0)
	}
	user := callerOf(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.check(p); err != nil {
		return err
	}
	all, err := a.recentQueries(p, user, loc)
	if err != nil {
		return err
	}
	// paging sorts its keys: a key that sorts most recent first.
	values := map[string][]byte{}
	keys := make([]string, 0, len(all))
	for _, e := range all {
		if keep != nil && !keep(e.q) {
			continue
		}
		k := fmt.Sprintf("%020d|%s", math.MaxInt64-e.at.UnixNano(), e.q.Name)
		keys = append(keys, k)
		values[k] = e.raw
	}
	page, next, err := paging.Page("logging-recentQueries|"+user+"|"+p.String()+"|"+loc+"|"+q.Get("filter"), keys, q.Get("pageToken"), size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	items := make([]json.RawMessage, 0, len(page))
	for _, k := range page {
		items = append(items, values[k])
	}
	out := map[string]any{"recentQueries": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	return rest.WriteJSON(w, http.StatusOK, out)
}
