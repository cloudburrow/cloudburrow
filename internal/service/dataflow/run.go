package dataflow

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var epoch = time.Unix(0, 0).UTC()

// startLocked starts r's run on the engine.
func (s *Server) startLocked(key string, r *jobRec) {
	j := r.job
	opts := pipelineOptions(j)
	sdk, ver := sdkOf(j)
	run := Run{Project: j.ProjectId, Location: j.Location, JobID: j.Id, Name: j.Name,
		Streaming:   j.Type == dataflowpb.JobType_JOB_TYPE_STREAMING,
		PipelineURL: optString(opts, "pipelineUrl", "pipeline_url"),
		Options:     opts, SDK: lowerSDK(sdk), SDKVersion: ver, SDKName: sdk, MaxWorkers: maxWorkers(j)}
	ctx, cancel := context.WithCancel(context.Background())
	drain := make(chan struct{})
	r.cancel, r.drain, r.drained, r.done = cancel, drain, false, make(chan struct{})
	run.Drain = drain
	done := r.done
	s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED, "Autoscaling is enabled for job "+j.Id+". The number of workers will be between 1 and "+fmt.Sprint(run.MaxWorkers)+".")
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		var out Outcome
		switch {
		case s.opts.Engine == nil:
			out = Outcome{State: dataflowpb.JobState_JOB_STATE_FAILED, Message: "Workflow failed. Causes: CloudBurrow runs Dataflow jobs on a local Apache Flink runner in Docker, which this instance does not have."}
		case run.PipelineURL == "":
			out = Outcome{State: dataflowpb.JobState_JOB_STATE_FAILED, Message: "Workflow failed. Causes: The job has no staged pipeline: environment.sdkPipelineOptions.options.pipelineUrl is not set. Submit the pipeline with an Apache Beam SDK's DataflowRunner."}
		default:
			out = s.opts.Engine.Run(ctx, run, &reporter{s: s, key: key})
		}
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur := s.jobs[key]; cur == r {
			s.finishLocked(r, out)
		}
	}()
}

// finishLocked ends r's job with out, and starts its replacement if it
// was being updated.
func (s *Server) finishLocked(r *jobRec, out Outcome) {
	j := r.job
	st := out.State
	switch {
	case j.RequestedState == dataflowpb.JobState_JOB_STATE_CANCELLED:
		st = dataflowpb.JobState_JOB_STATE_CANCELLED
	case r.replacement != "" && st != dataflowpb.JobState_JOB_STATE_FAILED:
		st = dataflowpb.JobState_JOB_STATE_UPDATED
	case j.RequestedState == dataflowpb.JobState_JOB_STATE_DRAINED && st == dataflowpb.JobState_JOB_STATE_DONE:
		st = dataflowpb.JobState_JOB_STATE_DRAINED
	}
	if st == dataflowpb.JobState_JOB_STATE_FAILED && out.Message != "" {
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR, out.Message)
	}
	if j.CurrentState == dataflowpb.JobState_JOB_STATE_RUNNING || j.CurrentState == dataflowpb.JobState_JOB_STATE_DRAINING ||
		j.CurrentState == dataflowpb.JobState_JOB_STATE_CANCELLING {
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED, "Stopping worker pool...")
		s.scaleLocked(r, 0)
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED, "Worker pool stopped.")
	}
	j.CurrentState, j.CurrentStateTime = st, timestamppb.New(s.now())
	r.cancel, r.drain, r.done = nil, nil, nil
	if r.replacement != "" {
		if next := s.jobs[r.replacement]; next != nil && next.job.CurrentState == dataflowpb.JobState_JOB_STATE_PENDING {
			if st == dataflowpb.JobState_JOB_STATE_UPDATED {
				s.startLocked(r.replacement, next)
			} else {
				next.job.CurrentState, next.job.CurrentStateTime = dataflowpb.JobState_JOB_STATE_FAILED, timestamppb.New(s.now())
				s.addMessageLocked(next, dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR, "Workflow failed. Causes: The job being replaced, "+j.Id+", failed before the update.")
			}
		}
		r.replacement = ""
	}
	s.save()
}

// scaleLocked records the worker pool's resize to n workers.
func (s *Server) scaleLocked(r *jobRec, n int) {
	cur := int64(0)
	if len(r.scaling) > 0 {
		cur = r.scaling[len(r.scaling)-1].TargetNumWorkers
	}
	if cur == int64(n) {
		return
	}
	text := fmt.Sprintf("Autoscaling: Resized worker pool from %d to %d.", cur, n)
	if n > int(cur) && cur == 0 {
		text = fmt.Sprintf("Autoscaling: Raised the number of workers to %d based on the rate of progress in the currently running stage(s).", n)
	}
	r.scaling = append(r.scaling, &dataflowpb.AutoscalingEvent{CurrentNumWorkers: cur, TargetNumWorkers: int64(n),
		EventType:   dataflowpb.AutoscalingEvent_TARGET_NUM_WORKERS_CHANGED,
		Description: &dataflowpb.StructuredMessage{MessageText: text},
		Time:        timestamppb.New(s.now()), WorkerPool: "Regular"})
	s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, text)
}

// reporter is a run's Reporter: its job's record.
type reporter struct {
	s   *Server
	key string
}

func (p *reporter) with(f func(r *jobRec)) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if r := p.s.jobs[p.key]; r != nil {
		f(r)
	}
}

func (p *reporter) Started(workers int) {
	p.with(func(r *jobRec) {
		s := p.s
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, fmt.Sprintf("Starting %d workers in %s...", workers, r.job.Location))
		s.scaleLocked(r, workers)
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_DETAILED, "All workers have finished the startup processes and began to receive work requests.")
		if r.job.CurrentState == dataflowpb.JobState_JOB_STATE_PENDING {
			r.job.CurrentState, r.job.CurrentStateTime = dataflowpb.JobState_JOB_STATE_RUNNING, timestamppb.New(s.now())
		}
		s.save()
	})
}

func (p *reporter) Message(imp dataflowpb.JobMessageImportance, text string) {
	p.with(func(r *jobRec) { p.s.addMessageLocked(r, imp, text) })
}

func (p *reporter) Metrics(m []Metric) {
	p.with(func(r *jobRec) { r.metrics = m })
}

func (p *reporter) Log(severity, text string) {
	if p.s.opts.Logs == nil {
		return
	}
	var project, loc, id, name string
	p.with(func(r *jobRec) { project, loc, id, name = r.job.ProjectId, r.job.Location, r.job.Id, r.job.Name })
	if id != "" {
		p.s.opts.Logs.Log(project, loc, id, name, severity, text)
	}
}

// Graph sets the job's pipeline description from the runner's view of the
// pipeline: its user transforms, their outputs and the stages they run in.
func (p *reporter) Graph(ts []Transform) {
	p.with(func(r *jobRec) { r.job.PipelineDescription = describe(ts) })
}
