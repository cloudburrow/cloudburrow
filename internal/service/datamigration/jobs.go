package datamigration

import (
	"context"
	"fmt"
	"strings"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// stepTimeout bounds one step of a job (a verification, a pause, a
// promotion) against the servers.
const stepTimeout = 10 * time.Minute

// jobLocked is the job named name; the caller holds mu.
func (s *Server) jobLocked(name string) (*dmspb.MigrationJob, error) {
	if jobRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/migrationJobs/{migration_job}", name)
	}
	j, ok := s.jobs[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return j, nil
}

// answer is j as it is answered, with its duration.
func (s *Server) answer(j *dmspb.MigrationJob) *dmspb.MigrationJob {
	out := clone(j)
	if st, ok := s.started[j.GetName()]; ok {
		end := s.now()
		if j.GetEndTime() != nil {
			end = j.GetEndTime().AsTime()
		}
		if d := end.Sub(st); d > 0 {
			out.Duration = durationpb.New(d.Truncate(time.Millisecond))
		}
	}
	return out
}

// nativeLogical is a job whose PostgreSQL migration uses native logical
// replication (postgresHomogeneousConfig.isNativeLogical, gcloud's
// --use-postgres-native): no pglogical on the source.
func nativeLogical(j *dmspb.MigrationJob) bool {
	return nestedBool(j, "postgres_homogeneous_config", "is_native_logical")
}

// checkJob validates a new or changed job against its profiles; the
// caller holds mu.
func (s *Server) checkJob(project, location string, j *dmspb.MigrationJob) error {
	switch j.GetType() {
	case dmspb.MigrationJob_ONE_TIME, dmspb.MigrationJob_CONTINUOUS:
	default:
		return apierror.InvalidArgument("migration_job.type is required: ONE_TIME or CONTINUOUS")
	}
	if j.GetSource() == "" {
		return apierror.InvalidArgument("migration_job.source is required")
	}
	if j.GetDestination() == "" {
		return apierror.InvalidArgument("migration_job.destination is required")
	}
	src, err := s.profileLocked(j.GetSource())
	if err != nil {
		return err
	}
	dst, err := s.profileLocked(j.GetDestination())
	if err != nil {
		return err
	}
	for _, n := range []string{j.GetSource(), j.GetDestination()} {
		if m := profileRE.FindStringSubmatch(n); m[1] != project || m[2] != location {
			return apierror.InvalidArgument("connection profile %s must be in projects/%s/locations/%s", n, project, location)
		}
	}
	if src.GetOracle() != nil {
		return unimplemented("a migration from Oracle", "an Oracle source migrates through a conversion workspace, which CloudBurrow does not run")
	}
	if !isSource(src) {
		return apierror.InvalidArgument("migration_job.source %s is not a MySQL or PostgreSQL connection profile", j.GetSource())
	}
	if dst.GetCloudsql() == nil && dst.GetAlloydb() == nil {
		return apierror.InvalidArgument("migration_job.destination %s is not a Cloud SQL or AlloyDB connection profile", j.GetDestination())
	}
	if dst.GetAlloydb() != nil && dst.GetAlloydb().GetSettings() == nil {
		return unimplemented("a migration to an existing AlloyDB cluster",
			"an existing destination is demoted first (migrationJobs.demoteDestination), which CloudBurrow does not serve; create the destination profile with settings (docs/gaps.md)")
	}
	if c := dst.GetCloudsql(); c != nil && c.GetSettings().GetSourceId() != j.GetSource() {
		return apierror.InvalidArgument("the destination %s was created as a replica of %s, not of the job's source %s",
			j.GetDestination(), c.GetSettings().GetSourceId(), j.GetSource())
	}
	if j.GetConversionWorkspace().GetName() != "" || j.GetFilter() != "" {
		return unimplemented("migration_job.conversion_workspace and filter", "conversion workspaces are not served (docs/gaps.md)")
	}
	if j.GetDumpPath() != "" || len(j.GetDumpFlags().GetDumpFlags()) > 0 {
		return unimplemented("migration_job.dump_path and dump_flags",
			"Database Migration Service's managed dump is the copy CloudBurrow runs; a manual dump and mysqldump flags are not (docs/gaps.md)")
	}
	if dt := str(j, "dump_type"); dt == "PHYSICAL" {
		return unimplemented("migration_job.dump_type PHYSICAL", "the copy is logical (docs/gaps.md)")
	}
	if j.GetCmekKeyName() != "" {
		// "Not supported for MySQL-to-MySQL, PostgreSQL-to-PostgreSQL, or
		// PostgreSQL-to-AlloyDB. Those use the key settings on the
		// connection profile instead."
		return apierror.InvalidArgument("migration_job.cmek_key_name is not supported for homogeneous migrations: set the key on the destination connection profile")
	}
	switch c := j.GetConnectivity().(type) {
	case *dmspb.MigrationJob_ReverseSshConnectivity:
		return unimplemented("migration_job.reverse_ssh_connectivity",
			"the destinations' servers reach a source directly, on the instance's network; no bastion VM tunnel is run (docs/gaps.md)")
	case *dmspb.MigrationJob_VpcPeeringConnectivity:
		vpc := c.VpcPeeringConnectivity.GetVpc()
		if vpc == "" {
			return apierror.InvalidArgument("migration_job.vpc_peering_connectivity.vpc is required")
		}
		if !strings.HasPrefix(vpc, "projects/") || !strings.Contains(vpc, "/global/networks/") {
			return apierror.InvalidArgument("migration_job.vpc_peering_connectivity.vpc %q must be projects/{project}/global/networks/{network}", vpc)
		}
		if s.opts.Network != nil && !s.opts.Network(vpc) {
			return apierror.NotFound("The VPC network %s was not found", vpc)
		}
	}
	if nativeLogical(j) && src.GetPostgresql() == nil {
		return apierror.InvalidArgument("migration_job.postgres_homogeneous_config is for a PostgreSQL source")
	}
	return nil
}

func (s *Server) CreateMigrationJob(_ context.Context, req *dmspb.CreateMigrationJobRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkID("migration_job", req.GetMigrationJobId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	j := clone(req.GetMigrationJob())
	if j == nil {
		return nil, apierror.InvalidArgument("migration_job is required")
	}
	name := req.GetParent() + "/migrationJobs/" + req.GetMigrationJobId()
	if _, ok := s.jobs[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	if err := s.checkJob(project, location, j); err != nil {
		return nil, err
	}
	for _, o := range s.jobs {
		if o.GetDestination() == j.GetDestination() {
			return nil, failedPrecondition("the destination %s is already the destination of the migration job %s", j.GetDestination(), o.GetName())
		}
	}
	src, dst := s.profiles[j.GetSource()], s.profiles[j.GetDestination()]
	now := ts(s.now())
	j.Name, j.CreateTime, j.UpdateTime = name, now, now
	j.Phase, j.Error, j.EndTime, j.Duration = dmspb.MigrationJob_PHASE_UNSPECIFIED, nil, nil, nil
	if j.State != dmspb.MigrationJob_DRAFT {
		j.State = dmspb.MigrationJob_NOT_STARTED
	}
	j.SourceDatabase = &dmspb.DatabaseType{Provider: src.GetProvider(), Engine: profileEngine(src)}
	j.DestinationDatabase = &dmspb.DatabaseType{Provider: dst.GetProvider(), Engine: profileEngine(dst)}
	if j.GetConnectivity() == nil {
		// UNVERIFIED: Google's console asks for one; a job without one
		// reaches the source as a static IP job does.
		// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/postgres/create-migration-job
		j.Connectivity = &dmspb.MigrationJob_StaticIpConnectivity{StaticIpConnectivity: &dmspb.StaticIpConnectivity{}}
	}
	s.jobs[name] = j
	return s.done(req.GetParent(), "create", name, req.GetRequestId(), s.answer(j))
}

func (s *Server) GetMigrationJob(_ context.Context, req *dmspb.GetMigrationJobRequest) (*dmspb.MigrationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return s.answer(j), nil
}

func (s *Server) ListMigrationJobs(_ context.Context, req *dmspb.ListMigrationJobsRequest) (*dmspb.ListMigrationJobsResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dmspb.MigrationJob
	for _, k := range sortedKeys(s.jobs, req.GetParent()+"/migrationJobs/") {
		all = append(all, s.answer(s.jobs[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("dms-jobs:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &dmspb.ListMigrationJobsResponse{MigrationJobs: out, NextPageToken: next}, nil
}

func (s *Server) UpdateMigrationJob(_ context.Context, req *dmspb.UpdateMigrationJobRequest) (*longrunningpb.Operation, error) {
	in := req.GetMigrationJob()
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, err := s.jobLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	next := clone(cur)
	editable := cur.State == dmspb.MigrationJob_NOT_STARTED || cur.State == dmspb.MigrationJob_DRAFT
	for _, p := range paths {
		switch p {
		case "display_name", "displayName":
			next.DisplayName = in.GetDisplayName()
		case "labels":
			next.Labels = in.GetLabels()
		case "performance_config", "performanceConfig":
			// "You can change the data dump parallelism with Edit, but must
			// restart the job afterward."
			next.PerformanceConfig = in.GetPerformanceConfig()
		case "type", "source", "destination", "static_ip_connectivity", "staticIpConnectivity",
			"vpc_peering_connectivity", "vpcPeeringConnectivity", "reverse_ssh_connectivity", "reverseSshConnectivity",
			"dump_path", "dumpPath", "dump_flags", "dumpFlags":
			if !editable {
				return nil, failedPrecondition("migration job %s is %s: %s can be changed before it starts", cur.GetName(), cur.State, p)
			}
			switch p {
			case "type":
				next.Type = in.GetType()
			case "source":
				next.Source = in.GetSource()
			case "destination":
				next.Destination = in.GetDestination()
			case "dump_path", "dumpPath":
				next.DumpPath = in.GetDumpPath()
			case "dump_flags", "dumpFlags":
				next.DumpFlags = in.GetDumpFlags()
			default:
				next.Connectivity = in.GetConnectivity()
			}
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated", p)
		}
	}
	m := jobRE.FindStringSubmatch(next.GetName())
	if err := s.checkJob(m[1], m[2], next); err != nil {
		return nil, err
	}
	next.UpdateTime = ts(s.now())
	s.jobs[next.GetName()] = next
	return s.done(parentOf(next.GetName()), "update", next.GetName(), req.GetRequestId(), s.answer(next))
}

// jobContext is what a job's steps need, read under mu.
type jobContext struct {
	name       string
	job        *dmspb.MigrationJob
	src        Source
	srcProfile *dmspb.ConnectionProfile
	dst        *dmspb.ConnectionProfile
	target     Target
}

func (s *Server) contextLocked(j *dmspb.MigrationJob) (jobContext, error) {
	src, ok1 := s.profiles[j.GetSource()]
	dst, ok2 := s.profiles[j.GetDestination()]
	if !ok1 || !ok2 {
		return jobContext{}, failedPrecondition("migration job %s's connection profiles no longer exist", j.GetName())
	}
	if dst.State != dmspb.ConnectionProfile_READY {
		return jobContext{}, failedPrecondition("the destination connection profile %s is %s, not READY", dst.GetName(), dst.State)
	}
	if s.opts.Backend == nil {
		return jobContext{}, failedPrecondition("this server runs no destinations")
	}
	t, _ := targetOf(dst)
	return jobContext{name: j.GetName(), job: clone(j), src: sourceOf(src), srcProfile: clone(src), dst: clone(dst), target: t}, nil
}

// transition moves a job from one of states to via, starts an operation,
// and runs step beside the request; step's error fails the operation and
// sets the job back (or to FAILED when failState is set).
func (s *Server) transition(name, verb string, allowed []dmspb.MigrationJob_State, via dmspb.MigrationJob_State, viaPhase dmspb.MigrationJob_Phase,
	step func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error)) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.jobLocked(name)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, a := range allowed {
		ok = ok || j.State == a
	}
	if !ok {
		return nil, failedPrecondition("migration job %s is %s: %s is not allowed in that state", name, j.State, verb)
	}
	jc, err := s.contextLocked(j)
	if err != nil {
		return nil, err
	}
	prevState, prevPhase := j.State, j.Phase
	j.State, j.UpdateTime = via, ts(s.now())
	if viaPhase != dmspb.MigrationJob_PHASE_UNSPECIFIED {
		j.Phase = viaPhase
	}
	op, err := s.startOp(parentOf(name), verb, name, "")
	if err != nil {
		j.State = prevState
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	opName := op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		state, phase, err := step(ctx, jc)
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.jobs[name]
		if cur == nil {
			s.finishOp(opName, nil, apierror.Aborted("migration job %s was deleted", name))
			_ = s.save()
			return
		}
		cur.UpdateTime = ts(s.now())
		if err != nil {
			if state == dmspb.MigrationJob_FAILED {
				cur.State, cur.Error = dmspb.MigrationJob_FAILED, statusOf(err)
			} else if cur.State == via {
				cur.State, cur.Phase = prevState, prevPhase
			}
			s.finishOp(opName, nil, err)
			_ = s.save()
			return
		}
		if cur.State == via {
			cur.State = state
			if phase != dmspb.MigrationJob_PHASE_UNSPECIFIED || state == dmspb.MigrationJob_COMPLETED {
				cur.Phase = phase
			}
		}
		s.finishOp(opName, s.answer(cur), nil)
		_ = s.save()
	})
	return clone(op), nil
}

// verificationError is one failed check.
func verificationError(code dmspb.MigrationJobVerificationError_ErrorCode, msg, detail string) *dmspb.MigrationJobVerificationError {
	return &dmspb.MigrationJobVerificationError{ErrorCode: code, ErrorMessage: msg, ErrorDetailMessage: detail}
}

// verificationFailed is the error a failed verification answers: Google's
// FAILED_PRECONDITION, each check's MigrationJobVerificationError a detail.
func verificationFailed(errs []*dmspb.MigrationJobVerificationError) error {
	var msgs []string
	e := &apierror.Error{Reason: "conditionNotMet"}
	for _, v := range errs {
		msgs = append(msgs, v.GetErrorCode().String()+": "+v.GetErrorMessage())
		if a, err := anypb.New(v); err == nil {
			e.Details = append(e.Details, a)
		}
	}
	fp := failedPrecondition("migration job verification failed: %s", strings.Join(msgs, "; ")).(*apierror.Error)
	e.Code, e.Message = fp.Code, fp.Message
	return e
}

func (s *Server) VerifyMigrationJob(_ context.Context, req *dmspb.VerifyMigrationJobRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	j, err := s.jobLocked(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	probe := clone(j)
	if req.GetMigrationJob() != nil {
		// "The changed migration job parameters to verify ... It will not
		// update the migration job."
		for _, p := range req.GetUpdateMask().GetPaths() {
			switch p {
			case "type":
				probe.Type = req.GetMigrationJob().GetType()
			case "source":
				probe.Source = req.GetMigrationJob().GetSource()
			case "destination":
				probe.Destination = req.GetMigrationJob().GetDestination()
			default:
				s.mu.Unlock()
				return nil, apierror.InvalidArgument("update_mask path %q cannot be verified", p)
			}
		}
		m := jobRE.FindStringSubmatch(j.GetName())
		if err := s.checkJob(m[1], m[2], probe); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	jc, err := s.contextLocked(probe)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	started := j.State != dmspb.MigrationJob_NOT_STARTED && j.State != dmspb.MigrationJob_DRAFT
	op, err := s.startOp(parentOf(j.GetName()), "verify", j.GetName(), "")
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	name, opName := j.GetName(), op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		errs := s.verify(ctx, jc, !started)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(errs) > 0 {
			s.finishOp(opName, nil, verificationFailed(errs))
		} else if cur := s.jobs[name]; cur != nil {
			s.finishOp(opName, s.answer(cur), nil)
		} else {
			s.finishOp(opName, nil, apierror.Aborted("migration job %s was deleted", name))
		}
		_ = s.save()
	})
	return clone(op), nil
}

func (s *Server) StartMigrationJob(_ context.Context, req *dmspb.StartMigrationJobRequest) (*longrunningpb.Operation, error) {
	skip := req.GetSkipValidation()
	return s.transition(req.GetName(), "start", []dmspb.MigrationJob_State{dmspb.MigrationJob_NOT_STARTED, dmspb.MigrationJob_DRAFT},
		dmspb.MigrationJob_STARTING, 0, func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
			return s.begin(ctx, jc, skip)
		})
}

// begin verifies a job (unless skip) and starts its runner: the job is
// then RUNNING in its FULL_DUMP phase.
func (s *Server) begin(ctx context.Context, jc jobContext, skip bool) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
	if !skip {
		if errs := s.verify(ctx, jc, true); len(errs) > 0 {
			return dmspb.MigrationJob_STATE_UNSPECIFIED, 0, verificationFailed(errs)
		}
	}
	s.mu.Lock()
	s.started[jc.name] = s.now()
	if j := s.jobs[jc.name]; j != nil {
		j.Error, j.EndTime = nil, nil
	}
	s.mu.Unlock()
	s.run(jc)
	return dmspb.MigrationJob_RUNNING, dmspb.MigrationJob_FULL_DUMP, nil
}

// run copies the job's databases beside the request: the full dump, then
// for a continuous job the CDC phase, for a one-time job its completion.
func (s *Server) run(jc jobContext) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if c := s.cancel[jc.name]; c != nil {
		c()
	}
	s.cancel[jc.name] = cancel
	s.mu.Unlock()
	s.background(func() {
		defer cancel()
		err := s.copyJob(ctx, jc)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.jobs[jc.name]
		if cur == nil || cur.State != dmspb.MigrationJob_RUNNING && cur.State != dmspb.MigrationJob_STARTING &&
			cur.State != dmspb.MigrationJob_RESTARTING {
			return
		}
		cur.UpdateTime = ts(s.now())
		switch {
		case err != nil:
			cur.State, cur.Error = dmspb.MigrationJob_FAILED, statusOf(err)
		case jc.job.GetType() == dmspb.MigrationJob_ONE_TIME:
			cur.State, cur.Phase, cur.EndTime = dmspb.MigrationJob_COMPLETED, dmspb.MigrationJob_PHASE_UNSPECIFIED, ts(s.now())
		default:
			cur.State, cur.Phase = dmspb.MigrationJob_RUNNING, dmspb.MigrationJob_CDC
		}
		delete(s.cancel, jc.name)
		_ = s.save()
	})
}

// copyJob runs the full dump and waits for it to finish; a one-time job's
// destination is then promoted, as it replicates nothing after.
func (s *Server) copyJob(ctx context.Context, jc jobContext) error {
	dbs, err := s.sourceDatabases(ctx, jc)
	if err != nil {
		return err
	}
	continuous := jc.job.GetType() == dmspb.MigrationJob_CONTINUOUS
	if err := s.opts.Backend.Migrate(ctx, jc.target, jc.src, dbs, continuous); err != nil {
		return fmt.Errorf("the full dump: %w", err)
	}
	if continuous && !jc.target.MySQL() {
		for {
			done, err := s.copied(ctx, jc.target, dbs)
			if err != nil {
				return err
			}
			if done {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.poll()):
			}
		}
	}
	if !continuous {
		// UNVERIFIED: a one-time job is "Completed after the full dump";
		// its destination is left standalone, which is what it is once
		// nothing replicates to it.
		// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/postgres/migration-job-statuses
		return s.promoteTarget(ctx, jc, dbs)
	}
	return nil
}

// copied reports whether every table of a PostgreSQL target's
// subscriptions has its initial copy (pg_subscription_rel's ready or
// synchronized states).
func (s *Server) copied(ctx context.Context, t Target, dbs []string) (bool, error) {
	for _, db := range dbs {
		rows, err := s.opts.Backend.QueryTarget(ctx, t, db,
			`SELECT (SELECT count(*) FROM pg_subscription_rel WHERE srsubstate NOT IN ('r', 's')),
			  (SELECT count(*) FROM pg_subscription WHERE subdbid = (SELECT oid FROM pg_database WHERE datname = current_database()))`)
		if err != nil {
			return false, err
		}
		if len(rows) != 1 || len(rows[0]) != 2 {
			return false, fmt.Errorf("the copy's progress in %s: unexpected answer %v", db, rows)
		}
		if rows[0][0] != "0" {
			return false, nil
		}
		if rows[0][1] == "0" {
			return false, fmt.Errorf("the database %s has no subscription to its source", db)
		}
	}
	return true, nil
}

// systemPostgres are the databases a PostgreSQL migration leaves out:
// "all databases ... except the template databases", and the providers'
// own (cloudsqladmin, rdsadmin, alloydbadmin) a managed source keeps.
const sourceDatabasesQuery = `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate
  AND datname NOT IN ('template0', 'template1', 'cloudsqladmin', 'rdsadmin', 'alloydbadmin') ORDER BY 1`

// sourceDatabases are the databases a job migrates: a PostgreSQL source's
// (a MySQL source's are every one but its system schemas, which the
// Backend finds itself).
func (s *Server) sourceDatabases(ctx context.Context, jc jobContext) ([]string, error) {
	if jc.src.Engine == dmspb.DatabaseEngine_MYSQL {
		return nil, nil
	}
	rows, err := s.opts.Backend.QuerySource(ctx, jc.target, jc.src, "postgres", sourceDatabasesQuery)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if len(r) > 0 && r[0] != "" {
			out = append(out, r[0])
		}
	}
	return out, nil
}

func (s *Server) StopMigrationJob(_ context.Context, req *dmspb.StopMigrationJobRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	j, err := s.jobLocked(req.GetName())
	var phase dmspb.MigrationJob_Phase
	if err == nil {
		phase = j.Phase
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.transition(req.GetName(), "stop", []dmspb.MigrationJob_State{dmspb.MigrationJob_RUNNING}, dmspb.MigrationJob_STOPPING, 0,
		func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
			if phase != dmspb.MigrationJob_CDC {
				// Stopped in its full dump, a job is restarted, not resumed.
				s.stopRunner(jc.name)
				return dmspb.MigrationJob_STOPPED, phase, nil
			}
			if err := s.opts.Backend.Pause(ctx, jc.target, true); err != nil {
				return 0, 0, err
			}
			return dmspb.MigrationJob_STOPPED, dmspb.MigrationJob_CDC, nil
		})
}

func (s *Server) stopRunner(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.cancel[name]; c != nil {
		c()
		delete(s.cancel, name)
	}
}

func (s *Server) ResumeMigrationJob(_ context.Context, req *dmspb.ResumeMigrationJobRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	j, err := s.jobLocked(req.GetName())
	if err == nil && j.State == dmspb.MigrationJob_STOPPED && j.Phase != dmspb.MigrationJob_CDC {
		err = failedPrecondition("migration job %s was stopped in its %s phase: only a job stopped in its CDC phase is resumed; restart it", j.GetName(), j.Phase)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.transition(req.GetName(), "resume", []dmspb.MigrationJob_State{dmspb.MigrationJob_STOPPED}, dmspb.MigrationJob_RESUMING, 0,
		func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
			if err := s.opts.Backend.Pause(ctx, jc.target, false); err != nil {
				return 0, 0, err
			}
			return dmspb.MigrationJob_RUNNING, dmspb.MigrationJob_CDC, nil
		})
}

func (s *Server) PromoteMigrationJob(_ context.Context, req *dmspb.PromoteMigrationJobRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	j, err := s.jobLocked(req.GetName())
	if err == nil && j.Phase != dmspb.MigrationJob_CDC {
		err = failedPrecondition("migration job %s is in its %s phase: a job is promoted in its CDC phase", j.GetName(), j.Phase)
	}
	if err == nil && j.GetType() != dmspb.MigrationJob_CONTINUOUS {
		err = failedPrecondition("migration job %s is ONE_TIME: only a continuous job is promoted", j.GetName())
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	allowed := []dmspb.MigrationJob_State{dmspb.MigrationJob_RUNNING, dmspb.MigrationJob_STOPPED, dmspb.MigrationJob_FAILED}
	return s.transition(req.GetName(), "promote", allowed, dmspb.MigrationJob_RUNNING, dmspb.MigrationJob_PROMOTE_IN_PROGRESS,
		func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
			dbs, err := s.sourceDatabases(ctx, jc)
			if err != nil {
				return dmspb.MigrationJob_FAILED, 0, err
			}
			if err := s.promoteTarget(ctx, jc, dbs); err != nil {
				return dmspb.MigrationJob_FAILED, 0, err
			}
			s.mu.Lock()
			if cur := s.jobs[jc.name]; cur != nil {
				cur.EndTime = ts(s.now())
			}
			s.mu.Unlock()
			return dmspb.MigrationJob_COMPLETED, dmspb.MigrationJob_PHASE_UNSPECIFIED, nil
		})
}

// promoteTarget ends a target's replication: a PostgreSQL target's
// sequences are first set to the source's values, which logical
// replication does not carry.
func (s *Server) promoteTarget(ctx context.Context, jc jobContext, dbs []string) error {
	if !jc.target.MySQL() {
		for _, db := range dbs {
			rows, err := s.opts.Backend.QuerySource(ctx, jc.target, jc.src, db,
				`SELECT format('%I.%I', schemaname, sequencename), last_value FROM pg_sequences WHERE last_value IS NOT NULL ORDER BY 1`)
			if err != nil {
				return fmt.Errorf("read the source's sequences in %s: %w", db, err)
			}
			for _, r := range rows {
				if len(r) != 2 {
					continue
				}
				stmt := fmt.Sprintf("SELECT setval(%s, %s, true)", quoteLiteral(r[0]), quoteLiteral(r[1]))
				if _, err := s.opts.Backend.QueryTarget(ctx, jc.target, db, stmt+"::bigint"); err != nil {
					return fmt.Errorf("set the sequence %s in %s: %w", r[0], db, err)
				}
			}
		}
	}
	return s.opts.Backend.Promote(ctx, jc.target)
}

func quoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

func (s *Server) RestartMigrationJob(_ context.Context, req *dmspb.RestartMigrationJobRequest) (*longrunningpb.Operation, error) {
	skip := req.GetSkipValidation()
	allowed := []dmspb.MigrationJob_State{dmspb.MigrationJob_STOPPED, dmspb.MigrationJob_FAILED, dmspb.MigrationJob_RUNNING}
	return s.transition(req.GetName(), "restart", allowed, dmspb.MigrationJob_RESTARTING, 0,
		func(ctx context.Context, jc jobContext) (dmspb.MigrationJob_State, dmspb.MigrationJob_Phase, error) {
			s.stopRunner(jc.name)
			// "It resets the destination and restarts from scratch."
			if err := s.resetTarget(ctx, jc); err != nil {
				return dmspb.MigrationJob_FAILED, 0, err
			}
			return s.begin(ctx, jc, skip)
		})
}

// resetTarget empties a destination for a restart: a Cloud SQL replica is
// created again (its sync runs once), an AlloyDB cluster's migration is
// undone.
func (s *Server) resetTarget(ctx context.Context, jc jobContext) error {
	if jc.target.CloudSQL {
		if err := s.opts.Backend.Delete(ctx, jc.target); err != nil {
			return fmt.Errorf("delete the destination replica: %w", err)
		}
		addrs, err := s.opts.Backend.CreateCloudSQL(ctx, jc.target, jc.dst.GetCloudsql().GetSettings(), jc.src)
		if err != nil {
			return fmt.Errorf("create the destination replica again: %w", err)
		}
		s.mu.Lock()
		if p := s.profiles[jc.dst.GetName()]; p != nil && p.GetCloudsql() != nil {
			p.GetCloudsql().PublicIp, p.GetCloudsql().PrivateIp = addrs.PublicIP, addrs.PrivateIP
		}
		s.mu.Unlock()
		return nil
	}
	dbs, err := s.sourceDatabases(ctx, jc)
	if err != nil {
		return err
	}
	return s.opts.Backend.Reset(ctx, jc.target, dbs)
}

func (s *Server) DeleteMigrationJob(_ context.Context, req *dmspb.DeleteMigrationJobRequest) (*longrunningpb.Operation, error) {
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	j, err := s.jobLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if j.State == dmspb.MigrationJob_DELETING || j.Phase == dmspb.MigrationJob_PROMOTE_IN_PROGRESS {
		return nil, failedPrecondition("migration job %s is %s", j.GetName(), j.State)
	}
	name := j.GetName()
	if c := s.cancel[name]; c != nil {
		c()
		delete(s.cancel, name)
	}
	dst := s.profiles[j.GetDestination()]
	var t Target
	var replicating bool
	if dst != nil {
		t, _ = targetOf(dst)
		replicating = j.State != dmspb.MigrationJob_COMPLETED && j.State != dmspb.MigrationJob_NOT_STARTED && j.State != dmspb.MigrationJob_DRAFT
	}
	j.State = dmspb.MigrationJob_DELETING
	op, err := s.startOp(parentOf(name), "delete", name, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	_ = s.save()
	force, opName := req.GetForce(), op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), destinationTimeout)
		defer cancel()
		var err error
		switch {
		case dst == nil || s.opts.Backend == nil:
		case t.CloudSQL && force:
			// "In case of force delete, the destination CloudSQL replica
			// database is also deleted."
			err = s.opts.Backend.Delete(ctx, t)
		case !t.CloudSQL && replicating:
			// UNVERIFIED: an AlloyDB destination left mid-migration stops
			// replicating, so the source's replication slots are freed; its
			// data stays.
			// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.migrationJobs/delete
			err = s.opts.Backend.Promote(ctx, t)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			if cur := s.jobs[name]; cur != nil {
				cur.State, cur.Error = dmspb.MigrationJob_FAILED, statusOf(err)
			}
			s.finishOp(opName, nil, err)
			_ = s.save()
			return
		}
		delete(s.jobs, name)
		delete(s.policies, name)
		delete(s.started, name)
		if dst != nil && t.CloudSQL {
			// "The destination CloudSQL connection profile is always deleted
			// with the migration job."
			delete(s.profiles, dst.GetName())
			delete(s.policies, dst.GetName())
		}
		s.finishOp(opName, &emptyResp, nil)
		_ = s.save()
	})
	return clone(op), nil
}

func (s *Server) GenerateSshScript(context.Context, *dmspb.GenerateSshScriptRequest) (*dmspb.SshScript, error) {
	return nil, unimplemented("migrationJobs.generateSshScript",
		"reverse SSH connectivity through a bastion VM is not run: the destinations' servers reach a source directly, on the instance's network (docs/gaps.md)")
}

func (s *Server) GenerateTcpProxyScript(context.Context, *dmspb.GenerateTcpProxyScriptRequest) (*dmspb.TcpProxyScript, error) {
	return nil, unimplemented("migrationJobs.generateTcpProxyScript",
		"a TCP proxy VM in front of a Private Service Connect source is not run (docs/gaps.md)")
}

// Resume picks up the jobs a previous run left: one in its full dump
// waits for it again; one caught in a transition is FAILED, to be
// restarted.
func (s *Server) Resume() {
	s.mu.Lock()
	var copying []jobContext
	for _, k := range sortedKeys(s.jobs, "") {
		j := s.jobs[k]
		switch {
		case j.State == dmspb.MigrationJob_RUNNING && j.Phase == dmspb.MigrationJob_FULL_DUMP:
			if jc, err := s.contextLocked(j); err == nil {
				copying = append(copying, jc)
			}
		case j.State == dmspb.MigrationJob_STARTING || j.State == dmspb.MigrationJob_STOPPING || j.State == dmspb.MigrationJob_RESUMING ||
			j.State == dmspb.MigrationJob_RESTARTING || j.State == dmspb.MigrationJob_DELETING || j.Phase == dmspb.MigrationJob_PROMOTE_IN_PROGRESS:
			j.State, j.Error = dmspb.MigrationJob_FAILED, statusOf(apierror.Aborted("CloudBurrow stopped while the job was %s; restart it", j.State))
		}
	}
	for k, p := range s.profiles {
		if p.State == dmspb.ConnectionProfile_CREATING || p.State == dmspb.ConnectionProfile_DELETING {
			p.State, p.Error = dmspb.ConnectionProfile_FAILED, statusOf(apierror.Aborted("CloudBurrow stopped while %s was %s", k, p.State))
		}
	}
	_ = s.save()
	s.mu.Unlock()
	for _, jc := range copying {
		jc := jc
		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.cancel[jc.name] = cancel
		s.mu.Unlock()
		s.background(func() {
			defer cancel()
			dbs, err := s.sourceDatabases(ctx, jc)
			for err == nil && !jc.target.MySQL() && jc.job.GetType() == dmspb.MigrationJob_CONTINUOUS {
				var done bool
				if done, err = s.copied(ctx, jc.target, dbs); done || err != nil {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.poll()):
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur := s.jobs[jc.name]; cur != nil && cur.State == dmspb.MigrationJob_RUNNING && cur.Phase == dmspb.MigrationJob_FULL_DUMP {
				if err != nil {
					cur.State, cur.Error = dmspb.MigrationJob_FAILED, statusOf(err)
				} else if jc.job.GetType() == dmspb.MigrationJob_CONTINUOUS {
					cur.Phase = dmspb.MigrationJob_CDC
				}
				_ = s.save()
			}
		})
	}
}
