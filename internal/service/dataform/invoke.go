package dataform

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/genproto/googleapis/type/interval"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ---- Workflow configs ----

func checkInvocationConfig(c *dfpb.InvocationConfig) error {
	if c == nil {
		return nil
	}
	if c.GetEndUserAuthConfig() != nil {
		return apierror.Unimplemented("invocation_config.end_user_auth_config is not served by CloudBurrow")
	}
	if c.GetExecutionMode() != dfpb.InvocationConfig_EXECUTION_MODE_UNSPECIFIED {
		return apierror.Unimplemented("invocation_config.execution_mode is not served by CloudBurrow")
	}
	if n := c.ConcurrentActionsLimit; n != nil && *n < 0 {
		return apierror.InvalidArgument("invocation_config.concurrent_actions_limit must not be negative")
	}
	return nil
}

func (s *Server) CreateWorkflowConfig(_ context.Context, req *dfpb.CreateWorkflowConfigRequest) (*dfpb.WorkflowConfig, error) {
	if err := checkID("workflow_config_id", req.GetWorkflowConfigId()); err != nil {
		return nil, err
	}
	w := clone(req.GetWorkflowConfig())
	if w == nil || w.GetReleaseConfig() == "" {
		return nil, apierror.InvalidArgument("workflow_config.release_config is required")
	}
	if err := checkCron(w.GetCronSchedule(), w.GetTimeZone()); err != nil {
		return nil, err
	}
	if err := checkInvocationConfig(w.GetInvocationConfig()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(w.GetReleaseConfig(), req.GetParent()+"/releaseConfigs/") {
		return nil, apierror.InvalidArgument("release_config %q must be a release config of %s", w.GetReleaseConfig(), req.GetParent())
	}
	if _, err := s.release(w.GetReleaseConfig()); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/workflowConfigs/" + req.GetWorkflowConfigId()
	if _, ok := s.workflows[name]; ok {
		return nil, apierror.AlreadyExists("WorkflowConfig %s already exists", name)
	}
	w.Name = name
	w.CreateTime, w.UpdateTime = s.now(), s.now()
	w.RecentScheduledExecutionRecords, w.InternalMetadata = nil, nil
	s.workflows[name] = w
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(w), nil
}

func (s *Server) workflow(name string) (*dfpb.WorkflowConfig, error) {
	if !workflowRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/workflowConfigs/*", name)
	}
	w, ok := s.workflows[name]
	if !ok {
		return nil, apierror.NotFound("WorkflowConfig %s not found", name)
	}
	return w, nil
}

func (s *Server) GetWorkflowConfig(_ context.Context, req *dfpb.GetWorkflowConfigRequest) (*dfpb.WorkflowConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, err := s.workflow(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(w), nil
}

func (s *Server) ListWorkflowConfigs(_ context.Context, req *dfpb.ListWorkflowConfigsRequest) (*dfpb.ListWorkflowConfigsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		return nil, err
	}
	keys := childrenOf(s.workflows, req.GetParent(), "workflowConfigs")
	start, end, next, err := page("wc:"+req.GetParent(), req.GetPageToken(), req.GetPageSize(), len(keys))
	if err != nil {
		return nil, err
	}
	out := &dfpb.ListWorkflowConfigsResponse{NextPageToken: next}
	for _, k := range keys[start:end] {
		out.WorkflowConfigs = append(out.WorkflowConfigs, clone(s.workflows[k]))
	}
	return out, nil
}

func (s *Server) UpdateWorkflowConfig(_ context.Context, req *dfpb.UpdateWorkflowConfigRequest) (*dfpb.WorkflowConfig, error) {
	in := req.GetWorkflowConfig()
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.workflow(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(cur)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"release_config", "invocation_config", "cron_schedule", "time_zone", "disabled"}
	}
	for _, p := range paths {
		switch p {
		case "release_config":
			next.ReleaseConfig = in.GetReleaseConfig()
		case "invocation_config":
			next.InvocationConfig = clone(in.GetInvocationConfig())
		case "cron_schedule":
			next.CronSchedule = in.GetCronSchedule()
		case "time_zone":
			next.TimeZone = in.GetTimeZone()
		case "disabled":
			next.Disabled = in.GetDisabled()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of WorkflowConfig that can be updated", p)
		}
	}
	repo := workflowRE.FindStringSubmatch(next.Name)[1]
	if !strings.HasPrefix(next.GetReleaseConfig(), repo+"/releaseConfigs/") {
		return nil, apierror.InvalidArgument("release_config %q must be a release config of %s", next.GetReleaseConfig(), repo)
	}
	if _, err := s.release(next.GetReleaseConfig()); err != nil {
		return nil, err
	}
	if err := checkCron(next.GetCronSchedule(), next.GetTimeZone()); err != nil {
		return nil, err
	}
	if err := checkInvocationConfig(next.GetInvocationConfig()); err != nil {
		return nil, err
	}
	next.UpdateTime = s.now()
	s.workflows[next.Name] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteWorkflowConfig(_ context.Context, req *dfpb.DeleteWorkflowConfigRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.workflow(req.GetName()); err != nil {
		return nil, err
	}
	delete(s.workflows, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- Workflow invocations ----

func (s *Server) CreateWorkflowInvocation(ctx context.Context, req *dfpb.CreateWorkflowInvocationRequest) (*dfpb.WorkflowInvocation, error) {
	in := clone(req.GetWorkflowInvocation())
	if in == nil {
		return nil, apierror.InvalidArgument("workflow_invocation is required")
	}
	if in.GetPipelineConfig() != nil {
		return nil, apierror.Unimplemented("pipeline_config is not served by CloudBurrow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		return nil, err
	}
	cfg := in.GetInvocationConfig()
	var compName string
	switch src := in.GetCompilationSource().(type) {
	case *dfpb.WorkflowInvocation_CompilationResult:
		compName = src.CompilationResult
	case *dfpb.WorkflowInvocation_WorkflowConfig:
		w, err := s.workflow(src.WorkflowConfig)
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			return nil, apierror.InvalidArgument("invocation_config must not be set when invoking a workflow config, which has its own")
		}
		cfg = clone(w.GetInvocationConfig())
		rc, err := s.release(w.GetReleaseConfig())
		if err != nil {
			return nil, err
		}
		compName = rc.GetReleaseCompilationResult()
		if compName == "" {
			return nil, apierror.FailedPrecondition("ReleaseConfig %s has no release_compilation_result yet", rc.GetName())
		}
	default:
		return nil, apierror.InvalidArgument("workflow_invocation needs compilation_result or workflow_config")
	}
	if err := checkInvocationConfig(cfg); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(compName, req.GetParent()+"/compilationResults/") {
		return nil, apierror.InvalidArgument("compilation result %q must be one of %s", compName, req.GetParent())
	}
	comp, err := s.compilationOf(compName)
	if err != nil {
		return nil, err
	}
	if len(comp.C.GetCompilationErrors()) > 0 {
		return nil, apierror.InvalidArgument("CompilationResult %s has compilation errors and cannot be invoked", compName)
	}
	selected, err := selectActions(comp.Actions, cfg)
	if err != nil {
		return nil, err
	}
	now := s.now()
	in.Name = req.GetParent() + "/workflowInvocations/" + newUUID()
	in.InvocationConfig = cfg
	in.ResolvedCompilationResult = compName
	in.State = dfpb.WorkflowInvocation_RUNNING
	in.InvocationTiming = &interval.Interval{StartTime: now}
	in.DataEncryptionState, in.InternalMetadata = nil, nil
	inv := &invocation{I: in}
	for _, a := range selected {
		wa := &dfpb.WorkflowInvocationAction{Target: clone(a.GetTarget()), CanonicalTarget: clone(a.GetCanonicalTarget()), State: dfpb.WorkflowInvocationAction_PENDING}
		if disabled(a) {
			wa.State = dfpb.WorkflowInvocationAction_DISABLED
		}
		if a.GetNotebook() != nil || a.GetDataPreparation() != nil {
			wa.State = dfpb.WorkflowInvocationAction_FAILED
			wa.FailureReason = "notebooks and data preparations are not run by CloudBurrow's Dataform"
		}
		inv.Actions = append(inv.Actions, wa)
	}
	s.invocations[in.Name] = inv
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancels[in.Name] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.run(runCtx, in.Name, comp, selected, cfg)
	}()
	return clone(in), nil
}

func disabled(a *dfpb.CompilationResultAction) bool {
	return a.GetRelation().GetDisabled() || a.GetOperations().GetDisabled() || a.GetAssertion().GetDisabled() ||
		a.GetNotebook().GetDisabled() || a.GetDataPreparation().GetDisabled()
}

func deps(a *dfpb.CompilationResultAction) []*dfpb.Target {
	switch {
	case a.GetRelation() != nil:
		return a.GetRelation().GetDependencyTargets()
	case a.GetOperations() != nil:
		return a.GetOperations().GetDependencyTargets()
	case a.GetAssertion() != nil:
		return a.GetAssertion().GetDependencyTargets()
	case a.GetNotebook() != nil:
		return a.GetNotebook().GetDependencyTargets()
	case a.GetDataPreparation() != nil:
		return a.GetDataPreparation().GetDependencyTargets()
	}
	return nil
}

func tags(a *dfpb.CompilationResultAction) []string {
	switch {
	case a.GetRelation() != nil:
		return a.GetRelation().GetTags()
	case a.GetOperations() != nil:
		return a.GetOperations().GetTags()
	case a.GetAssertion() != nil:
		return a.GetAssertion().GetTags()
	}
	return nil
}

// selectActions picks the actions an invocation config names (every
// non-declaration action when it names none), with their transitive
// dependencies or dependents as asked, in dependency order.
func selectActions(all []*dfpb.CompilationResultAction, cfg *dfpb.InvocationConfig) ([]*dfpb.CompilationResultAction, error) {
	byKey := map[string]*dfpb.CompilationResultAction{}
	var keys []string
	for _, a := range all {
		if a.GetDeclaration() != nil {
			continue
		}
		k := targetKey(a.GetTarget())
		byKey[k] = a
		keys = append(keys, k)
	}
	chosen := map[string]bool{}
	if len(cfg.GetIncludedTargets()) == 0 && len(cfg.GetIncludedTags()) == 0 {
		for _, k := range keys {
			chosen[k] = true
		}
	} else {
		for _, t := range cfg.GetIncludedTargets() {
			k := targetKey(t)
			if _, ok := byKey[k]; !ok {
				return nil, apierror.InvalidArgument("included target %s is not an action of the compilation result", k)
			}
			chosen[k] = true
		}
		for _, tag := range cfg.GetIncludedTags() {
			for _, k := range keys {
				for _, t := range tags(byKey[k]) {
					if t == tag {
						chosen[k] = true
					}
				}
			}
		}
		if cfg.GetTransitiveDependenciesIncluded() {
			var add func(k string)
			add = func(k string) {
				for _, d := range deps(byKey[k]) {
					dk := targetKey(d)
					if _, ok := byKey[dk]; ok && !chosen[dk] {
						chosen[dk] = true
						add(dk)
					}
				}
			}
			for k := range copyKeys(chosen) {
				add(k)
			}
		}
		if cfg.GetTransitiveDependentsIncluded() {
			for changed := true; changed; {
				changed = false
				for _, k := range keys {
					if chosen[k] {
						continue
					}
					for _, d := range deps(byKey[k]) {
						if chosen[targetKey(d)] {
							chosen[k] = true
							changed = true
							break
						}
					}
				}
			}
		}
	}
	// Dependency order: an action after every chosen action it depends on.
	var order []*dfpb.CompilationResultAction
	state := map[string]int{}
	var visit func(k string) error
	visit = func(k string) error {
		switch state[k] {
		case 1:
			return apierror.InvalidArgument("the compilation result's actions have a dependency cycle through %s", k)
		case 2:
			return nil
		}
		state[k] = 1
		for _, d := range deps(byKey[k]) {
			dk := targetKey(d)
			if chosen[dk] {
				if err := visit(dk); err != nil {
					return err
				}
			}
		}
		state[k] = 2
		order = append(order, byKey[k])
		return nil
	}
	sort.Strings(keys)
	for _, k := range keys {
		if chosen[k] {
			if err := visit(k); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

func copyKeys(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func endTiming(i *interval.Interval, t *timestamppb.Timestamp) {
	if i != nil && i.EndTime == nil {
		i.EndTime = t
	}
}

// run executes an invocation's actions in order. An action whose
// dependency did not succeed is SKIPPED; a disabled one stays DISABLED and
// counts as done for its dependents, as Dataform runs them.
func (s *Server) run(ctx context.Context, name string, comp *compilation, actions []*dfpb.CompilationResultAction, cfg *dfpb.InvocationConfig) {
	location := comp.C.GetCodeCompilationConfig().GetDefaultLocation()
	outcome := map[string]dfpb.WorkflowInvocationAction_State{}
	failed := false
	for i, a := range actions {
		key := targetKey(a.GetTarget())
		s.mu.Lock()
		inv, ok := s.invocations[name]
		if !ok {
			s.mu.Unlock()
			return
		}
		wa := inv.Actions[i]
		if ctx.Err() != nil {
			if wa.State == dfpb.WorkflowInvocationAction_PENDING {
				wa.State = dfpb.WorkflowInvocationAction_CANCELLED
			}
			outcome[key] = wa.State
			s.mu.Unlock()
			continue
		}
		if wa.State != dfpb.WorkflowInvocationAction_PENDING {
			outcome[key] = wa.State
			if wa.State == dfpb.WorkflowInvocationAction_FAILED {
				failed = true
			}
			s.mu.Unlock()
			continue
		}
		skip := false
		for _, d := range deps(a) {
			st, ok := outcome[targetKey(d)]
			if ok && st != dfpb.WorkflowInvocationAction_SUCCEEDED && st != dfpb.WorkflowInvocationAction_DISABLED {
				skip = true
			}
		}
		if skip {
			wa.State = dfpb.WorkflowInvocationAction_SKIPPED
			outcome[key] = wa.State
			s.mu.Unlock()
			continue
		}
		wa.State = dfpb.WorkflowInvocationAction_RUNNING
		wa.InvocationTiming = &interval.Interval{StartTime: s.now()}
		_ = s.save()
		s.mu.Unlock()

		script, jobID, err := s.execute(ctx, a, location, cfg)

		s.mu.Lock()
		wa.Action = &dfpb.WorkflowInvocationAction_BigqueryAction{BigqueryAction: &dfpb.WorkflowInvocationAction_BigQueryAction{SqlScript: script, JobId: jobID}}
		endTiming(wa.InvocationTiming, s.now())
		switch {
		case ctx.Err() != nil:
			wa.State = dfpb.WorkflowInvocationAction_CANCELLED
		case err != nil:
			wa.State = dfpb.WorkflowInvocationAction_FAILED
			wa.FailureReason = err.Error()
			failed = true
		default:
			wa.State = dfpb.WorkflowInvocationAction_SUCCEEDED
		}
		outcome[key] = wa.State
		_ = s.save()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, name)
	inv, ok := s.invocations[name]
	if !ok {
		return
	}
	switch {
	case ctx.Err() != nil || inv.I.State == dfpb.WorkflowInvocation_CANCELING:
		inv.I.State = dfpb.WorkflowInvocation_CANCELLED
	case failed:
		inv.I.State = dfpb.WorkflowInvocation_FAILED
	default:
		inv.I.State = dfpb.WorkflowInvocation_SUCCEEDED
	}
	endTiming(inv.I.InvocationTiming, s.now())
	_ = s.save()
}

func bq(t *dfpb.Target) string {
	return "`" + t.GetDatabase() + "." + t.GetSchema() + "." + t.GetName() + "`"
}

// execute runs one action on BigQuery, and returns the SQL it ran and the
// last job's ID. The SQL is what Dataform's BigQuery adapter
// (@dataform/core's execution SQL) generates for the action.
func (s *Server) execute(ctx context.Context, a *dfpb.CompilationResultAction, location string, cfg *dfpb.InvocationConfig) (string, string, error) {
	w := s.opts.Warehouse
	if w == nil {
		return "", "", fmt.Errorf("BigQuery is not served on this instance")
	}
	t := a.GetTarget()
	project := t.GetDatabase()
	var statements []string
	var jobID string
	exec := func(sql string) error {
		statements = append(statements, strings.TrimSpace(sql))
		id, err := w.Exec(ctx, project, location, sql)
		if id != "" {
			jobID = id
		}
		return err
	}
	script := func() string { return strings.Join(statements, ";\n") }
	ensure := func() error {
		if err := w.EnsureDataset(ctx, project, t.GetSchema(), location); err != nil {
			return fmt.Errorf("create dataset %s.%s: %w", project, t.GetSchema(), err)
		}
		return nil
	}
	switch {
	case a.GetRelation() != nil:
		r := a.GetRelation()
		if err := ensure(); err != nil {
			return script(), jobID, err
		}
		exists, kind, columns, err := w.Table(ctx, project, t.GetSchema(), t.GetName())
		if err != nil {
			return script(), jobID, err
		}
		want := "TABLE"
		switch r.GetRelationType() {
		case dfpb.CompilationResultAction_Relation_VIEW:
			want = "VIEW"
		case dfpb.CompilationResultAction_Relation_MATERIALIZED_VIEW:
			want = "MATERIALIZED_VIEW"
		}
		inc := r.GetRelationType() == dfpb.CompilationResultAction_Relation_INCREMENTAL_TABLE
		ic := r.GetIncrementalTableConfig()
		incremental := inc && exists && kind == "TABLE" && (!cfg.GetFullyRefreshIncrementalTablesEnabled() || ic.GetRefreshDisabled())
		pre, post := r.GetPreOperations(), r.GetPostOperations()
		if incremental {
			pre, post = ic.GetIncrementalPreOperations(), ic.GetIncrementalPostOperations()
		}
		for _, q := range pre {
			if err := exec(q); err != nil {
				return script(), jobID, err
			}
		}
		if exists && kind != want {
			drop := map[string]string{"TABLE": "table", "VIEW": "view", "MATERIALIZED_VIEW": "materialized view"}[kind]
			if drop == "" {
				drop = "table"
			}
			if err := exec("drop " + drop + " if exists " + bq(t)); err != nil {
				return script(), jobID, err
			}
		}
		var stmt string
		switch {
		case incremental && len(ic.GetUniqueKeyParts()) > 0:
			stmt = mergeInto(t, columns, incrementalQuery(r), ic.GetUniqueKeyParts(), ic.GetUpdatePartitionFilter())
		case incremental:
			stmt = insertInto(t, columns, incrementalQuery(r))
		case want == "VIEW":
			stmt = "create or replace view " + bq(t) + " as " + r.GetSelectQuery()
		case want == "MATERIALIZED_VIEW":
			stmt = "create materialized view if not exists " + bq(t) + " " + tableOptions(r) + "as " + r.GetSelectQuery()
		default:
			stmt = "create or replace table " + bq(t) + " " + tableOptions(r) + "as " + r.GetSelectQuery()
		}
		if err := exec(stmt); err != nil {
			return script(), jobID, err
		}
		for _, q := range post {
			if err := exec(q); err != nil {
				return script(), jobID, err
			}
		}
		return script(), jobID, nil
	case a.GetOperations() != nil:
		o := a.GetOperations()
		if o.GetHasOutput() {
			if err := ensure(); err != nil {
				return script(), jobID, err
			}
		}
		for _, q := range o.GetQueries() {
			if strings.TrimSpace(strings.TrimRight(strings.TrimSpace(q), ";")) == "" {
				continue
			}
			if err := exec(q); err != nil {
				return script(), jobID, err
			}
		}
		return script(), jobID, nil
	case a.GetAssertion() != nil:
		if err := ensure(); err != nil {
			return script(), jobID, err
		}
		if err := exec("create or replace view " + bq(t) + " as " + a.GetAssertion().GetSelectQuery()); err != nil {
			return script(), jobID, err
		}
		check := "select sum(1) as row_count from " + bq(t)
		statements = append(statements, check)
		id, n, err := w.Count(ctx, project, location, check)
		if id != "" {
			jobID = id
		}
		if err != nil {
			return script(), jobID, err
		}
		if n > 0 {
			return script(), jobID, fmt.Errorf("Assertion failed: query returned %d row(s).", n)
		}
		return script(), jobID, nil
	}
	return "", "", fmt.Errorf("action %s has nothing CloudBurrow can run", targetKey(t))
}

func incrementalQuery(r *dfpb.CompilationResultAction_Relation) string {
	if q := r.GetIncrementalTableConfig().GetIncrementalSelectQuery(); q != "" {
		return q
	}
	return r.GetSelectQuery()
}

func tableOptions(r *dfpb.CompilationResultAction_Relation) string {
	var b strings.Builder
	if p := r.GetPartitionExpression(); p != "" {
		b.WriteString("partition by " + p + " ")
	}
	if c := r.GetClusterExpressions(); len(c) > 0 {
		b.WriteString("cluster by " + strings.Join(c, ", ") + " ")
	}
	var opts []string
	if r.GetPartitionExpression() != "" && r.GetPartitionExpirationDays() > 0 {
		opts = append(opts, fmt.Sprintf("partition_expiration_days=%d", r.GetPartitionExpirationDays()))
	}
	if r.GetRequirePartitionFilter() {
		opts = append(opts, "require_partition_filter=true")
	}
	keys := make([]string, 0, len(r.GetAdditionalOptions()))
	for k := range r.GetAdditionalOptions() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		opts = append(opts, k+"="+r.GetAdditionalOptions()[k])
	}
	if len(opts) > 0 {
		b.WriteString("OPTIONS(" + strings.Join(opts, ",") + ") ")
	}
	return b.String()
}

func backticked(columns []string) []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = "`" + c + "`"
	}
	return out
}

func insertInto(t *dfpb.Target, columns []string, query string) string {
	cols := strings.Join(backticked(columns), ",")
	return "\ninsert into " + bq(t) + "\n(" + cols + ")\nselect " + cols + "\nfrom (" + query + "\n) as insertions"
}

func mergeInto(t *dfpb.Target, columns []string, query string, uniqueKey []string, updatePartitionFilter string) string {
	var on []string
	for _, k := range uniqueKey {
		on = append(on, "T."+k+" = S."+k)
	}
	var set []string
	for _, c := range columns {
		set = append(set, "`"+c+"` = S."+c)
	}
	filter := ""
	if updatePartitionFilter != "" {
		filter = "and T." + updatePartitionFilter
	}
	cols := strings.Join(backticked(columns), ",")
	return "\nmerge " + bq(t) + " T\nusing (" + query + "\n) S\non " + strings.Join(on, " and ") + "\n  " + filter +
		"\nwhen matched then\n  update set " + strings.Join(set, ",") + "\nwhen not matched then\n  insert (" + cols + ") values (" + cols + ")"
}

func (s *Server) invocationOf(name string) (*invocation, error) {
	if !invocationRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/workflowInvocations/*", name)
	}
	i, ok := s.invocations[name]
	if !ok {
		return nil, apierror.NotFound("WorkflowInvocation %s not found", name)
	}
	return i, nil
}

func (s *Server) GetWorkflowInvocation(_ context.Context, req *dfpb.GetWorkflowInvocationRequest) (*dfpb.WorkflowInvocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.invocationOf(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(i.I), nil
}

func (s *Server) ListWorkflowInvocations(_ context.Context, req *dfpb.ListWorkflowInvocationsRequest) (*dfpb.ListWorkflowInvocationsResponse, error) {
	s.mu.Lock()
	if _, err := s.repo(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*dfpb.WorkflowInvocation
	for _, k := range childrenOf(s.invocations, req.GetParent(), "workflowInvocations") {
		all = append(all, clone(s.invocations[k].I))
	}
	s.mu.Unlock()
	all, err := filterBy(all, req.GetFilter(), func(i *dfpb.WorkflowInvocation) map[string]string {
		return map[string]string{"name": i.GetName(), "compilation_result": i.GetCompilationResult(), "workflow_config": i.GetWorkflowConfig(), "state": i.GetState().String()}
	})
	if err != nil {
		return nil, err
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "invocation_timing.start_time desc"
	}
	if err := orderBy(all, order, func(i *dfpb.WorkflowInvocation) map[string]string {
		return map[string]string{"name": i.GetName(), "invocation_timing.start_time": i.GetInvocationTiming().GetStartTime().AsTime().Format(time.RFC3339Nano)}
	}); err != nil {
		return nil, err
	}
	start, end, next, err := page("wi:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.ListWorkflowInvocationsResponse{WorkflowInvocations: all[start:end], NextPageToken: next}, nil
}

func (s *Server) DeleteWorkflowInvocation(_ context.Context, req *dfpb.DeleteWorkflowInvocationRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.invocationOf(req.GetName()); err != nil {
		return nil, err
	}
	if c, ok := s.cancels[req.GetName()]; ok {
		c()
		delete(s.cancels, req.GetName())
	}
	delete(s.invocations, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) CancelWorkflowInvocation(_ context.Context, req *dfpb.CancelWorkflowInvocationRequest) (*dfpb.CancelWorkflowInvocationResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.invocationOf(req.GetName())
	if err != nil {
		return nil, err
	}
	if i.I.State != dfpb.WorkflowInvocation_RUNNING {
		return nil, apierror.FailedPrecondition("WorkflowInvocation %s is %s, not RUNNING", req.GetName(), i.I.State)
	}
	i.I.State = dfpb.WorkflowInvocation_CANCELING
	if c, ok := s.cancels[req.GetName()]; ok {
		c()
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &dfpb.CancelWorkflowInvocationResponse{}, nil
}

func (s *Server) QueryWorkflowInvocationActions(_ context.Context, req *dfpb.QueryWorkflowInvocationActionsRequest) (*dfpb.QueryWorkflowInvocationActionsResponse, error) {
	s.mu.Lock()
	i, err := s.invocationOf(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*dfpb.WorkflowInvocationAction
	for _, a := range i.Actions {
		all = append(all, clone(a))
	}
	s.mu.Unlock()
	start, end, next, err := page("wia:"+req.GetName(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryWorkflowInvocationActionsResponse{WorkflowInvocationActions: all[start:end], NextPageToken: next}, nil
}

// ---- filters and order ----

var termRE = regexp.MustCompile(`^\s*([a-z_.]+)\s*(=|!=|:)\s*"?([^"]*)"?\s*$`)

// filterBy applies an AIP-160 filter of field=value, field!=value and
// field:value terms joined by AND over the fields fieldsOf gives.
func filterBy[T any](xs []T, filter string, fieldsOf func(T) map[string]string) ([]T, error) {
	if strings.TrimSpace(filter) == "" {
		return xs, nil
	}
	type term struct{ field, op, value string }
	var terms []term
	for _, part := range regexp.MustCompile(`\s+AND\s+`).Split(filter, -1) {
		m := termRE.FindStringSubmatch(part)
		if m == nil {
			return nil, apierror.InvalidArgument("filter %q: want field=\"value\" terms joined by AND", filter)
		}
		terms = append(terms, term{m[1], m[2], m[3]})
	}
	var out []T
	for _, x := range xs {
		f := fieldsOf(x)
		ok := true
		for _, t := range terms {
			v, known := f[t.field]
			if !known {
				return nil, apierror.InvalidArgument("filter field %q is not supported", t.field)
			}
			switch t.op {
			case "=":
				ok = ok && v == t.value
			case "!=":
				ok = ok && v != t.value
			case ":":
				ok = ok && strings.Contains(v, t.value)
			}
		}
		if ok {
			out = append(out, x)
		}
	}
	return out, nil
}

func orderBy[T any](xs []T, order string, fieldsOf func(T) map[string]string) error {
	if strings.TrimSpace(order) == "" {
		return nil
	}
	f := strings.Fields(order)
	if len(f) > 2 || (len(f) == 2 && f[1] != "desc" && f[1] != "asc") {
		return apierror.InvalidArgument("order_by %q is not supported", order)
	}
	if len(xs) > 0 {
		if _, ok := fieldsOf(xs[0])[f[0]]; !ok {
			return apierror.InvalidArgument("order_by field %q is not supported", f[0])
		}
	}
	desc := len(f) == 2 && f[1] == "desc"
	sort.SliceStable(xs, func(i, j int) bool { return less(fieldsOf(xs[i])[f[0]], fieldsOf(xs[j])[f[0]], desc) })
	return nil
}

// callerOf is the caller's principal, from the instance's identity header
// when the IAM layer sets one.
func callerOf(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for _, k := range []string{"x-cloudburrow-principal", "x-goog-authenticated-user-email"} {
			if v := md.Get(k); len(v) > 0 && v[0] != "" {
				return v[0]
			}
		}
	}
	return "user:developer@cloudburrow.local"
}
