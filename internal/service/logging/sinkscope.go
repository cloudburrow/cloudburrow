package logging

// ListSinks' filter (#1577). The REST reference of projects.sinks.list (and
// of folders, organizations and billingAccounts) documents a `filter` query
// parameter that loggingpb's ListSinksRequest lacks, as googleapis'
// logging_config.proto does (read 2026-11-18): "A filter expression to
// constrain the sinks returned. Today, this only supports the following
// strings: '' 'in_scope("ALL")', 'in_scope("ANCESTOR")',
// 'in_scope("DEFAULT")'", where ALL "Includes all of the sinks which can be
// returned in any other scope", ANCESTOR "Includes intercepting sinks owned
// by ancestor resources" and DEFAULT "Includes sinks owned by `parent`"; the
// empty string applies DEFAULT
// (https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.sinks/list).
// LogSink.intercept_children says the same: such a sink is listed from a
// child resource under in_scope("ALL") or in_scope("ANCESTOR"). It is served
// on the JSON API, where google.golang.org/api/logging/v2's SinksListCall
// sends it; any other filter string is INVALID_ARGUMENT (UNVERIFIED, docs
// re-read 2026-11-18: the
// reference names no error).

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

type sinkScope int

const (
	scopeDefault sinkScope = iota
	scopeAncestor
	scopeAll
)

func (s sinkScope) String() string {
	return [...]string{"DEFAULT", "ANCESTOR", "ALL"}[s]
}

// parseSinkScope reads ListSinks' filter: empty, in_scope("DEFAULT"),
// in_scope("ANCESTOR") or in_scope("ALL").
func parseSinkScope(filter string) (sinkScope, error) {
	f := strings.TrimSpace(filter)
	if f == "" {
		return scopeDefault, nil
	}
	if arg, ok := strings.CutPrefix(f, "in_scope("); ok {
		if arg, ok = strings.CutSuffix(arg, ")"); ok {
			switch strings.TrimSpace(arg) {
			case `"DEFAULT"`:
				return scopeDefault, nil
			case `"ANCESTOR"`:
				return scopeAncestor, nil
			case `"ALL"`:
				return scopeAll, nil
			}
		}
	}
	return scopeDefault, apierror.InvalidArgument(`filter %q is not supported: ListSinks takes only '', 'in_scope("ALL")', 'in_scope("ANCESTOR")' or 'in_scope("DEFAULT")'`, filter)
}

// scopedSink is a sink listed under in_scope("ANCESTOR") or in_scope("ALL"),
// with whether it intercepts (its owner's, not the parent's).
type scopedSink struct {
	sink      *loggingpb.LogSink
	intercept bool
}

// listScopedSinks lists, for in_scope("ANCESTOR") or in_scope("ALL"), the
// intercepting sinks of p's ancestors, after p's own sinks for ALL. Each is
// keyed by its owner and ID, so the page token is stable across owners.
func (a *Admin) listScopedSinks(p parent, scope sinkScope, token string, size int32) ([]scopedSink, string, error) {
	type owned struct {
		owner parent
		id    string
	}
	byKey := map[string]owned{}
	var keys []string
	add := func(owner parent, rank int, onlyIntercepting bool) error {
		ks, err := a.db.List(kindSink.prefix(owner))
		if err != nil {
			return apierror.Internal(err, "listing sinks")
		}
		for _, k := range ks {
			raw, err := hex.DecodeString(strings.TrimPrefix(k, kindSink.prefix(owner)))
			if err != nil {
				continue
			}
			id := string(raw)
			if onlyIntercepting && !a.intercepts(owner, id) {
				continue
			}
			// rank keeps the parent's own sinks first, then the nearest
			// ancestor's.
			key := string(rune('a'+rank)) + "|" + owner.String() + "|" + id
			byKey[key] = owned{owner, id}
			keys = append(keys, key)
		}
		return nil
	}
	if scope == scopeAll {
		if err := add(p, 0, false); err != nil {
			return nil, "", err
		}
	}
	for i, anc := range a.ancestors(p) {
		if err := add(anc, i+1, true); err != nil {
			return nil, "", err
		}
	}
	page, next, err := paging.Page("logging-sinks-"+scope.String()+"|"+p.String(), keys, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("page_token: %v", err)
	}
	var out []scopedSink
	for _, k := range page {
		o := byKey[k]
		m, err := a.get(kindSink, o.owner, o.id)
		if err != nil {
			continue // deleted between the listing and the read
		}
		out = append(out, scopedSink{m.(*loggingpb.LogSink), a.intercepts(o.owner, o.id)})
	}
	return out, next, nil
}

// serveScopedSinks answers a JSON ListSinks whose filter is
// in_scope("ANCESTOR") or in_scope("ALL"): the transcoder's request has no
// filter field, so it is answered here, each sink with its own
// interceptChildren.
func serveScopedSinks(a *Admin, w http.ResponseWriter, r *http.Request, parentName string, scope sinkScope) {
	q := r.URL.Query()
	p, err := a.parseParent(parentName)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	size := 0
	if v := q.Get("pageSize"); v != "" {
		if size, err = strconv.Atoi(v); err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("pageSize %q is not an integer", v))
			return
		}
	}
	sinks, next, err := a.listScopedSinks(p, scope, q.Get("pageToken"), int32(size))
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	body := map[string]any{}
	var list []json.RawMessage
	for _, s := range sinks {
		raw, err := protojson.Marshal(s.sink)
		if err != nil {
			apierror.WriteJSON(w, apierror.Internal(err, "encoding a sink"))
			return
		}
		if s.intercept {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) == nil {
				m["interceptChildren"] = json.RawMessage("true")
				raw, _ = json.Marshal(m)
			}
		}
		list = append(list, raw)
	}
	if len(list) > 0 {
		body["sinks"] = list
	}
	if next != "" {
		body["nextPageToken"] = next
	}
	out, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}
