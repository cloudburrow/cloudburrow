package dataform

// v1beta1 workflow configs' table update triggers. RunSchedules evaluates
// each trigger config on its tick: a table's last modification time, read
// from BigQuery, newer than the one its trigger last ran for is a met
// trigger; the condition (ANY unless ALL) decides whether the workflow
// runs, max_wait_duration bounds how long a partly met ALL waits, and
// min_execution_duration (or, without it, a running invocation of the
// config) holds a run back. The first evaluation of a table only records
// its time, so a config fires on changes made after it was set.

import (
	"context"
	"encoding/json"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TableModifier is a Warehouse that reads a table's last modification
// time; triggers need it.
type TableModifier interface {
	// Modified is project.dataset.table's lastModifiedTime, and whether
	// the table exists.
	Modified(ctx context.Context, project, dataset, table string) (time.Time, bool, error)
}

// triggerState is a workflow config's trigger config and what its
// evaluations have seen.
type triggerState struct {
	Config *b1pb.WorkflowTriggerConfig `json:"-"`
	Raw    json.RawMessage             `json:"config"`
	// Seen is, by table, the modification time last consumed (a run, or
	// the first evaluation); a table absent when first evaluated is
	// recorded with the zero time, so its creation is a change.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Partial is when an ALL condition was first partly met.
	Partial time.Time `json:"partial,omitempty"`
	// LastRun is when the trigger last invoked the workflow.
	LastRun time.Time `json:"lastRun,omitempty"`
}

func (t *triggerState) marshal() {
	if t.Config != nil {
		t.Raw, _ = protojson.Marshal(t.Config)
	}
}

func (t *triggerState) unmarshal() error {
	t.Config = &b1pb.WorkflowTriggerConfig{}
	return protojson.Unmarshal(t.Raw, t.Config)
}

func triggerTable(repo string, t *b1pb.Target) (project, dataset, table string) {
	project = t.GetDatabase()
	if project == "" {
		if m := locationRE.FindStringSubmatch(repositoryRE.FindStringSubmatch(repo)[1]); m != nil {
			project = m[1]
		}
	}
	return project, t.GetSchema(), t.GetName()
}

// checkTriggerConfig validates a trigger config and keeps its input
// fields; nil clears the trigger.
func (s *Server) checkTriggerConfig(repo string, in *b1pb.WorkflowTriggerConfig) (*b1pb.WorkflowTriggerConfig, error) {
	if in == nil {
		return nil, nil
	}
	if repositoryRE.FindStringSubmatch(repo) == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/*/locations/*/repositories/*", repo)
	}
	if _, ok := b1pb.WorkflowTriggerConfig_Condition_name[int32(in.GetCondition())]; !ok {
		return nil, apierror.InvalidArgument("workflow_trigger_config.condition %v is not a Condition", in.GetCondition())
	}
	if len(in.GetWorkflowTriggers()) == 0 {
		return nil, apierror.InvalidArgument("workflow_trigger_config.workflow_triggers is required")
	}
	for _, d := range []struct {
		field string
		v     interface{ AsDuration() time.Duration }
		set   bool
	}{{"min_execution_duration", in.GetMinExecutionDuration(), in.GetMinExecutionDuration() != nil},
		{"max_wait_duration", in.GetMaxWaitDuration(), in.GetMaxWaitDuration() != nil}} {
		if d.set && d.v.AsDuration() < 0 {
			return nil, apierror.InvalidArgument("workflow_trigger_config.%s must not be negative", d.field)
		}
	}
	out := &b1pb.WorkflowTriggerConfig{Condition: in.GetCondition(), MinExecutionDuration: clone(in.GetMinExecutionDuration()), MaxWaitDuration: clone(in.GetMaxWaitDuration())}
	for i, t := range in.GetWorkflowTriggers() {
		u := t.GetTableUpdateTrigger()
		if u == nil || u.GetTable().GetSchema() == "" || u.GetTable().GetName() == "" {
			return nil, apierror.InvalidArgument("workflow_trigger_config.workflow_triggers[%d].table_update_trigger.table needs a schema and a name", i)
		}
		out.WorkflowTriggers = append(out.WorkflowTriggers, &b1pb.WorkflowTrigger{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{
			TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: clone(u.GetTable())}}})
	}
	return out, nil
}

// setTrigger stores (or, for nil, drops) a workflow config's trigger,
// keeping what was seen of the tables it still names.
func (s *Server) setTrigger(name string, tc *b1pb.WorkflowTriggerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workflows[name]; !ok {
		return apierror.NotFound("WorkflowConfig %s not found", name)
	}
	if tc == nil {
		delete(s.triggers, name)
		return apierror.Wrap(s.save())
	}
	repo := workflowRE.FindStringSubmatch(name)[1]
	old := s.triggers[name]
	st := &triggerState{Config: tc, Seen: map[string]time.Time{}}
	if old != nil {
		tc.RecentTriggerEvaluationRecords = old.Config.GetRecentTriggerEvaluationRecords()
		tc.LastSuccessfulEvaluationTime = old.Config.GetLastSuccessfulEvaluationTime()
		st.LastRun = old.LastRun
		prev := map[string]*timestamppb.Timestamp{}
		for _, t := range old.Config.GetWorkflowTriggers() {
			p, d, n := triggerTable(repo, t.GetTableUpdateTrigger().GetTable())
			prev[p+"."+d+"."+n] = t.GetTableUpdateTrigger().GetTriggerUpdateTime()
		}
		for _, t := range tc.GetWorkflowTriggers() {
			p, d, n := triggerTable(repo, t.GetTableUpdateTrigger().GetTable())
			k := p + "." + d + "." + n
			if seen, ok := old.Seen[k]; ok {
				st.Seen[k] = seen
			}
			t.GetTableUpdateTrigger().TriggerUpdateTime = prev[k]
		}
	}
	s.triggers[name] = st
	if err := s.save(); err != nil {
		return apierror.Wrap(err)
	}
	return nil
}

// evaluateTriggers evaluates every enabled workflow config's trigger.
func (s *Server) evaluateTriggers(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var names []string
	for _, k := range sortedKeys(s.triggers) {
		if w, ok := s.workflows[k]; ok && !w.GetDisabled() {
			names = append(names, k)
		}
	}
	s.mu.Unlock()
	for _, name := range names {
		s.evaluateTrigger(ctx, name, now)
	}
}

func (s *Server) evaluateTrigger(ctx context.Context, name string, now time.Time) {
	repo := workflowRE.FindStringSubmatch(name)[1]
	s.mu.Lock()
	st := s.triggers[name]
	if st == nil {
		s.mu.Unlock()
		return
	}
	cfg := clone(st.Config)
	running := false
	for _, inv := range s.invocations {
		if inv.I.GetWorkflowConfig() == name && (inv.I.GetState() == dfpb.WorkflowInvocation_RUNNING || inv.I.GetState() == dfpb.WorkflowInvocation_CANCELING) {
			running = true
		}
	}
	s.mu.Unlock()

	tm, _ := s.opts.Warehouse.(TableModifier)
	var evalErr error
	if tm == nil {
		evalErr = apierror.FailedPrecondition("BigQuery is not served on this instance, so table update triggers cannot be evaluated")
	}
	type change struct {
		key string
		mod time.Time
		i   int
	}
	var changed []change
	first := map[string]time.Time{}
	if evalErr == nil {
		evalErr = s.checkKey(ctx, repo)
	}
	if evalErr == nil {
		for i, t := range cfg.GetWorkflowTriggers() {
			p, d, n := triggerTable(repo, t.GetTableUpdateTrigger().GetTable())
			key := p + "." + d + "." + n
			mod, exists, err := tm.Modified(ctx, p, d, n)
			if err != nil {
				evalErr = apierror.FailedPrecondition("read table %s: %v", key, err)
				break
			}
			if !exists {
				mod = time.Time{}
			}
			seen, ok := st.Seen[key]
			if !ok {
				first[key] = mod
				continue
			}
			if exists && mod.After(seen) {
				changed = append(changed, change{key, mod, i})
			}
		}
	}
	fire := false
	partial := time.Time{}
	if evalErr == nil && len(changed) > 0 {
		all := cfg.GetCondition() == b1pb.WorkflowTriggerConfig_ALL
		if !all || len(changed) == len(cfg.GetWorkflowTriggers()) {
			fire = true
		} else {
			partial = st.Partial
			if partial.IsZero() {
				partial = now
			}
			if w := cfg.GetMaxWaitDuration(); w != nil && now.Sub(partial) >= w.AsDuration() {
				fire = true
			}
		}
		if fire {
			if d := cfg.GetMinExecutionDuration(); d != nil && d.AsDuration() > 0 {
				if !st.LastRun.IsZero() && now.Sub(st.LastRun) < d.AsDuration() {
					fire = false
				}
			} else if running {
				fire = false
			}
		}
	}
	var runErr error
	if fire {
		_, runErr = s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo,
			WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_WorkflowConfig{WorkflowConfig: name}}})
		if runErr != nil && evalErr == nil {
			evalErr = runErr
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.triggers[name]
	if cur != st {
		return // changed while evaluating
	}
	for k, v := range first {
		cur.Seen[k] = v
	}
	if fire && runErr == nil {
		for _, c := range changed {
			cur.Seen[c.key] = c.mod
			if t := cur.Config.GetWorkflowTriggers()[c.i].GetTableUpdateTrigger(); t != nil {
				t.TriggerUpdateTime = timestamppb.New(c.mod)
			}
		}
		cur.LastRun, cur.Partial = now, time.Time{}
	} else {
		cur.Partial = partial
	}
	rec := &b1pb.TriggerEvaluationRecord{EvaluationTime: timestamppb.New(now), Status: &status.Status{}}
	if evalErr != nil {
		rec.Status = apierror.From(evalErr).GRPCStatus().Proto()
	} else {
		cur.Config.LastSuccessfulEvaluationTime = timestamppb.New(now)
	}
	cur.Config.RecentTriggerEvaluationRecords = append([]*b1pb.TriggerEvaluationRecord{rec}, cur.Config.GetRecentTriggerEvaluationRecords()...)
	if len(cur.Config.RecentTriggerEvaluationRecords) > 10 {
		cur.Config.RecentTriggerEvaluationRecords = cur.Config.RecentTriggerEvaluationRecords[:10]
	}
	_ = s.save()
}
