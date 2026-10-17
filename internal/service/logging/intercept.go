package logging

// Intercepting sinks (#1521). LogSink.intercept_children is in Google's
// Logging API (the v2 discovery document gcloud and Terraform are generated
// from: "interceptChildren", gcloud's `logging sinks create
// --intercept-children`, Terraform's google_logging_folder_sink and
// google_logging_organization_sink `intercept_children`), but not in
// cloud.google.com/go/logging v1.20.0's loggingpb, so it is carried beside the
// generated message:
//   - over JSON, NewRESTHandler takes "interceptChildren" out of a sink's
//     request body before the transcoder (which refuses unknown fields) and
//     adds it to every sink the answer holds;
//   - over gRPC, a client built from the newer proto sends it as field 10,
//     which reaches the server as an unknown field and is answered the same
//     way;
//   - it is stored under its own key (interceptKey).
//
// As the field documents: "This field applies only to sinks owned by
// organizations and folders. When the value of 'intercept_children' is true,
// the following restrictions apply: The sink must have the include_children
// flag set to true. The sink destination must be a Cloud project. Also, the
// following behaviors apply: Any logs matched by the sink won't be included by
// non-_Required sinks owned by child resources." routeOf applies the last
// sentence: an entry an intercepting sink above it matches is kept out of its
// own _Default bucket and its own sinks, and out of the sinks of every folder
// between it and the intercepting sink's owner. The farthest intercepting sink
// that matches wins, as the sinks below it are its children's. _Required is
// not modelled (the gaps row), so nothing is exempt.
//
// The field's last sentence, that the sink is listed from a child resource
// when ListSinks' filter is in_scope("ALL") or in_scope("ANCESTOR"), is not
// served: ListSinksRequest has no filter in loggingpb either.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// interceptField is LogSink.intercept_children's field number in
// google/logging/v2/logging_config.proto.
const interceptField protowire.Number = 10

type interceptCtxKey struct{}

// requestIntercept is the intercept_children a request's sink carries: from
// the JSON body (NewRESTHandler) or the unknown field 10 a newer gRPC client
// sends. set is false when it carries none.
func requestIntercept(ctx context.Context, sk *loggingpb.LogSink) (v, set bool) {
	if b, ok := ctx.Value(interceptCtxKey{}).(bool); ok {
		return b, true
	}
	if sk == nil {
		return false, false
	}
	b := sk.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, t, l := protowire.ConsumeTag(b)
		if l < 0 {
			return v, set
		}
		b = b[l:]
		if n == interceptField && t == protowire.VarintType {
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return v, set
			}
			v, set = x != 0, true
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(n, t, b)
		if m < 0 {
			return v, set
		}
		b = b[m:]
	}
	return v, set
}

// markIntercept sets sk's field 10, for a gRPC answer, dropping any other
// unknown fields the request carried.
func markIntercept(sk *loggingpb.LogSink, v bool) {
	var u []byte
	if v {
		u = protowire.AppendTag(u, interceptField, protowire.VarintType)
		u = protowire.AppendVarint(u, 1)
	}
	sk.ProtoReflect().SetUnknown(u)
}

func interceptKey(p parent, id string) string {
	return "logging/sinkintercept/" + p.seg() + "/" + hex.EncodeToString([]byte(id))
}

// intercepts is whether p's sink id has intercept_children.
func (a *Admin) intercepts(p parent, id string) bool {
	b, err := a.db.Get(interceptKey(p, id))
	return err == nil && string(b) == "true"
}

func (a *Admin) putIntercept(p parent, id string, v bool) error {
	var err error
	if v {
		err = a.db.Put(interceptKey(p, id), []byte("true"))
	} else if err = a.db.Delete(interceptKey(p, id)); errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return apierror.Internal(err, "writing the sink's intercept_children")
	}
	return nil
}

// checkIntercept applies intercept_children's documented restrictions.
func checkIntercept(p parent, sk *loggingpb.LogSink, v bool) error {
	if !v {
		return nil
	}
	if !p.hasChildren() {
		return apierror.InvalidArgument("intercept_children applies only to sinks owned by organizations and folders, not of %s", p)
	}
	if !sk.GetIncludeChildren() {
		return apierror.InvalidArgument("intercept_children requires include_children to be true")
	}
	parts := strings.Split(sk.GetDestination(), "/")
	if len(parts) != 3 || parts[0] != "logging.googleapis.com" || parts[1] != "projects" {
		return apierror.InvalidArgument("intercept_children requires the sink destination to be a Cloud project, "+
			"logging.googleapis.com/projects/[PROJECT], not %q", sk.GetDestination())
	}
	return nil
}

// withoutInterceptPath splits intercept_children off an update mask.
func withoutInterceptPath(paths []string) (rest []string, named bool) {
	for _, p := range paths {
		if p == "intercept_children" || p == "interceptChildren" {
			named = true
			continue
		}
		rest = append(rest, p)
	}
	return rest, named
}

// sinkPath is a sink collection or sink in the JSON API:
// /v2/{parent}/sinks or /v2/{parent}/sinks/{sink}.
var sinkPath = regexp.MustCompile(`^/v2/((?:projects|folders|organizations|billingAccounts)/[^/]+)/sinks(?:/([^/]+))?$`)

// serveSinkJSON carries interceptChildren around the transcoder for a sink
// request, and reports false when r is not one.
func serveSinkJSON(a *Admin, next http.Handler, w http.ResponseWriter, r *http.Request) bool {
	m := sinkPath.FindStringSubmatch(r.URL.Path)
	if m == nil || a == nil {
		return false
	}
	p, _, err := cutParent("parent", m[1])
	if err != nil {
		return false // the transcoder reports it
	}
	if q := r.URL.Query(); q.Has("updateMask") {
		// gcloud logging sinks update sends snake_case paths
		// (destination,intercept_children), which Google accepts; the
		// transcoder takes FieldMask's JSON form, lowerCamelCase.
		paths := strings.Split(q.Get("updateMask"), ",")
		for i, p := range paths {
			paths[i] = lowerCamel(strings.TrimSpace(p))
		}
		q.Set("updateMask", strings.Join(paths, ","))
		u := *r.URL
		u.RawQuery = q.Encode()
		r = r.Clone(r.Context())
		r.URL = &u
	}
	if (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch) && r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("the request body could not be read"))
			return true
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) == nil {
			if ic, ok := body["interceptChildren"]; ok {
				var v bool
				if json.Unmarshal(ic, &v) != nil {
					apierror.WriteJSON(w, apierror.InvalidArgument("interceptChildren: %s is not a boolean", ic))
					return true
				}
				delete(body, "interceptChildren")
				raw, _ = json.Marshal(body)
				r = r.WithContext(context.WithValue(r.Context(), interceptCtxKey{}, v))
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	}
	bw := &bufferedWriter{ResponseWriter: w, code: http.StatusOK}
	next.ServeHTTP(bw, r)
	out := bw.buf.Bytes()
	if bw.code == http.StatusOK && r.Method != http.MethodDelete {
		out = addIntercept(a, p, out)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(bw.code)
	_, _ = w.Write(out)
	return true
}

// lowerCamel is a field path's JSON form: bigquery_options.use_partitioned_tables
// is bigqueryOptions.usePartitionedTables.
func lowerCamel(p string) string {
	var b strings.Builder
	up := false
	for _, c := range p {
		switch {
		case c == '_':
			up = true
		case up:
			b.WriteString(strings.ToUpper(string(c)))
			up = false
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// bufferedWriter holds an answer to rewrite it.
type bufferedWriter struct {
	http.ResponseWriter
	code int
	buf  bytes.Buffer
}

func (w *bufferedWriter) WriteHeader(code int)        { w.code = code }
func (w *bufferedWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }

// addIntercept adds interceptChildren to the sink, or each listed sink, of p
// that has it.
func addIntercept(a *Admin, p parent, raw []byte) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return raw
	}
	mark := func(sk map[string]json.RawMessage) {
		var id string
		if json.Unmarshal(sk["name"], &id) == nil && id != "" && a.intercepts(p, id) {
			sk["interceptChildren"] = json.RawMessage("true")
		}
	}
	if list, ok := body["sinks"]; ok {
		var sinks []map[string]json.RawMessage
		if json.Unmarshal(list, &sinks) != nil {
			return raw
		}
		for _, sk := range sinks {
			mark(sk)
		}
		body["sinks"], _ = json.Marshal(sinks)
	} else {
		mark(body)
	}
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}
