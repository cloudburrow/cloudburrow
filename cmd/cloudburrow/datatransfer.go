package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// dataTransferService keys the BigQuery Data Transfer Service (#1948),
// served beside BigQuery.
const dataTransferService config.Service = "datatransfer"

// dataTransferSvc serves the BigQuery Data Transfer Service in the CLI
// process, gRPC and JSON on one port as bigquerydatatransfer.googleapis.com
// serves both. Its runs execute on the instance's BigQuery through the
// official client, and its clock is BigQuery's, so the clock:advance
// extension that moves BigQuery's time also fires transfer schedules.
type dataTransferSvc struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	// wrapJSON, when set, wraps the JSON API (IAM enforcement, #1879).
	wrapJSON func(http.Handler) http.Handler
	requests func(rest.Request)
	bigquery func() *netfwd.Forwarder
	pubsub   func() *netfwd.Forwarder
	// registry is the project registry, which names a project by number.
	registry func() *resourcemanager.Registry

	server *grpctransport.Server
	// bound is the bound address once Start has returned, which the
	// BigQuery tunnel's Local hook reads from its own goroutines.
	bound  atomic.Pointer[string]
	api    *datatransfer.Server
	db     store.Store
	cancel context.CancelFunc
	done   chan struct{}
}

func newDataTransferService(cfg config.Config) *dataTransferSvc {
	if !dataTransferEnabled(cfg) {
		return nil
	}
	return &dataTransferSvc{cfg: cfg}
}

// dataTransferEnabled is whether `up` binds the Data Transfer port: when
// BigQuery is enabled, as the service runs on it.
func dataTransferEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.DataTransfer >= 0
}

func (s *dataTransferSvc) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *dataTransferSvc) Name() string { return "datatransfer" }

// API is the server, available after Start.
func (s *dataTransferSvc) API() *datatransfer.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *dataTransferSvc) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *dataTransferSvc) Start(ctx context.Context) error {
	var db store.Store = store.NewMemory()
	if s.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "datatransfer"))
		if err != nil {
			return fmt.Errorf("open BigQuery Data Transfer state: %w", err)
		}
		db = durable
	}
	s.db = db
	bq := bqExecutor{tunnel: s.bigquery}
	api, err := datatransfer.NewServer(db, &bqClock{tunnel: s.bigquery}, bq, dtPublisher{tunnel: s.pubsub})
	if err != nil {
		_ = db.Close()
		return err
	}
	s.api = api
	api.ResolveProjects(func(number string) (string, bool) {
		if s.registry == nil {
			return "", false
		}
		id, err := serviceUsageProjects{reg: s.registry()}.Resolve(number)
		return id, err == nil && id != number
	})
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.DataTransfer)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	locations := &datatransfer.LocationsServer{}
	register := func(g grpc.ServiceRegistrar) { api.Register(g); locations.Register(g) }
	var jsonAPI http.Handler = datatransfer.NewRESTHandler(register)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { register(g) }); err != nil {
		_ = db.Close()
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		_ = db.Close()
		return err
	}
	addr := s.server.Addr()
	s.bound.Store(&addr)
	// The runner outlives Start's context, until Stop.
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		api.Run(rctx, time.Second)
	}()
	return nil
}

// bigQueryLocal is the BigQuery tunnel's Local hook (netfwd.Forwarder):
// bq sends its Data Transfer calls to the address --api names, BigQuery's
// (bq_api_utils.get_tpc_root_url_from_flags gives every service the --api
// value), so that address answers Data Transfer's discovery document,
// $discovery/rest?version=v1, and passes /v1/ to this service.
// BigQuery's own API is /bigquery/v2/ and its discovery version=v2, which
// the BigQuery backend answers as before, and so is the BigQuery Connection
// API (#1755), whose /v1/projects/{p}/locations/{l}/connections paths the
// front serves on the same port: they are never Data Transfer's.
func (s *dataTransferSvc) bigQueryLocal(r *http.Request) http.Handler {
	switch {
	case connectionAPIPath.MatchString(r.URL.Path):
		return nil
	case r.URL.Path == "/$discovery/rest" && r.URL.Query().Get("version") == "v1":
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			doc, err := datatransfer.DiscoveryDocument("http://" + r.Host + "/")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			_, _ = w.Write(doc)
		})
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		bound := s.bound.Load()
		if bound == nil {
			return nil
		}
		addr := *bound
		return &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.Out.Host = addr
		}}
	}
	return nil
}

// connectionAPIPath is a BigQuery Connection API v1 path, the BigQuery
// front's (internal/bigqueryfront's connectionRoute), with or without a
// connection ID and a :getIamPolicy-style verb.
var connectionAPIPath = regexp.MustCompile(`^/v1/projects/[^/]+/locations/[^/]+/connections(?:[/:]|$)`)

func (s *dataTransferSvc) Stop(ctx context.Context) error {
	var errs []error
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
	if s.server != nil {
		errs = append(errs, s.server.Stop(ctx))
	}
	if s.db != nil {
		errs = append(errs, s.db.Close())
		s.db = nil
	}
	return errors.Join(errs...)
}

// dataTransferResetter forgets the transfer configs and their runs.
type dataTransferResetter struct{ svc *dataTransferSvc }

func (r *dataTransferResetter) Name() string { return "datatransfer" }

func (r *dataTransferResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *dataTransferResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the BigQuery Data Transfer Service has not started")
	}
	return api.Reset(project)
}

// bqClock is BigQuery's clock, read through its clock:advance extension
// with 0 seconds and cached for a moment, as a tick reads it many times.
// Before BigQuery answers, it is the host's time.
type bqClock struct {
	tunnel func() *netfwd.Forwarder
	mu     sync.Mutex
	offset time.Duration
	read   time.Time
}

func (c *bqClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.read) < 250*time.Millisecond {
		return now.Add(c.offset)
	}
	c.read = now
	if t, err := bigQueryNow(c.tunnel); err == nil {
		c.offset = t.Sub(now)
	}
	return now.Add(c.offset)
}

func bigQueryNow(tunnel func() *netfwd.Forwarder) (time.Time, error) {
	f := tunnel()
	if f == nil || f.HostAddr() == "" {
		return time.Time{}, errors.New("BigQuery has not started on this instance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+f.HostAddr()+"/cloudburrow/bigquery/v2/clock:advance",
		strings.NewReader(`{"seconds":0}`))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	var got struct {
		Time time.Time `json:"time"`
	}
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("BigQuery's clock: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Time.IsZero() {
		return time.Time{}, fmt.Errorf("BigQuery's clock: %v", err)
	}
	return got.Time, nil
}

// bqExecutor runs transfer runs on the instance's BigQuery with the
// official client, as the service's own jobs.
type bqExecutor struct{ tunnel func() *netfwd.Forwarder }

func (e bqExecutor) client(ctx context.Context, project string) (*bigquery.Client, error) {
	f := e.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, apierror.FailedPrecondition("BigQuery has not started on this instance")
	}
	return bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithoutAuthentication())
}

// bqError keeps BigQuery's HTTP status as the gRPC code a run reports.
func bqError(err error) error {
	var g *googleapi.Error
	if errors.As(err, &g) {
		switch g.Code {
		case http.StatusNotFound:
			return apierror.NotFound("%s", g.Message)
		case http.StatusBadRequest:
			return apierror.InvalidArgument("%s", g.Message)
		case http.StatusConflict:
			return apierror.AlreadyExists("%s", g.Message)
		}
	}
	return err
}

func (e bqExecutor) DatasetLocation(ctx context.Context, project, dataset string) (string, error) {
	c, err := e.client(ctx, project)
	if err != nil {
		return "", err
	}
	defer c.Close()
	md, err := c.Dataset(dataset).Metadata(ctx)
	if err != nil {
		return "", bqError(err)
	}
	return md.Location, nil
}

func (e bqExecutor) RunQuery(ctx context.Context, j datatransfer.QueryJob) (string, int64, error) {
	c, err := e.client(ctx, j.Project)
	if err != nil {
		return "", 0, err
	}
	defer c.Close()
	q := c.Query(j.Query)
	q.Location = j.Location
	// Only the parameters the query names: @run_time and @run_date.
	if strings.Contains(j.Query, "@run_time") {
		q.Parameters = append(q.Parameters, bigquery.QueryParameter{Name: "run_time", Value: j.RunTime})
	}
	if strings.Contains(j.Query, "@run_date") {
		q.Parameters = append(q.Parameters, bigquery.QueryParameter{Name: "run_date", Value: civil.DateOf(j.RunTime)})
	}
	if j.DestTable != "" {
		q.Dst = c.Dataset(j.DestDataset).Table(j.DestTable)
		q.CreateDisposition = bigquery.CreateIfNeeded
		q.WriteDisposition = bigquery.TableWriteDisposition(j.WriteDisposition)
		if j.PartitioningField != "" {
			q.TimePartitioning = &bigquery.TimePartitioning{Field: j.PartitioningField}
		}
	}
	job, err := q.Run(ctx)
	if err != nil {
		return "", 0, bqError(err)
	}
	st, err := job.Wait(ctx)
	if err != nil {
		return job.ID(), 0, bqError(err)
	}
	if err := st.Err(); err != nil {
		return job.ID(), 0, bqError(err)
	}
	var rows int64
	if j.DestTable != "" {
		if md, err := q.Dst.Metadata(ctx); err == nil {
			rows = int64(md.NumRows)
		}
	}
	return job.ID(), rows, nil
}

func (e bqExecutor) CopyTables(ctx context.Context, j datatransfer.CopyJob) ([]string, error) {
	c, err := e.client(ctx, j.Project)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	src := c.DatasetInProject(j.SourceProject, j.SourceDataset)
	var copied []string
	it := src.Tables(ctx)
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return copied, bqError(err)
		}
		md, err := t.Metadata(ctx)
		if err != nil {
			return copied, bqError(err)
		}
		// Views, routines and external tables are not copied, as Google
		// documents.
		if md.Type != bigquery.RegularTable {
			continue
		}
		dst := c.Dataset(j.DestDataset).Table(t.TableID)
		if _, err := dst.Metadata(ctx); err == nil && !j.Overwrite {
			continue // appending is not supported; the existing table stays
		}
		cp := dst.CopierFrom(t)
		cp.WriteDisposition = bigquery.WriteTruncate
		cp.CreateDisposition = bigquery.CreateIfNeeded
		job, err := cp.Run(ctx)
		if err != nil {
			return copied, bqError(err)
		}
		st, err := job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			return copied, bqError(err)
		}
		copied = append(copied, t.TableID)
	}
	return copied, nil
}

// dtPublisher publishes run notifications to the instance's Pub/Sub.
type dtPublisher struct{ tunnel func() *netfwd.Forwarder }

func (p dtPublisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	f := p.tunnel()
	if f == nil || f.HostAddr() == "" {
		return errors.New("Pub/Sub is not enabled on this instance")
	}
	return publishLocal(ctx, f, topic, data, attrs)
}
