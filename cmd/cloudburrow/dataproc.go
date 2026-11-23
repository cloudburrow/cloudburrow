package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// dataprocService serves the Dataproc API's serverless surface (#2156),
// Serverless for Apache Spark, in the CLI process: gRPC and JSON on one
// port, with the Spark Connect bridge to its sessions. Its batches and
// sessions run on the Spark runner of BigQuery's stored procedures
// (sparkrunner.go), their files and driver output in the instance's
// Cloud Storage, their lines in its Cloud Logging.
type dataprocService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement); nil is none.
	wrapJSON func(http.Handler) http.Handler
	// spark is the Spark runner's engine, nil without one.
	spark func() *sparkproc.Docker
	// live is the instance's bound addresses, its Cloud Storage's among
	// them.
	live func() map[string]string
	// caller is a request's principal, a resource's creator.
	caller func(ctx context.Context) string
	// gke is the Kubernetes Engine API, Dataproc on GKE's clusters' (nil:
	// none served).
	gke    func() *gke.Server
	server *grpctransport.Server
	api    *dataproc.Server
}

func newDataprocService(cfg config.Config) *dataprocService {
	if !dataprocEnabled(cfg) {
		return nil
	}
	return &dataprocService{cfg: cfg}
}

// dataprocEnabled is whether `up` binds the Dataproc port: with dataproc
// selected and BigQuery beside it, whose Spark runner runs its workloads.
func dataprocEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceDataproc) && cfg.Endpoints.Dataproc >= 0
}

func (s *dataprocService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *dataprocService) Name() string { return "dataproc" }

// API is the server, available after Start.
func (s *dataprocService) API() *dataproc.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *dataprocService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *dataprocService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "dataproc.json")
	}
	opts := dataproc.Options{State: state, Caller: s.caller,
		Logf: func(format string, args ...any) { fmt.Printf("cloudburrow: "+format+"\n", args...) }}
	if s.spark != nil {
		if d := s.spark(); d != nil {
			opts.Engine = dataproc.Docker{D: d}
			opts.HistoryDir = d.History
		}
	}
	opts.InstanceID = s.cfg.Name
	opts.AllowUnconfinedContainers = s.cfg.AllowUnconfinedContainers
	if s.gke != nil {
		opts.Virtual = dataprocGKE{api: s.gke, dir: filepath.Join(s.cfg.InstanceDir(), "dataproc-gke")}
	}
	if s.live != nil && serviceEnabled(s.cfg, config.ServiceStorage) {
		opts.Storage = &dataprocStorage{live: s.live}
	}
	opts.ProjectNumber = func(project string) string { return strconv.FormatInt(resource.ProjectNumber(project), 10) }
	api, err := dataproc.NewServer(opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Dataproc)))
	s.server.Observe(s.calls)
	// Spark Connect calls on the port itself reach their sessions.
	s.server.ServerOptions(api.ProxyOption())
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = dataproc.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	inner := jsonAPI
	jsonAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(dataproc.WithAuthority(r.Context(), r.Host)))
	})
	// The WebSocket bridge outside every wrapper, which would hide the
	// connection's Hijacker.
	// The History Servers (the Spark UI, #2162) beside the JSON API.
	s.server.ServeHTTP(api.Bridge(api.WebUI(jsonAPI)))
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *dataprocService) Stop(ctx context.Context) error {
	var errs []error
	if s.server != nil {
		errs = append(errs, s.server.Stop(ctx))
	}
	if s.api != nil {
		s.api.Close()
	}
	return errors.Join(errs...)
}

// dataprocResetter forgets the batches, sessions and templates, stopping
// what runs.
type dataprocResetter struct{ svc *dataprocService }

func (r *dataprocResetter) Name() string { return "dataproc" }

func (r *dataprocResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *dataprocResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the Dataproc API has not started")
	}
	return api.Reset(project)
}

// dataprocStorage is the instance's Cloud Storage through the official
// client, as a workload's files are read and its output written.
type dataprocStorage struct {
	live func() map[string]string
}

func (d *dataprocStorage) client(ctx context.Context) (*storage.Client, error) {
	addr := d.live()["storage"]
	if addr == "" {
		return nil, errors.New("Cloud Storage is not served by this instance")
	}
	return storage.NewClient(ctx, option.WithEndpoint("http://"+addr+"/storage/v1/"), option.WithoutAuthentication(),
		storage.WithJSONReads(), storage.WithDisabledClientMetrics())
}

func dataprocGS(uri string) (string, string, error) {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if !strings.HasPrefix(uri, "gs://") || !ok || bucket == "" || object == "" {
		return "", "", fmt.Errorf("%q is not gs://bucket/object", uri)
	}
	return bucket, object, nil
}

func (d *dataprocStorage) Read(ctx context.Context, uri string) ([]byte, error) {
	bucket, object, err := dataprocGS(uri)
	if err != nil {
		return nil, err
	}
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	r, err := c.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, sparkproc.MaxJobBytes))
}

func (d *dataprocStorage) Write(ctx context.Context, uri string, data []byte) error {
	bucket, object, err := dataprocGS(uri)
	if err != nil {
		return err
	}
	c, err := d.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	w := c.Bucket(bucket).Object(object).NewWriter(ctx)
	w.ContentType = "text/plain; charset=utf-8"
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// List lists the bucket's objects under prefix, by name with their sizes.
func (d *dataprocStorage) List(ctx context.Context, bucket, prefix string) (map[string]int64, error) {
	c, err := d.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := map[string]int64{}
	it := c.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[a.Name] = a.Size
	}
}

func (d *dataprocStorage) EnsureBucket(ctx context.Context, project, bucket, location string) error {
	c, err := d.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Bucket(bucket).Attrs(ctx); err == nil {
		return nil
	}
	err = c.Bucket(bucket).Create(ctx, project, &storage.BucketAttrs{Location: location})
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusConflict {
		return nil
	}
	return err
}
