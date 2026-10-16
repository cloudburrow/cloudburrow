package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// loggingService serves the Cloud Logging write and read API (#304) in the
// CLI process. Its store is bounded and in memory in every mode: a log store
// that grew without limit on a laptop would be a problem of its own.
type loggingService struct {
	cfg config.Config
	// tracing, when on, spans gRPC calls (#600), as for Cloud Tasks.
	tracing *telemetry.Tracing
	calls   grpctransport.Observer
	// interpose run inside the call observer, in order: the request log
	// (#314), which is what `cloudburrow logs --service logging` reads
	// (#587), then fault injection (#600), so the log sees injected faults.
	interpose []grpc.UnaryServerInterceptor
	// requests reports each JSON request to the admin event log (#591).
	requests func(rest.Request)
	console  *console.Recorder
	server   *grpctransport.Server
	store    *logging.Store
	// api is the one LoggingServiceV2 server: gRPC, JSON and the console's
	// Delete log (#799) all call it.
	api *logging.Server
	// admin stores log-based metrics, sinks and exclusions (#1226), durable
	// in persistent mode; set by Start.
	admin *logging.Admin
	db    store.Store
	// storage is the Cloud Storage tunnel CopyLogEntries writes through
	// (#1394), or nil; without Cloud Storage on the instance, the copy says
	// so.
	storage func() *netfwd.Forwarder
	// hierarchy is Resource Manager's registry, whose folders and
	// organization are Logging parents (#1395); nil when it is not open.
	hierarchy *resourcemanager.Registry
	// pubsub and bigquery are the tunnels sinks deliver through (#1404),
	// or nil.
	pubsub, bigquery func() *netfwd.Forwarder
	// stopDelivery ends the Cloud Storage sinks' hourly writes.
	stopDelivery context.CancelFunc
}

// storageObjectWriter uploads an object to the instance's Cloud Storage
// with the JSON API's media upload, as any client would.
type storageObjectWriter struct {
	src    storageSourceFetcher // for its base(): the tunnel, else the port
	client *http.Client
}

func (w storageObjectWriter) WriteObject(ctx context.Context, bucket, name, contentType string, data []byte) error {
	u := w.src.base() + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	_, msg, _, _ := apierror.DecodeJSON(io.LimitReader(resp.Body, 1<<16))
	if msg == "" {
		msg = resp.Status
	}
	if resp.StatusCode == http.StatusNotFound {
		// A sink reports a missing bucket as a sink error (#1404).
		return fmt.Errorf("gs://%s/%s: %s: %w", bucket, name, msg, logging.ErrDestinationNotFound)
	}
	return fmt.Errorf("gs://%s/%s: %s", bucket, name, msg)
}

// useHierarchy makes the registry's folders and organization Logging
// parents. It is called before Start.
func (s *loggingService) useHierarchy(reg *resourcemanager.Registry) {
	if s != nil {
		s.hierarchy = reg
	}
}

// loggingHierarchy is logging.Hierarchy over Resource Manager's registry.
type loggingHierarchy struct{ reg *resourcemanager.Registry }

func (h loggingHierarchy) Exists(name string) error {
	switch {
	case name == resourcemanager.OrganizationName:
		return nil
	case strings.HasPrefix(name, "folders/"):
		_, err := h.reg.Folder(name)
		return err
	}
	return fmt.Errorf("%s is not a folder or the organization %s", name, resourcemanager.OrganizationName)
}

func (h loggingHierarchy) Ancestors(name string) ([]string, error) {
	var out []string
	if id, ok := strings.CutPrefix(name, "projects/"); ok {
		chain, err := h.reg.Ancestry(id)
		if err != nil {
			return nil, err
		}
		for _, a := range chain[1:] {
			out = append(out, a.Type+"s/"+a.ID)
		}
		return out, nil
	}
	for parent, i := name, 0; strings.HasPrefix(parent, "folders/") && i <= resourcemanager.MaxFolderDepth+1; i++ {
		f, err := h.reg.Folder(parent)
		if err != nil {
			return nil, err
		}
		parent = f.Parent
		if parent != "" {
			out = append(out, parent)
		}
	}
	return out, nil
}

// Admin is the metrics, sinks and exclusions server, or nil before Start.
func (s *loggingService) Admin() *logging.Admin {
	if s == nil {
		return nil
	}
	return s.admin
}

func newLoggingService(cfg config.Config) *loggingService {
	if !serviceEnabled(cfg, config.ServiceLogging) {
		return nil
	}
	store := logging.NewStore(logging.DefaultLimit)
	return &loggingService{cfg: cfg, store: store, api: logging.NewServer(store)}
}

func (s *loggingService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *loggingService) Name() string { return "logging" }

// Addr is the API's bound address.
func (s *loggingService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// toConsole sends API-written entries to the console's Logs Explorer, so
// what an application logged through the API is where the pod logs are.
func (s *loggingService) toConsole(rec *console.Recorder) {
	if s == nil || rec == nil {
		return
	}
	s.store.Observe(func(e *loggingpb.LogEntry) { rec.Log(consoleEntry(e)) })
}

// consoleLogDeleter is the Logs Explorer's Delete log (#799): DeleteLog on the
// same server the gRPC and JSON transports serve, with the instance's fault
// rules applied first, as the service's interceptor applies them, so an
// injected DeleteLog fault fails the console with the message an SDK gets.
type consoleLogDeleter struct {
	svc    *loggingService
	faults *admin.Faults
}

func (d consoleLogDeleter) DeleteLog(ctx context.Context, logName string) error {
	// The interceptor names a call's resource by the request's name or
	// parent, and DeleteLogRequest has neither, so it passes none; so does
	// this, or a project-scoped rule would fail the console and not the SDK.
	if err := d.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_DeleteLog_FullMethodName, ""); err != nil {
		return err
	}
	_, err := d.svc.api.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: logName})
	return err
}

// logDeleter is the console's Delete log, or nil when Logging is not served.
func (s *loggingService) logDeleter(faults *admin.Faults) console.LogDeleter {
	if s == nil {
		return nil
	}
	return consoleLogDeleter{svc: s, faults: faults}
}

// consoleSeverity folds Logging's nine levels onto the console's four.
func consoleSeverity(s string) console.Severity {
	switch s {
	case "DEBUG", "DEFAULT":
		return console.SeverityDefault
	case "INFO", "NOTICE":
		return console.SeverityInfo
	case "WARNING":
		return console.SeverityWarning
	default:
		return console.SeverityError
	}
}

// entryMessage is the payload as one line: text as it is, structured
// payloads as compact JSON.
func entryMessage(e *loggingpb.LogEntry) string {
	switch p := e.GetPayload().(type) {
	case *loggingpb.LogEntry_TextPayload:
		return p.TextPayload
	case *loggingpb.LogEntry_JsonPayload:
		if b, err := protojson.Marshal(p.JsonPayload); err == nil {
			var compact json.RawMessage = b
			if c, err := json.Marshal(compact); err == nil {
				return string(c)
			}
			return string(b)
		}
	case *loggingpb.LogEntry_ProtoPayload:
		return "(proto payload " + p.ProtoPayload.GetTypeUrl() + ")"
	}
	return ""
}

func (s *loggingService) Start(ctx context.Context) error {
	var db store.Store = store.NewMemory()
	if s.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "logging"))
		if err != nil {
			return fmt.Errorf("open Cloud Logging state: %w", err)
		}
		db = durable
	}
	s.db = db
	s.admin = logging.NewAdmin(db)
	if s.hierarchy != nil {
		s.admin.UseHierarchy(loggingHierarchy{s.hierarchy})
	}
	s.api.UseAdmin(s.admin)
	if serviceEnabled(s.cfg, config.ServiceStorage) {
		src := storageSourceFetcher{cfg: s.cfg}
		if s.storage != nil {
			src.addr = func() string {
				if f := s.storage(); f != nil {
					return f.HostAddr()
				}
				return ""
			}
		}
		s.admin.UseStorage(storageObjectWriter{src: src, client: &http.Client{Timeout: 2 * time.Minute}})
	}
	// Sinks deliver to the instance's Cloud Storage, Pub/Sub and BigQuery
	// (#1404).
	s.api.UseDestinations(s.destinations())
	dctx, cancel := context.WithCancel(context.Background())
	s.stopDelivery = cancel
	go s.api.RunDelivery(dctx)
	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Logging))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	// One port for gRPC and JSON, as logging.googleapis.com (#591): the same
	// server, transcoded, for REST clients and gcloud.
	register := func(g grpc.ServiceRegistrar) { s.api.Register(g); s.admin.Register(g) }
	var jsonAPI http.Handler = logging.NewRESTHandler(register)
	// The Logging clock (#1404), a CloudBurrow extension on the API's port.
	transcoded := jsonAPI
	jsonAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == logging.ClockPath {
			s.api.ServeClock(w, r)
			return
		}
		transcoded.ServeHTTP(w, r)
	})
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *loggingService) Stop(ctx context.Context) error {
	if s.stopDelivery != nil {
		s.stopDelivery()
	}
	var err error
	if s.server != nil {
		err = s.server.Stop(ctx)
	}
	if s.db != nil {
		if cerr := s.db.Close(); err == nil {
			err = cerr
		}
		s.db = nil
	}
	return err
}

// loggingResetter clears every entry, metric, sink and exclusion.
type loggingResetter struct{ svc *loggingService }

func (r *loggingResetter) Name() string { return "logging" }

func (r *loggingResetter) Reset(context.Context) error {
	if r.svc == nil || r.svc.store == nil {
		return errors.New("Cloud Logging is not enabled")
	}
	r.svc.store.Reset()
	// And the metrics, sinks and exclusions (#1226).
	if db := r.svc.db; db != nil {
		keys, err := db.List("logging/")
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := db.Delete(k); err != nil {
				return err
			}
		}
	}
	return nil
}
