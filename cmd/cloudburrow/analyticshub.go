package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/analyticshub"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// analyticsHubService keys BigQuery sharing (Analytics Hub, #1960), served
// beside BigQuery.
const analyticsHubService config.Service = "analyticshub"

// analyticsHubSvc serves analyticshub.googleapis.com v1 in the CLI process,
// gRPC and JSON on one port as Google serves both. Its linked datasets are
// made in the instance's BigQuery, through the BigQuery front.
type analyticsHubSvc struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	// enforce are the IAM checks (#1989), innermost on gRPC and on every
	// JSON call too, which passes no other interceptor.
	enforce  []grpc.UnaryServerInterceptor
	requests func(rest.Request)
	// orgOf names a project's organization, for ListOrgDataExchanges.
	orgOf    func(project string) string
	bigquery func() *netfwd.Forwarder
	server   *grpctransport.Server
	api      *analyticshub.Server
}

func newAnalyticsHubService(cfg config.Config) *analyticsHubSvc {
	if !analyticsHubEnabled(cfg) {
		return nil
	}
	return &analyticsHubSvc{cfg: cfg}
}

// analyticsHubEnabled is whether `up` binds the Analytics Hub port: when
// BigQuery is enabled, as its linked datasets are BigQuery's.
func analyticsHubEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.AnalyticsHub >= 0
}

func (s *analyticsHubSvc) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *analyticsHubSvc) Name() string { return "analyticshub" }

// API is the server, available after Start.
func (s *analyticsHubSvc) API() *analyticshub.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *analyticsHubSvc) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *analyticsHubSvc) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "analyticshub.json")
	}
	api, err := analyticshub.NewServer(state, hubLinker{tunnel: s.bigquery, client: &http.Client{Timeout: 2 * time.Minute}}, nil)
	if err != nil {
		return err
	}
	s.api = api
	if s.orgOf != nil {
		api.OrganizationOf(s.orgOf)
	}
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.AnalyticsHub)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	for _, i := range s.enforce {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = analyticshub.NewRESTHandler(api.Register, s.enforce...)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *analyticsHubSvc) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// analyticsHubResetter forgets the data exchanges, listings and
// subscriptions. The linked datasets are BigQuery's, which its own reset
// deletes.
type analyticsHubResetter struct{ svc *analyticsHubSvc }

func (r *analyticsHubResetter) Name() string { return "analyticshub" }

func (r *analyticsHubResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *analyticsHubResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Analytics Hub has not started")
	}
	return api.Reset(project)
}

// hubLinker makes linked datasets on the instance's BigQuery, through the
// front, with bigqueryfront.HubLinkHeader on each of its requests.
type hubLinker struct {
	tunnel func() *netfwd.Forwarder
	client *http.Client
}

func (h hubLinker) do(ctx context.Context, method, path string, link *analyticshub.LinkedDataset, body any) ([]byte, error) {
	f := h.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, apierror.FailedPrecondition("BigQuery has not started on this instance")
	}
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+f.HostAddr()+"/bigquery/v2"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if link != nil {
		m := map[string]any{"sourceProject": link.SourceProject, "sourceDataset": link.SourceDataset, "tables": link.Tables,
			"restricted": link.Restricted, "queryResult": link.RestrictQueryResult, "revoked": link.Revoked}
		if link.CleanRoom {
			// A data clean room's one linked dataset (#2101): each
			// listing's table, view or routine, from its own dataset.
			res := make([]map[string]any, 0, len(link.Resources))
			for _, r := range link.Resources {
				res = append(res, map[string]any{"sourceProject": r.SourceProject, "sourceDataset": r.SourceDataset,
					"table": r.Table, "routine": r.Routine, "queryResult": r.RestrictQueryResult})
			}
			m["cleanRoom"], m["resources"] = true, res
		}
		v, _ := json.Marshal(m)
		req.Header.Set(bigqueryfront.HubLinkHeader, string(v))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, apierror.Internal(err, "BigQuery: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode/100 == 2 {
		return buf.Bytes(), nil
	}
	_, msg, _, _ := apierror.DecodeJSON(bytes.NewReader(buf.Bytes()))
	if msg == "" {
		msg = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, apierror.NotFound("%s", msg)
	case http.StatusConflict:
		return nil, apierror.AlreadyExists("%s", msg)
	case http.StatusBadRequest:
		return nil, apierror.InvalidArgument("%s", msg)
	}
	return nil, apierror.Internal(fmt.Errorf("BigQuery %s %s: %s", method, path, resp.Status), "BigQuery: %s", msg)
}

func datasetPath(project, dataset string) string {
	return "/projects/" + url.PathEscape(project) + "/datasets/" + url.PathEscape(dataset)
}

func (h hubLinker) DatasetLocation(ctx context.Context, project, dataset string) (string, error) {
	b, err := h.do(ctx, http.MethodGet, datasetPath(project, dataset), nil, nil)
	if err != nil {
		if apierror.From(err).HTTPStatus() == http.StatusNotFound {
			return "", apierror.NotFound("The shared dataset projects/%s/datasets/%s was not found in BigQuery", project, dataset)
		}
		return "", err
	}
	var ds struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal(b, &ds); err != nil {
		return "", err
	}
	return ds.Location, nil
}

func (h hubLinker) CreateLinked(ctx context.Context, l analyticshub.LinkedDataset) error {
	body := map[string]any{
		"datasetReference": map[string]string{"projectId": l.Project, "datasetId": l.Dataset},
		"location":         l.Location,
	}
	if l.FriendlyName != "" {
		body["friendlyName"] = l.FriendlyName
	}
	if l.Description != "" {
		body["description"] = l.Description
	}
	if len(l.Labels) > 0 {
		body["labels"] = l.Labels
	}
	_, err := h.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(l.Project)+"/datasets", &l, body)
	return err
}

func (h hubLinker) UpdateLinked(ctx context.Context, l analyticshub.LinkedDataset) error {
	_, err := h.do(ctx, http.MethodPatch, datasetPath(l.Project, l.Dataset), &l, map[string]any{})
	return err
}

func (h hubLinker) DeleteLinked(ctx context.Context, project, dataset string) error {
	_, err := h.do(ctx, http.MethodDelete, datasetPath(project, dataset)+"?deleteContents=true",
		&analyticshub.LinkedDataset{Project: project, Dataset: dataset}, nil)
	if err != nil && apierror.From(err).HTTPStatus() == http.StatusNotFound {
		return nil
	}
	return err
}
