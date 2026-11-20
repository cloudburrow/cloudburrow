package workflows

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves both APIs, their operations and locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	workflowspb.RegisterWorkflowsServer(g, s.Workflows())
	executionspb.RegisterExecutionsServer(g, s.Executions())
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{s: s})
}

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
	s *Server
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id, DisplayName: id}
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), locations.Regions, func(id string) (*locationpb.Location, error) { return mk(id), nil })
	if err != nil {
		return nil, err
	}
	page, next, err := paging.Page("locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mk(id))
	}
	return out, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := locationRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	for _, r := range locations.Regions {
		if r == m[2] {
			return &locationpb.Location{Name: req.GetName(), LocationId: r, DisplayName: r}, nil
		}
	}
	return nil, apierror.NotFound("Location '%s' was not found", req.GetName())
}

// mixins are the bindings workflows_v1.yaml adds: Locations and Operations.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.longrunning.Operations.DeleteOperation",
		Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.workflows.v1", "google.cloud.workflows.executions.v1"},
		Mixins: mixins, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

func protojsonMarshal(m proto.Message) ([]byte, error) { return protojson.Marshal(m) }

// NewRESTHandler is both JSON APIs over s, with the methods the Go gRPC
// clients do not have (step entries, callbacks, exportData,
// deleteExecutionHistory, triggerPubsubExecution) and the callback
// endpoints.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.serveExtra(w, r) {
			return
		}
		t.ServeHTTP(w, r)
	})
}

var (
	stepEntriesRE = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/workflows/[^/]+/executions/[^/:]+)/stepEntries(?:/([0-9]+))?$`)
	callbacksRE   = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/workflows/[^/]+/executions/[^/:]+)/callbacks(?:/([^/]+))?$`)
	execVerbRE    = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/workflows/[^/]+/executions/[^/:]+):(exportData|deleteExecutionHistory)$`)
	triggerRE     = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/workflows/[^/:]+):triggerPubsubExecution$`)
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// stepEntryJSON is a StepEntry in the API's JSON form.
func stepEntryJSON(execution string, e StepEntry) map[string]any {
	out := map[string]any{
		"name": execution + "/stepEntries/" + strconv.FormatInt(e.EntryID, 10), "step": e.Step, "routine": e.Routine,
		"entryId": strconv.FormatInt(e.EntryID, 10), "stepType": e.StepType, "state": e.State,
		"createTime": e.CreateTime, "updateTime": e.UpdateTime,
		"stepEntryMetadata": map[string]any{"threadId": e.ThreadID},
	}
	if e.Parent != 0 {
		out["navigationInfo"] = map[string]any{"parent": strconv.FormatInt(e.Parent, 10)}
	}
	if e.Exception != "" {
		out["exception"] = map[string]any{"payload": e.Exception}
	}
	return out
}

func (s *Server) serveExtra(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if m := stepEntriesRE.FindStringSubmatch(p); m != nil {
		if r.Method != http.MethodGet {
			apierror.WriteJSON(w, apierror.InvalidArgument("stepEntries are read with GET"))
			return true
		}
		entries, err := s.StepEntries(m[1])
		if err != nil {
			apierror.WriteJSON(w, err)
			return true
		}
		if m[2] != "" {
			id, _ := strconv.ParseInt(m[2], 10, 64)
			for _, e := range entries {
				if e.EntryID == id {
					writeJSON(w, stepEntryJSON(m[1], e))
					return true
				}
			}
			apierror.WriteJSON(w, apierror.NotFound("Step entry '%s/stepEntries/%s' was not found", m[1], m[2]))
			return true
		}
		q := r.URL.Query()
		if f := q.Get("filter"); f != "" {
			// Google's filter fields; the common ones are matched exactly.
			entries = filterEntries(entries, f)
		}
		if q.Get("orderBy") != "" && strings.Contains(q.Get("orderBy"), "desc") {
			for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
		size, _ := strconv.Atoi(q.Get("pageSize"))
		if size > 1000 {
			size = 1000
		}
		page, next, err := pageOf("steps:"+m[1]+q.Get("filter")+q.Get("orderBy"), entries, func(e StepEntry) string { return strconv.FormatInt(e.EntryID, 10) }, q.Get("pageToken"), size)
		if err != nil {
			apierror.WriteJSON(w, err)
			return true
		}
		list := []any{}
		for _, e := range page {
			list = append(list, stepEntryJSON(m[1], e))
		}
		out := map[string]any{"stepEntries": list, "totalSize": len(entries)}
		if next != "" {
			out["nextPageToken"] = next
		}
		writeJSON(w, out)
		return true
	}
	if m := callbacksRE.FindStringSubmatch(p); m != nil {
		if m[2] == "" {
			cbs, err := s.Callbacks(m[1])
			if err != nil {
				apierror.WriteJSON(w, err)
				return true
			}
			if cbs == nil {
				cbs = []CallbackInfo{}
			}
			writeJSON(w, map[string]any{"callbacks": cbs})
			return true
		}
		// A request to a callback endpoint.
		body, _ := io.ReadAll(io.LimitReader(r.Body, MaxMemoryBytes+1))
		if len(body) > MaxMemoryBytes {
			apierror.WriteJSON(w, apierror.InvalidArgument("the callback request is larger than %d bytes", MaxMemoryBytes))
			return true
		}
		headers := map[string]any{}
		for k, v := range r.Header {
			headers[k] = strings.Join(v, ", ")
		}
		query := map[string]any{}
		for k, v := range r.URL.Query() {
			l := []any{}
			for _, x := range v {
				l = append(l, x)
			}
			query[k] = l
		}
		req := map[string]any{"headers": headers, "method": r.Method, "query": query, "url": r.URL.Path,
			"body": decodeBody(r.Header.Get("Content-Type"), body)}
		if err := s.Deliver(m[1], m[2], r.Method, req); err != nil {
			apierror.WriteJSON(w, err)
			return true
		}
		writeJSON(w, map[string]any{})
		return true
	}
	if m := execVerbRE.FindStringSubmatch(p); m != nil {
		switch m[2] {
		case "exportData":
			entries, err := s.StepEntries(m[1])
			if err != nil {
				apierror.WriteJSON(w, err)
				return true
			}
			list := []any{}
			for _, e := range entries {
				list = append(list, stepEntryJSON(m[1], e))
			}
			b, _ := json.Marshal(map[string]any{"stepEntries": list})
			writeJSON(w, map[string]any{"data": string(b)})
		case "deleteExecutionHistory":
			if err := s.DeleteHistory(m[1]); err != nil {
				apierror.WriteJSON(w, err)
				return true
			}
			writeJSON(w, map[string]any{})
		}
		return true
	}
	if m := triggerRE.FindStringSubmatch(p); m != nil && r.Method == http.MethodPost {
		var in struct {
			Subscription string `json:"subscription"`
			Message      struct {
				Data        string            `json:"data"`
				Attributes  map[string]string `json:"attributes"`
				MessageID   string            `json:"messageId"`
				PublishTime string            `json:"publishTime"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("bad request body: %v", err))
			return true
		}
		if _, err := base64.StdEncoding.DecodeString(in.Message.Data); err != nil {
			apierror.WriteJSON(w, apierror.InvalidArgument("message.data must be base64"))
			return true
		}
		arg := PubsubCloudEvent(in.Subscription, in.Message.Data, in.Message.Attributes, in.Message.MessageID, in.Message.PublishTime)
		ex, err := s.Start(m[1], &executionspb.Execution{Argument: arg})
		if err != nil {
			apierror.WriteJSON(w, err)
			return true
		}
		b, _ := protojsonMarshal(ex)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(b)
		return true
	}
	return false
}

// PubsubCloudEvent is the argument an execution started from a Pub/Sub
// message receives: the CloudEvent Eventarc delivers.
func PubsubCloudEvent(subscription, data string, attrs map[string]string, id, publishTime string) string {
	topic := ""
	if i := strings.Index(subscription, "/subscriptions/"); i >= 0 {
		topic = subscription[:i]
	}
	ev := map[string]any{
		"specversion": "1.0", "type": "google.cloud.pubsub.topic.v1.messagePublished",
		"source": "//pubsub.googleapis.com/" + topic, "id": id, "time": publishTime,
		"datacontenttype": "application/json",
		"data": map[string]any{"subscription": subscription,
			"message": map[string]any{"data": data, "attributes": attrs, "messageId": id, "publishTime": publishTime}},
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func filterEntries(entries []StepEntry, filter string) []StepEntry {
	var out []StepEntry
	for _, e := range entries {
		ok := true
		for _, clause := range strings.Split(filter, " AND ") {
			k, v, found := strings.Cut(clause, "=")
			if !found {
				continue
			}
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
			switch k {
			case "step":
				ok = ok && e.Step == v
			case "routine":
				ok = ok && e.Routine == v
			case "state":
				ok = ok && e.State == v
			case "stepType":
				ok = ok && e.StepType == v
			case "entryId":
				ok = ok && strconv.FormatInt(e.EntryID, 10) == v
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}
