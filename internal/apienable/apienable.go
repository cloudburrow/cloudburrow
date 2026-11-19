// Package apienable refuses calls to an API that is disabled in the
// project they are made in, as Google does (#1914): PERMISSION_DENIED
// (HTTP 403), "<Title> has not been used in project <number> before or it
// is disabled. Enable it by visiting <activationUrl> then retry. ...", with
// a google.rpc.ErrorInfo (reason SERVICE_DISABLED, domain googleapis.com,
// metadata service, consumer, activationUrl, serviceTitle, containerInfo),
// a google.rpc.LocalizedMessage and a google.rpc.Help link.
//
// There is one check, made the way IAM enforcement makes its (#1879):
//
//   - Gate wraps a port's whole HTTP handler, before gRPC and JSON are told
//     apart, so one wrapper covers both. It takes the project from the
//     request's routing headers (x-goog-request-params,
//     google-cloud-resource-prefix), its path or its project query
//     parameter, and answers a gRPC call with a trailers-only status and a
//     REST call with Google's JSON error body.
//   - Interceptors check the gRPC calls Gate could not place, by the first
//     "projects/..." name in the request message, for the services
//     CloudBurrow serves in-process.
//   - The deciding is the host's: the Service Usage server knows each
//     project's disabled APIs. The servers that run in the cluster (Cloud
//     Storage, the Pub/Sub, BigQuery, Bigtable and call fronts) ask it at
//     Path on the host's Resource Manager port through Remote, which
//     keeps the answer for CacheFor, as Google's own disable takes a while
//     to reach every server.
//
// Every served API is enabled in every project unless someone disables it,
// so nothing is refused until then.
package apienable

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Denial is a call refused because its API is disabled in its project.
type Denial struct {
	// Service is the API's name ("pubsub.googleapis.com"), Title its
	// display name ("Cloud Pub/Sub API").
	Service string
	Title   string
	// Number is the project's number, which Google names it by.
	Number int64
}

// ActivationURL is where Google's console enables the API.
func (d Denial) ActivationURL() string {
	return "https://console.developers.google.com/apis/api/" + d.Service + "/overview?project=" + strconv.FormatInt(d.Number, 10)
}

func (d Denial) title() string {
	if d.Title != "" {
		return d.Title
	}
	return d.Service
}

// Message is Google's text.
func (d Denial) Message() string {
	return fmt.Sprintf("%s has not been used in project %d before or it is disabled. Enable it by visiting %s then retry. "+
		"If you enabled this API recently, wait a few minutes for the action to propagate to our systems and retry.",
		d.title(), d.Number, d.ActivationURL())
}

// Metadata is the ErrorInfo's metadata.
func (d Denial) Metadata() map[string]string {
	n := strconv.FormatInt(d.Number, 10)
	return map[string]string{
		"service":       d.Service,
		"consumer":      "projects/" + n,
		"activationUrl": d.ActivationURL(),
		"serviceTitle":  d.title(),
		"containerInfo": n,
	}
}

const (
	// Reason and Domain are the ErrorInfo's.
	Reason = "SERVICE_DISABLED"
	Domain = "googleapis.com"
	// helpDescription is the Help link's, as Google words it.
	helpDescription = "Google developers console API activation"
)

// Status is the denial as a gRPC status, with its details.
func (d Denial) Status() *status.Status {
	st := status.New(codes.PermissionDenied, d.Message())
	withDetails, err := st.WithDetails(
		&errdetails.ErrorInfo{Reason: Reason, Domain: Domain, Metadata: d.Metadata()},
		&errdetails.LocalizedMessage{Locale: "en-US", Message: d.Message()},
		&errdetails.Help{Links: []*errdetails.Help_Link{{Description: helpDescription, Url: d.ActivationURL()}}},
	)
	if err != nil {
		return st
	}
	return withDetails
}

// Err is Status as an error.
func (d Denial) Err() error { return d.Status().Err() }

// JSON is Google's JSON error body: the google.rpc.Status of its error
// model (https://cloud.google.com/apis/design/errors#http_mapping), which
// the official clients parse the ErrorInfo from.
func (d Denial) JSON() map[string]any {
	msg := d.Message()
	return map[string]any{"error": map[string]any{
		"code":    http.StatusForbidden,
		"message": msg,
		"status":  "PERMISSION_DENIED",
		"details": []any{
			map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": Reason, "domain": Domain, "metadata": d.Metadata()},
			map[string]any{"@type": "type.googleapis.com/google.rpc.LocalizedMessage", "locale": "en-US", "message": msg},
			map[string]any{"@type": "type.googleapis.com/google.rpc.Help", "links": []any{
				map[string]any{"description": helpDescription, "url": d.ActivationURL()}}},
		},
	}}
}

// WriteJSON answers the denial as JSON.
func (d Denial) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(d.JSON())
}

// WriteGRPC answers a gRPC call over HTTP/2 with a trailers-only status,
// as a gRPC server refusing a call before its handler does.
func (d Denial) WriteGRPC(w http.ResponseWriter) {
	st := d.Status()
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Status", strconv.Itoa(int(st.Code())))
	h.Set("Grpc-Message", encodeGRPCMessage(st.Message()))
	if b, err := proto.Marshal(st.Proto()); err == nil {
		h.Set("Grpc-Status-Details-Bin", base64.RawStdEncoding.EncodeToString(b))
	}
	w.WriteHeader(http.StatusOK)
}

// encodeGRPCMessage percent-encodes as gRPC's grpc-message requires.
func encodeGRPCMessage(msg string) string {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c >= ' ' && c <= '~' && c != '%' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Decider decides whether service is disabled in project (an ID or a
// number), with the denial to answer when it is.
type Decider interface {
	Disabled(ctx context.Context, project, service string) (Denial, bool)
}

// IsGRPC reports whether r is a gRPC call.
func IsGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// ProjectOfName is the project a resource name names ("projects/p/..."),
// "" for none or the wildcard "_" or "-".
func ProjectOfName(name string) string {
	name = strings.TrimPrefix(name, "/")
	if i := strings.Index(name, "projects/"); i >= 0 && (i == 0 || name[i-1] == '/') {
		rest := name[i+len("projects/"):]
		p, _, _ := strings.Cut(rest, "/")
		if p, _, _ = strings.Cut(p, ":"); p != "" && p != "_" && p != "-" {
			return p
		}
	}
	return ""
}

// ProjectOf is the project r is made in: the x-goog-user-project header
// (the quota project, which Google bills and checks), else what the
// routing headers, the path or the project query parameter name. "" is a
// request that names none, which is not checked.
func ProjectOf(r *http.Request) string {
	if p := ProjectOfHeaders(r.Header.Values); p != "" {
		return p
	}
	if IsGRPC(r) {
		return ""
	}
	if p := ProjectOfName(r.URL.Path); p != "" {
		return p
	}
	if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" {
		return p
	}
	if p := strings.TrimSpace(r.URL.Query().Get("projectId")); p != "" {
		return p
	}
	return ""
}

// ProjectOfHeaders is the project a call's headers name (get is
// http.Header.Values, or gRPC metadata's Get): the quota project, else the
// routing headers'.
func ProjectOfHeaders(get func(string) []string) string {
	for _, v := range get("X-Goog-User-Project") {
		if p := strings.TrimSpace(v); p != "" {
			return p
		}
	}
	for _, v := range get("Google-Cloud-Resource-Prefix") {
		if p := ProjectOfName(v); p != "" {
			return p
		}
	}
	for _, v := range get("X-Goog-Request-Params") {
		for _, kv := range strings.Split(v, "&") {
			_, val, _ := strings.Cut(kv, "=")
			if u, err := url.QueryUnescape(val); err == nil {
				val = u
			}
			if p := ProjectOfName(val); p != "" {
				return p
			}
		}
	}
	return ""
}
