package datatransfer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dtpb "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// maxCatchUp bounds the runs one tick queues for a config whose clock
// jumped far ahead, so an advance of a year does not queue 100,000 runs.
const maxCatchUp = 500

func (s *Server) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run ticks every interval of real time, and whenever a run is queued,
// until ctx ends. The instance's clock decides what is due, so advancing it
// fires a schedule on the next tick.
func (s *Server) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Name is the worker's name.
func (s *Server) Name() string { return "bigquerydatatransfer" }

// Tick queues the scheduled runs now due and executes every pending run
// whose schedule time has come, one at a time per config, oldest first.
func (s *Server) Tick(ctx context.Context) {
	now := s.Now()
	s.mu.Lock()
	var due []*dtpb.TransferRun
	for _, rec := range s.configs {
		changed := false
		for i := 0; i < maxCatchUp && rec.Config.GetNextRunTime() != nil && !rec.Config.GetNextRunTime().AsTime().After(now); i++ {
			rt := rec.Config.GetNextRunTime().AsTime()
			s.addRun(rec, rt, rt)
			s.nextRun(rec, rt)
			changed = true
		}
		if changed {
			_ = s.save(rec)
		}
		var pending []*runRecord
		running := false
		for _, rr := range rec.Runs {
			switch rr.Run.GetState() {
			case dtpb.TransferState_RUNNING:
				running = true
			case dtpb.TransferState_PENDING:
				if !rr.Run.GetScheduleTime().AsTime().After(now) {
					pending = append(pending, rr)
				}
			}
		}
		if running || len(pending) == 0 {
			continue
		}
		sort.SliceStable(pending, func(i, j int) bool {
			return pending[i].Run.GetScheduleTime().AsTime().Before(pending[j].Run.GetScheduleTime().AsTime())
		})
		due = append(due, proto.Clone(pending[0].Run).(*dtpb.TransferRun))
	}
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].GetName() < due[j].GetName() })
	for _, run := range due {
		if ctx.Err() != nil {
			return
		}
		s.execute(ctx, run.GetName())
	}
}

// update changes a run under mu and saves its config.
func (s *Server) update(name string, f func(rec *record, rr *runRecord)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, rr, err := s.findRun(name)
	if err != nil {
		return
	}
	f(rec, rr)
	rr.Run.UpdateTime = timestamppb.New(s.Now())
	rec.Config.State = rr.Run.GetState()
	_ = s.save(rec)
}

func (s *Server) logf(rr *runRecord, sev dtpb.TransferMessage_MessageSeverity, format string, a ...any) {
	rr.Logs = append(rr.Logs, &dtpb.TransferMessage{MessageTime: timestamppb.New(s.Now()), Severity: sev,
		MessageText: fmt.Sprintf(format, a...)})
}

// execute runs one pending run to SUCCEEDED or FAILED.
func (s *Server) execute(ctx context.Context, name string) {
	var run *dtpb.TransferRun
	var cfg *dtpb.TransferConfig
	s.update(name, func(rec *record, rr *runRecord) {
		rr.Run.State = dtpb.TransferState_RUNNING
		rr.Run.StartTime = timestamppb.New(s.Now())
		s.logf(rr, dtpb.TransferMessage_INFO, "Dispatched run to data source with id %s", rr.Run.GetDataSourceId())
		run = proto.Clone(rr.Run).(*dtpb.TransferRun)
		cfg = proto.Clone(rec.Config).(*dtpb.TransferConfig)
	})
	if run == nil {
		return
	}
	project := configRE.FindStringSubmatch(cfg.GetName())[1]
	var msgs []string
	var err error
	if s.exec == nil {
		err = apierror.FailedPrecondition("BigQuery is not running on this instance")
	} else {
		switch run.GetDataSourceId() {
		case ScheduledQuery:
			msgs, err = s.runQuery(ctx, project, cfg, run)
		case CrossRegionCopy:
			msgs, err = s.runCopy(ctx, project, run)
		default:
			err = apierror.Unimplemented("data source %s is not served", run.GetDataSourceId())
		}
	}
	var final *dtpb.TransferRun
	s.update(name, func(rec *record, rr *runRecord) {
		for _, m := range msgs {
			s.logf(rr, dtpb.TransferMessage_INFO, "%s", m)
		}
		rr.Run.EndTime = timestamppb.New(s.Now())
		if err != nil {
			rr.Run.State = dtpb.TransferState_FAILED
			code := codes.Unknown
			if e := apierror.From(err); e != nil {
				code = e.Code
			}
			rr.Run.ErrorStatus = &rpcstatus.Status{Code: int32(code), Message: err.Error()}
			s.logf(rr, dtpb.TransferMessage_ERROR, "%s", err.Error())
			rec.Config.Error = rr.Run.ErrorStatus
		} else {
			rr.Run.State = dtpb.TransferState_SUCCEEDED
			s.logf(rr, dtpb.TransferMessage_INFO, "Transfer run completed successfully.")
			rec.Config.Error = nil
		}
		final = proto.Clone(rr.Run).(*dtpb.TransferRun)
	})
	if final != nil {
		s.notifyFinished(ctx, final)
	}
}

func (s *Server) runQuery(ctx context.Context, project string, cfg *dtpb.TransferConfig, run *dtpb.TransferRun) ([]string, error) {
	p := run.GetParams()
	rt := run.GetRunTime().AsTime().UTC()
	job := QueryJob{Project: project, Location: cfg.GetDatasetRegion(), Query: param(p, "query"), RunTime: rt,
		WriteDisposition: param(p, "write_disposition"), PartitioningField: param(p, "partitioning_field")}
	if job.Location == "" {
		job.Location = configRE.FindStringSubmatch(cfg.GetName())[2]
	}
	if d := run.GetDestinationDatasetId(); d != "" {
		table, err := RenderTableTemplate(param(p, "destination_table_name_template"), rt)
		if err != nil {
			return nil, apierror.InvalidArgument("%v", err)
		}
		job.DestDataset, job.DestTable = d, table
		if job.WriteDisposition == "" {
			job.WriteDisposition = "WRITE_APPEND"
		}
	}
	msgs := []string{fmt.Sprintf("Query parameters: @run_time=%s, @run_date=%s", rt.Format(time.RFC3339), rt.Format("2006-01-02"))}
	id, rows, err := s.exec.RunQuery(ctx, job)
	if err != nil {
		return msgs, err
	}
	if job.DestTable != "" {
		msgs = append(msgs, fmt.Sprintf("Job %s:%s (table %s) completed successfully. Number of rows written: %d.", project, id, job.DestTable, rows))
	} else {
		msgs = append(msgs, fmt.Sprintf("Job %s:%s completed successfully.", project, id))
	}
	return msgs, nil
}

func (s *Server) runCopy(ctx context.Context, project string, run *dtpb.TransferRun) ([]string, error) {
	p := run.GetParams()
	job := CopyJob{Project: project, DestDataset: run.GetDestinationDatasetId(),
		SourceProject: param(p, "source_project_id"), SourceDataset: param(p, "source_dataset_id"),
		Overwrite: param(p, "overwrite_destination_table") == "true"}
	copied, err := s.exec.CopyTables(ctx, job)
	msgs := []string{fmt.Sprintf("Copying dataset %s:%s to %s:%s", job.SourceProject, job.SourceDataset, project, job.DestDataset)}
	for _, t := range copied {
		msgs = append(msgs, fmt.Sprintf("Table %s successfully copied.", t))
	}
	if err != nil {
		return msgs, err
	}
	msgs = append(msgs, fmt.Sprintf("%d tables copied.", len(copied)))
	return msgs, nil
}

// notifyFinished publishes the finished run to its Pub/Sub topic, as
// Google's transfer run notifications do (the TransferRun in JSON, with
// eventType TRANSFER_RUN_FINISHED and payloadFormat JSON_API_V1), and
// records a failure email.
func (s *Server) notifyFinished(ctx context.Context, run *dtpb.TransferRun) {
	if topic := run.GetNotificationPubsubTopic(); topic != "" {
		var err error
		if s.pub == nil {
			err = errors.New("Pub/Sub is not enabled on this instance")
		} else {
			body, _ := protojson.Marshal(run)
			err = s.pub.Publish(ctx, topic, body, map[string]string{"eventType": "TRANSFER_RUN_FINISHED", "payloadFormat": "JSON_API_V1"})
		}
		if err != nil {
			s.update(run.GetName(), func(_ *record, rr *runRecord) {
				s.logf(rr, dtpb.TransferMessage_WARNING, "Failed to publish the run notification to %s: %v", topic, err)
			})
		}
	}
	if run.GetState() == dtpb.TransferState_FAILED && run.GetEmailPreferences().GetEnableFailureEmail() {
		cfg := run.GetName()[:strings.Index(run.GetName(), "/runs/")]
		s.mu.Lock()
		s.emails = append(s.emails, Email{Time: s.Now(), Config: cfg, Run: run.GetName(),
			Subject: "Transfer run failed: " + run.GetErrorStatus().GetMessage()})
		s.mu.Unlock()
	}
}
