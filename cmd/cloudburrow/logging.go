package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// loggingService serves the Cloud Logging write and read API (#304) in the
// CLI process. Its store is bounded and in memory in every mode: a log store
// that grew without limit on a laptop would be a problem of its own.
type loggingService struct {
	cfg     config.Config
	calls   grpctransport.Observer
	console *console.Recorder
	server  *grpctransport.Server
	store   *logging.Store
}

func newLoggingService(cfg config.Config) *loggingService {
	if !serviceEnabled(cfg, config.ServiceLogging) {
		return nil
	}
	return &loggingService{cfg: cfg, store: logging.NewStore(logging.DefaultLimit)}
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
	s.store.Observe(func(e *loggingpb.LogEntry) {
		project, _, _ := strings.Cut(strings.TrimPrefix(e.GetLogName(), "projects/"), "/")
		rec.Log(console.Entry{
			Timestamp: e.GetTimestamp().AsTime(),
			Severity:  consoleSeverity(e.GetSeverity().String()),
			Source:    "logging/" + e.GetLogName()[strings.LastIndex(e.GetLogName(), "/")+1:],
			Project:   project,
			Resource:  e.GetLogName(),
			Message:   entryMessage(e),
		})
	})
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
	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Logging))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	if err := s.server.Register(func(g *grpc.Server) { logging.NewServer(s.store).Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *loggingService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// loggingResetter clears every entry.
type loggingResetter struct{ svc *loggingService }

func (r *loggingResetter) Name() string { return "logging" }

func (r *loggingResetter) Reset(context.Context) error {
	if r.svc == nil || r.svc.store == nil {
		return errors.New("Cloud Logging is not enabled")
	}
	r.svc.store.Reset()
	return nil
}
