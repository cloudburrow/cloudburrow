package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/datamigration"
)

// The console's Database Migration Service pages (#2252), as Google's
// menu has them (console.cloud.google.com/dbmigrate, read 2026-11-25):
// Migration jobs (a project's jobs in every region; Create migration job
// with its type, source and destination profiles and connectivity, Create
// and Create & start; a job's page with its status, phase and details, and
// Verify, Start, Stop, Resume, Promote, Restart, Edit and Delete),
// Conversion workspaces (not served: the page says why), Connection
// profiles (sources and destinations; Create profile for a MySQL or
// PostgreSQL source, a Cloud SQL or an AlloyDB destination; Edit and
// Delete) and Private connectivity configurations (Create and Delete).
// AlloyDB's Clusters and Cloud SQL's Instances pages have Google's Migrate
// button, which opens Migration jobs.

const dmsNotStarted = "Database Migration Service has not started"

func dmsAPI(svc *dataMigrationService) (*datamigration.Server, error) {
	s := svc.API()
	if s == nil {
		return nil, errors.New(dmsNotStarted)
	}
	return s, nil
}

// dmsHref is the Migration jobs page, which the Migrate buttons open.
const dmsHref = "/dbmigrate/migrations"

// dmsMigrateAction is AlloyDB's and Cloud SQL's Migrate button.
func dmsMigrateAction() console.Action {
	return console.Action{ID: "migrate", Label: "Migrate", Href: dmsHref, Button: true}
}

// dmsWait waits a short while for an operation the console started, so a
// quick one's error is shown on the form; a long one goes on.
func dmsWait(s *datamigration.Server) func(op *longrunningpb.Operation, err error) error {
	return func(op *longrunningpb.Operation, err error) error { return dmsWaitOp(s, op, err) }
}

func dmsWaitOp(s *datamigration.Server, op *longrunningpb.Operation, err error) error {
	if err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for !op.GetDone() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		next, gerr := s.Operation(op.GetName())
		if gerr != nil {
			return nil
		}
		op = next
	}
	if e := op.GetError(); e != nil {
		return errors.New(e.GetMessage())
	}
	return nil
}

var dmsStates = map[string]string{"NOT_STARTED": "Not started", "DRAFT": "Draft", "STARTING": "Starting", "RUNNING": "Running",
	"STOPPING": "Stopping", "STOPPED": "Stopped", "RESUMING": "Resuming", "RESTARTING": "Restarting", "FAILED": "Failed",
	"COMPLETED": "Completed", "DELETING": "Deleting", "MAINTENANCE": "Maintenance", "UPDATING": "Updating", "DELETED": "Deleted"}

var dmsPhases = map[string]string{"FULL_DUMP": "Full dump", "CDC": "CDC", "PROMOTE_IN_PROGRESS": "Promote in progress",
	"WAITING_FOR_SOURCE_WRITES_TO_STOP": "Waiting for source writes to stop", "PREPARING_THE_DUMP": "Preparing the dump", "READY_FOR_PROMOTE": "Ready for promote"}

func dmsStatus(j *dmspb.MigrationJob) string {
	st := dmsStates[j.GetState().String()]
	if st == "" {
		st = j.GetState().String()
	}
	if ph := dmsPhases[j.GetPhase().String()]; ph != "" && (j.GetState() == dmspb.MigrationJob_RUNNING || j.GetState() == dmspb.MigrationJob_STOPPED) {
		st += " (" + ph + ")"
	}
	return st
}

func dmsType(t dmspb.MigrationJob_Type) string {
	switch t {
	case dmspb.MigrationJob_ONE_TIME:
		return "One-time"
	case dmspb.MigrationJob_CONTINUOUS:
		return "Continuous"
	}
	return "-"
}

func dmsDuration(j *dmspb.MigrationJob) string {
	if j.GetDuration() == nil {
		return "-"
	}
	return j.GetDuration().AsDuration().Truncate(time.Second).String()
}

// dmsEngine is a profile's database engine as the console names it.
func dmsEngine(p *dmspb.ConnectionProfile) string {
	if _, ok := datamigration.ExistingDestination(p); ok {
		switch {
		case p.GetMysql() != nil:
			return "Cloud SQL for MySQL"
		case p.GetAlloydb() != nil, datamigration.AlloyDBClusterID(p) != "":
			return "AlloyDB for PostgreSQL"
		}
		return "Cloud SQL for PostgreSQL"
	}
	switch {
	case p.GetMysql() != nil:
		return "MySQL"
	case p.GetPostgresql() != nil:
		return "PostgreSQL"
	case p.GetCloudsql() != nil:
		v := p.GetCloudsql().GetSettings().GetDatabaseVersion().String()
		if n := datamigration.CloudSQLVersionName(p.GetCloudsql().GetSettings()); n != "" {
			v = n
		}
		if strings.HasPrefix(v, "MYSQL") {
			return "Cloud SQL for MySQL"
		}
		return "Cloud SQL for PostgreSQL"
	case p.GetAlloydb() != nil:
		return "AlloyDB for PostgreSQL"
	case p.GetOracle() != nil:
		return "Oracle"
	}
	return "-"
}

func dmsIsSource(p *dmspb.ConnectionProfile) bool {
	_, existing := datamigration.ExistingDestination(p)
	return (p.GetMysql() != nil || p.GetPostgresql() != nil) && !existing
}

func dmsHostPort(p *dmspb.ConnectionProfile) string {
	if id, ok := datamigration.ExistingDestination(p); ok {
		return id
	}
	switch {
	case p.GetMysql() != nil:
		return p.GetMysql().GetHost() + ":" + strconv.Itoa(int(p.GetMysql().GetPort()))
	case p.GetPostgresql() != nil:
		return p.GetPostgresql().GetHost() + ":" + strconv.Itoa(int(p.GetPostgresql().GetPort()))
	case p.GetCloudsql() != nil:
		return kmsOrDash(p.GetCloudsql().GetPublicIp())
	}
	return "-"
}

// dmsProfiles are a project's connection profiles in every region.
func dmsProfiles(ctx context.Context, s *datamigration.Server, project string) []*dmspb.ConnectionProfile {
	var out []*dmspb.ConnectionProfile
	for _, loc := range datamigration.Locations() {
		resp, err := s.ListConnectionProfiles(ctx, &dmspb.ListConnectionProfilesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err == nil {
			out = append(out, resp.GetConnectionProfiles()...)
		}
	}
	return out
}

// ---- Migration jobs ----

type dmsJobsProvider struct{ svc *dataMigrationService }

func (dmsJobsProvider) ID() string    { return "datamigration" }
func (dmsJobsProvider) Title() string { return "Migration jobs" }

func (p dmsJobsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Migration job name", "Status", "Type", "Source", "Destination", "Region", "Duration", "Created"},
		NameColumn: "Migration job ID", Noun: "migration jobs"}
	if project == "" {
		l.Prompt = "Database Migration Service lists a project's migration jobs. Choose one in the toolbar."
		return l, nil
	}
	for _, loc := range datamigration.Locations() {
		resp, err := s.ListMigrationJobs(ctx, &dmspb.ListMigrationJobsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, j := range resp.GetMigrationJobs() {
			l.Items = append(l.Items, console.Resource{Name: j.GetName(), Status: j.GetState().String(), Fields: map[string]string{
				"Migration job name": kmsOrDash(j.GetDisplayName()), "Status": dmsStatus(j), "Type": dmsType(j.GetType()),
				"Source": lastSegment(j.GetSource()), "Destination": lastSegment(j.GetDestination()), "Region": kmsLocation(j.GetName()),
				"Duration": dmsDuration(j), "Created": kmsTime(j.GetCreateTime())}})
		}
	}
	l.Total, l.RowsOpenable = len(l.Items), true
	return l, nil
}

func (dmsJobsProvider) CreateTitle() string { return "Create a migration job" }

func (p dmsJobsProvider) CreateForm() (string, []console.Field) {
	return "Create migration job", p.jobFields(nil, nil)
}

// CreateFormFor offers the project's source and destination profiles.
func (p dmsJobsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	s, err := dmsAPI(p.svc)
	if err != nil || project == "" {
		return p.jobFields(nil, nil)
	}
	var srcs, dsts []string
	for _, pr := range dmsProfiles(ctx, s, project) {
		if pr.GetState() != dmspb.ConnectionProfile_READY {
			continue
		}
		if dmsIsSource(pr) {
			srcs = append(srcs, pr.GetName())
		} else {
			dsts = append(dsts, pr.GetName())
		}
	}
	return p.jobFields(srcs, dsts)
}

func (p dmsJobsProvider) jobFields(srcs, dsts []string) []console.Field {
	const start, source, dest, conn, test = "Get started", "Define your source", "Create your destination", "Define connectivity method", "Test and create migration job"
	label := func(names []string) map[string]string {
		m := map[string]string{}
		for _, n := range names {
			m[n] = lastSegment(n) + " (" + kmsLocation(n) + ")"
		}
		return m
	}
	return []console.Field{
		{Name: "displayName", Label: "Migration job name", Type: "text", Section: start},
		{Name: "jobId", Label: "Migration job ID", Type: "text", Required: true, Section: start, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "Lower-case letters, numbers and hyphens, starting with a letter; at most 63 characters."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: start, Options: datamigration.Locations(), Default: "us-central1",
			Help: "The region of the job and of its connection profiles."},
		{Name: "type", Label: "Migration job type", Type: "radio", Section: start, Options: []string{"CONTINUOUS", "ONE_TIME"}, Default: "CONTINUOUS",
			OptionLabels: map[string]string{"CONTINUOUS": "Continuous", "ONE_TIME": "One-time"},
			Help:         "A continuous job copies the source's databases and goes on replicating them until you promote it; a one-time job copies them once."},
		{Name: "source", Label: "Source connection profile", Type: "select", Required: true, Section: source, Options: srcs, OptionLabels: label(srcs),
			Help: "A MySQL or PostgreSQL source the destination's server can reach. Create one on Connection profiles."},
		{Name: "native", Label: "Use PostgreSQL native logical replication", Type: "checkbox", Section: source,
			Help: "For a PostgreSQL source: no pglogical extension is needed on it (a quick-start migration)."},
		{Name: "destination", Label: "Destination connection profile", Type: "select", Required: true, Section: dest, Options: dsts, OptionLabels: label(dsts),
			Help: "A Cloud SQL destination created for this source, or an AlloyDB destination. Create one on Connection profiles."},
		{Name: "databases", Label: "Databases to migrate", Type: "text", Section: source,
			Help: "Comma-separated database names; empty migrates all databases (Specific databases, #2257)."},
		{Name: "dumpFlags", Label: "Dump flags", Type: "text", Section: source,
			Help: "For a MySQL source: mysqldump flags as name=value, comma-separated (max-allowed-packet=64M,add-locks=)."},
		{Name: "dumpPath", Label: "Manual dump file", Type: "text", Section: source,
			Help: "For a MySQL source with GTIDs: gs://BUCKET/OBJECT, a dump loaded instead of Database Migration Service's own; excludes dump flags."},
		{Name: "connectivity", Label: "Connectivity method", Type: "radio", Section: conn, Options: []string{"static", "peering", "reverse"}, Default: "static",
			OptionLabels: map[string]string{"static": "IP allowlist", "peering": "VPC peering", "reverse": "Reverse-SSH tunnel via cloud-hosted VM"},
			Help:         "With an IP allowlist, allow the destination's outgoing address (Fetch static IPs) on the source; with a reverse-SSH tunnel, the destination connects to the bastion's port, which the source's side forwards to the source."},
		{Name: "vpc", Label: "VPC network", Type: "text", Section: conn, ShowWhen: "connectivity=peering", Help: "projects/PROJECT/global/networks/NETWORK"},
		{Name: "vm", Label: "VM instance name", Type: "text", Section: conn, ShowWhen: "connectivity=reverse"},
		{Name: "vmIp", Label: "VM IP address", Type: "text", Section: conn, ShowWhen: "connectivity=reverse", Help: "The bastion's address the destination reaches."},
		{Name: "vmPort", Label: "VM port", Type: "number", Section: conn, ShowWhen: "connectivity=reverse", Help: "The bastion's forwarding port."},
		{Name: "vmVpc", Label: "VPC", Type: "text", Section: conn, ShowWhen: "connectivity=reverse", Help: "projects/PROJECT/global/networks/NETWORK"},
		{Name: "startNow", Label: "Start the job once it is created (Create & start job)", Type: "checkbox", Section: test},
	}
}

func (p dmsJobsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a migration job")
	}
	parent := "projects/" + project + "/locations/" + strings.TrimSpace(v["region"])
	id := strings.TrimSpace(v["jobId"])
	typ := dmspb.MigrationJob_CONTINUOUS
	if v["type"] == "ONE_TIME" {
		typ = dmspb.MigrationJob_ONE_TIME
	}
	j := &dmspb.MigrationJob{DisplayName: v["displayName"], Type: typ, Source: v["source"], Destination: v["destination"]}
	switch v["connectivity"] {
	case "peering":
		j.Connectivity = &dmspb.MigrationJob_VpcPeeringConnectivity{VpcPeeringConnectivity: &dmspb.VpcPeeringConnectivity{Vpc: strings.TrimSpace(v["vpc"])}}
	case "reverse":
		port, _ := strconv.Atoi(strings.TrimSpace(v["vmPort"]))
		j.Connectivity = &dmspb.MigrationJob_ReverseSshConnectivity{ReverseSshConnectivity: &dmspb.ReverseSshConnectivity{
			Vm: strings.TrimSpace(v["vm"]), VmIp: strings.TrimSpace(v["vmIp"]), VmPort: int32(port), Vpc: strings.TrimSpace(v["vmVpc"])}}
	default:
		j.Connectivity = &dmspb.MigrationJob_StaticIpConnectivity{StaticIpConnectivity: &dmspb.StaticIpConnectivity{}}
	}
	if v["native"] == "true" || v["native"] == "on" {
		if err := datamigration.SetNativeLogical(j, true); err != nil {
			return "", err
		}
	}
	j.DumpPath = strings.TrimSpace(v["dumpPath"])
	for _, kv := range splitList(v["dumpFlags"]) {
		name, value, _ := strings.Cut(kv, "=")
		if j.DumpFlags == nil {
			j.DumpFlags = &dmspb.MigrationJob_DumpFlags{}
		}
		j.DumpFlags.DumpFlags = append(j.DumpFlags.DumpFlags, &dmspb.MigrationJob_DumpFlag{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	if err := datamigration.SetObjectsConfig(j, splitList(v["databases"])); err != nil {
		return "", err
	}
	if err := dmsWait(s)(s.CreateMigrationJob(ctx, &dmspb.CreateMigrationJobRequest{Parent: parent, MigrationJobId: id, MigrationJob: j})); err != nil {
		return "", err
	}
	name := parent + "/migrationJobs/" + id
	if v["startNow"] == "true" || v["startNow"] == "on" {
		if _, err := s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: name}); err != nil {
			return name, err
		}
	}
	return name, nil
}

func (p dmsJobsProvider) job(ctx context.Context, project, name string) (*datamigration.Server, *dmspb.MigrationJob, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, nil, err
	}
	j, err := s.GetMigrationJob(ctx, &dmspb.GetMigrationJobRequest{Name: name})
	return s, j, err
}

func (p dmsJobsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the migration job: " + consoleErr(err)}, nil
	}
	errText := "-"
	if e := j.GetError(); e != nil {
		errText = e.GetMessage()
	}
	conn := "IP allowlist"
	if v := j.GetVpcPeeringConnectivity(); v != nil {
		conn = "VPC peering: " + v.GetVpc()
	}
	if r := j.GetReverseSshConnectivity(); r != nil {
		conn = fmt.Sprintf("Reverse-SSH tunnel: %s:%d (VM %s)", r.GetVmIp(), r.GetVmPort(), kmsOrDash(r.GetVm()))
	}
	dbs := "All databases"
	if d := datamigration.SelectedDatabases(j); len(d) > 0 {
		dbs = strings.Join(d, ", ")
	}
	var flags []string
	for _, f := range j.GetDumpFlags().GetDumpFlags() {
		flags = append(flags, f.GetName()+"="+f.GetValue())
	}
	demoted := "-"
	if dst, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: j.GetDestination()}); err == nil {
		if _, ok := datamigration.ExistingDestination(dst); ok {
			demoted = "No"
			if s.Demoted(j.GetName()) {
				demoted = "Yes"
			}
		}
	}
	objects := console.Listing{Columns: []string{"Database", "Status", "Phase"}, NameColumn: "Object", Noun: "source objects (Fetch source objects reads them)"}
	for _, o := range s.Objects(j.GetName()) {
		objects.Items = append(objects.Items, console.Resource{Name: o.Name, Fields: map[string]string{"Database": o.Database, "Status": o.State, "Phase": kmsOrDash(o.Phase)}})
	}
	objects.Total = len(objects.Items)
	native := "No"
	if datamigration.NativeLogical(j) {
		native = "Yes"
	}
	ips := "-"
	if r, err := s.FetchStaticIps(ctx, &dmspb.FetchStaticIpsRequest{Name: parentOfName(j.GetName(), 2)}); err == nil && len(r.GetStaticIps()) > 0 {
		ips = strings.Join(r.GetStaticIps(), ", ")
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Status", Value: dmsStatus(j)}, {Label: "Type", Value: dmsType(j.GetType())}, {Label: "Region", Value: kmsLocation(j.GetName())}},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Migration job", Properties: []console.Property{
				{Label: "Migration job ID", Value: lastSegment(j.GetName())}, {Label: "Migration job name", Value: kmsOrDash(j.GetDisplayName())},
				{Label: "Status", Value: dmsStatus(j)}, {Label: "Type", Value: dmsType(j.GetType())}, {Label: "Error", Value: errText},
				{Label: "Duration", Value: dmsDuration(j)}, {Label: "Created", Value: kmsTime(j.GetCreateTime())}, {Label: "Last updated", Value: kmsTime(j.GetUpdateTime())},
				{Label: "Ended", Value: kmsTime(j.GetEndTime())}}},
			{Heading: "Source", Properties: []console.Property{{Label: "Connection profile", Value: j.GetSource()},
				{Label: "Database engine", Value: j.GetSourceDatabase().GetEngine().String()}, {Label: "Native logical replication", Value: native},
				{Label: "Databases to migrate", Value: dbs}, {Label: "Dump flags", Value: kmsOrDash(strings.Join(flags, ", "))},
				{Label: "Manual dump file", Value: kmsOrDash(j.GetDumpPath())}}},
			{Heading: "Destination", Properties: []console.Property{{Label: "Connection profile", Value: j.GetDestination()},
				{Label: "Database engine", Value: j.GetDestinationDatabase().GetProvider().String() + " " + j.GetDestinationDatabase().GetEngine().String()},
				{Label: "Existing destination demoted", Value: demoted}}},
			{Heading: "Connectivity", Properties: []console.Property{{Label: "Method", Value: conn}, {Label: "Outgoing IP addresses", Value: ips}}},
		}}, {ID: "objects", Label: "Objects", Listing: objects}},
	}, nil
}

// parentOfName drops the last n segments of a resource name.
func parentOfName(name string, n int) string {
	parts := strings.Split(name, "/")
	if len(parts) <= n {
		return name
	}
	return strings.Join(parts[:len(parts)-n], "/")
}

func (p dmsJobsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return nil
	}
	st, ph := j.GetState(), j.GetPhase()
	notStarted := st == dmspb.MigrationJob_NOT_STARTED || st == dmspb.MigrationJob_DRAFT
	cdc := ph == dmspb.MigrationJob_CDC
	unavail := func(ok bool, why string) string {
		if ok {
			return ""
		}
		return why
	}
	s, _ := dmsAPI(p.svc)
	existing, demoted := "", false
	if s != nil {
		if dst, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: j.GetDestination()}); err == nil {
			existing, _ = datamigration.ExistingDestination(dst)
		}
		demoted = s.Demoted(j.GetName())
	}
	var out []console.Action
	if existing != "" {
		// Google's console: "Select and continue", then "the destination
		// instance will be demoted ... enter the destination instance ID
		// and click Confirm and continue" (mysql/create-migration-job-existing-instance,
		// read 2026-11-25).
		out = append(out, console.Action{ID: "demote", Label: "Demote destination", Destructive: true,
			Unavailable:  unavail(notStarted && !demoted, "Only a job not started yet, whose destination is not demoted, demotes it."),
			ConfirmTitle: "Demote the destination instance?",
			Confirm:      "The destination " + existing + " becomes a read replica of the source: its data, backups, point-in-time recovery and replicas are suspended or replaced by the migration.",
			ConfirmWord:  existing})
	}
	if j.GetSourceDatabase().GetEngine() == dmspb.DatabaseEngine_POSTGRESQL {
		out = append(out, console.Action{ID: "fetchObjects", Label: "Fetch source objects"})
	}
	return append(out, []console.Action{
		{ID: "verify", Label: "Test job"},
		{ID: "start", Label: "Start", Unavailable: unavail(notStarted, "Only a job not started yet is started.")},
		{ID: "stop", Label: "Stop", Unavailable: unavail(st == dmspb.MigrationJob_RUNNING, "Only a running job is stopped."),
			// UNVERIFIED: Google's docs describe no Stop or Restart dialog, so
			// the job's ID is typed back, the strictest confirmation (docs
			// re-read 2026-11-25: https://docs.cloud.google.com/database-migration/docs/postgres/migration-job-statuses).
			Confirm: "Stopped in its CDC phase, the job is resumed later; stopped in its full dump, it is restarted.", ConfirmWord: lastSegment(j.GetName())},
		{ID: "resume", Label: "Resume", Unavailable: unavail(st == dmspb.MigrationJob_STOPPED && cdc, "Only a job stopped in its CDC phase is resumed.")},
		{ID: "promote", Label: "Promote", Destructive: true, Unavailable: unavail(cdc && j.GetType() == dmspb.MigrationJob_CONTINUOUS &&
			(st == dmspb.MigrationJob_RUNNING || st == dmspb.MigrationJob_STOPPED || st == dmspb.MigrationJob_FAILED), "A continuous job is promoted in its CDC phase."),
			// "In the dialog, confirm that no errors are shown, then click
			// Promote": nothing typed (docs re-read 2026-11-25:
			// https://docs.cloud.google.com/database-migration/docs/postgres/promote-migration).
			Confirm:      "Replication stops and the destination becomes a standalone database. Stop writes to the source first.",
			ConfirmStyle: console.DeleteConfirmClick, ConfirmTitle: "Promote migration job?"},
		{ID: "restart", Label: "Restart", Unavailable: unavail(st == dmspb.MigrationJob_STOPPED || st == dmspb.MigrationJob_FAILED || st == dmspb.MigrationJob_RUNNING,
			"A stopped, failed or running job is restarted."), Confirm: "The destination is emptied and the migration starts again from scratch.",
			ConfirmWord: lastSegment(j.GetName())},
		{ID: "edit", Label: "Edit", Fields: []console.Field{{Name: "displayName", Label: "Migration job name", Type: "text", Default: j.GetDisplayName()}}},
		{ID: "delete", Label: "Delete", Destructive: true, Leaves: true, Fields: []console.Field{
			{Name: "force", Label: "Also delete the destination Cloud SQL instance", Type: "checkbox"}},
			Confirm: "A Cloud SQL destination's connection profile is deleted with the job.", ConfirmWord: lastSegment(j.GetName())},
	}...)
}

func (p dmsJobsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no migration job")
	}
	s, j, err := p.job(ctx, project, path[0])
	if err != nil {
		return err
	}
	n := j.GetName()
	switch action {
	case "demote":
		return dmsWait(s)(s.DemoteDestination(n))
	case "fetchObjects":
		return dmsWait(s)(s.FetchSourceObjects(n))
	case "verify":
		return dmsWait(s)(s.VerifyMigrationJob(ctx, &dmspb.VerifyMigrationJobRequest{Name: n}))
	case "start":
		return dmsWait(s)(s.StartMigrationJob(ctx, &dmspb.StartMigrationJobRequest{Name: n}))
	case "stop":
		return dmsWait(s)(s.StopMigrationJob(ctx, &dmspb.StopMigrationJobRequest{Name: n}))
	case "resume":
		return dmsWait(s)(s.ResumeMigrationJob(ctx, &dmspb.ResumeMigrationJobRequest{Name: n}))
	case "promote":
		return dmsWait(s)(s.PromoteMigrationJob(ctx, &dmspb.PromoteMigrationJobRequest{Name: n}))
	case "restart":
		return dmsWait(s)(s.RestartMigrationJob(ctx, &dmspb.RestartMigrationJobRequest{Name: n}))
	case "edit":
		return dmsWait(s)(s.UpdateMigrationJob(ctx, &dmspb.UpdateMigrationJobRequest{MigrationJob: &dmspb.MigrationJob{Name: n, DisplayName: v["displayName"]},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}))
	case "delete":
		force := v["force"] == "true" || v["force"] == "on"
		return dmsWait(s)(s.DeleteMigrationJob(ctx, &dmspb.DeleteMigrationJobRequest{Name: n, Force: force}))
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Connection profiles ----

type dmsProfilesProvider struct{ svc *dataMigrationService }

func (dmsProfilesProvider) ID() string    { return "datamigration-profiles" }
func (dmsProfilesProvider) Title() string { return "Connection profiles" }

func (p dmsProfilesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Connection profile name", "Role", "Database engine", "Hostname:Port", "Region", "Status", "Created"},
		NameColumn: "Connection profile ID", Noun: "connection profiles"}
	if project == "" {
		l.Prompt = "Database Migration Service lists a project's connection profiles. Choose one in the toolbar."
		return l, nil
	}
	ps := dmsProfiles(ctx, s, project)
	sort.Slice(ps, func(i, k int) bool { return ps[i].GetName() < ps[k].GetName() })
	for _, pr := range ps {
		role := "Destination"
		if dmsIsSource(pr) {
			role = "Source"
		}
		l.Items = append(l.Items, console.Resource{Name: pr.GetName(), Status: pr.GetState().String(), Fields: map[string]string{
			"Connection profile name": kmsOrDash(pr.GetDisplayName()), "Role": role, "Database engine": dmsEngine(pr), "Hostname:Port": dmsHostPort(pr),
			"Region": kmsLocation(pr.GetName()), "Status": pr.GetState().String(), "Created": kmsTime(pr.GetCreateTime())}})
	}
	l.Total, l.RowsOpenable = len(l.Items), true
	return l, nil
}

func (dmsProfilesProvider) CreateTitle() string { return "Create a connection profile" }

func (p dmsProfilesProvider) CreateForm() (string, []console.Field) {
	return "Create profile", p.profileFields(nil)
}

func (p dmsProfilesProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	s, err := dmsAPI(p.svc)
	if err != nil || project == "" {
		return p.profileFields(nil)
	}
	var srcs []string
	for _, pr := range dmsProfiles(ctx, s, project) {
		if dmsIsSource(pr) {
			srcs = append(srcs, pr.GetName())
		}
	}
	return p.profileFields(srcs)
}

func (p dmsProfilesProvider) profileFields(srcs []string) []console.Field {
	const what, conn, cs, adb = "Profile", "Define connection configurations", "Cloud SQL destination", "AlloyDB destination"
	srcLabels := map[string]string{}
	for _, n := range srcs {
		srcLabels[n] = lastSegment(n) + " (" + kmsLocation(n) + ")"
	}
	return []console.Field{
		{Name: "kind", Label: "Database engine", Type: "select", Required: true, Section: what, Default: "postgresql",
			Options: []string{"postgresql", "mysql", "cloudsql", "alloydb", "cloudsql-existing", "alloydb-existing"},
			OptionLabels: map[string]string{"postgresql": "PostgreSQL (source)", "mysql": "MySQL (source)", "cloudsql": "Cloud SQL (destination)", "alloydb": "AlloyDB for PostgreSQL (destination)",
				"cloudsql-existing": "Existing Cloud SQL instance (destination)", "alloydb-existing": "Existing AlloyDB cluster (destination)"}},
		{Name: "displayName", Label: "Connection profile name", Type: "text", Section: what},
		{Name: "profileId", Label: "Connection profile ID", Type: "text", Required: true, Section: what, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "A Cloud SQL destination's instance takes this ID."},
		{Name: "region", Label: "Region", Type: "select", Required: true, Section: what, Options: datamigration.Locations(), Default: "us-central1"},
		{Name: "host", Label: "Hostname or IP address", Type: "text", Section: conn, ShowWhen: "kind=postgresql|mysql",
			Help: "As the destination's server reaches it: a host on the instance's network."},
		{Name: "port", Label: "Port", Type: "number", Section: conn, ShowWhen: "kind=postgresql|mysql", Help: "5432 for PostgreSQL, 3306 for MySQL."},
		{Name: "username", Label: "Username", Type: "text", Section: conn, ShowWhen: "kind=postgresql|mysql"},
		{Name: "password", Label: "Password", Type: "password", Section: conn, ShowWhen: "kind=postgresql|mysql"},
		{Name: "ssl", Label: "Encryption type", Type: "select", Section: conn, ShowWhen: "kind=postgresql|mysql", Options: []string{"NONE", "REQUIRED", "SERVER_ONLY", "SERVER_CLIENT"}, Default: "NONE",
			OptionLabels: map[string]string{"NONE": "None", "REQUIRED": "Basic (encrypted, the certificate not verified)",
				"SERVER_ONLY": "TLS (the source's certificate verified)", "SERVER_CLIENT": "mTLS (the source's certificate verified, a client certificate presented)"}},
		{Name: "caCertificate", Label: "Source CA certificate", Type: "textarea", Section: conn, ShowWhen: "ssl=SERVER_ONLY|SERVER_CLIENT",
			Help: "PEM: the CA that signed the source's certificate."},
		{Name: "clientCertificate", Label: "Client certificate", Type: "textarea", Section: conn, ShowWhen: "ssl=SERVER_CLIENT", Help: "PEM."},
		{Name: "clientKey", Label: "Client private key", Type: "textarea", Section: conn, ShowWhen: "ssl=SERVER_CLIENT", Help: "PEM, unencrypted."},
		{Name: "sshTunnel", Label: "Connect through a forward-SSH tunnel", Type: "checkbox", Section: conn, ShowWhen: "kind=postgresql|mysql",
			Help: "The destination reaches the source through an SSH host that can reach it."},
		{Name: "sshHost", Label: "SSH hostname", Type: "text", Section: conn, ShowWhen: "sshTunnel"},
		{Name: "sshPort", Label: "SSH port", Type: "number", Section: conn, ShowWhen: "sshTunnel", Default: "22"},
		{Name: "sshUser", Label: "SSH username", Type: "text", Section: conn, ShowWhen: "sshTunnel"},
		{Name: "sshPassword", Label: "SSH password", Type: "password", Section: conn, ShowWhen: "sshTunnel", Help: "Or a private key below."},
		{Name: "sshKey", Label: "SSH private key", Type: "textarea", Section: conn, ShowWhen: "sshTunnel", Help: "PEM, unencrypted."},
		{Name: "existingId", Label: "Instance or cluster ID", Type: "text", Section: what, ShowWhen: "kind=cloudsql-existing|alloydb-existing",
			Help: "The existing destination, in the profile's region. It must be standalone and hold no user data; a migration job demotes it."},
		{Name: "existingEngine", Label: "Destination engine", Type: "select", Section: what, ShowWhen: "kind=cloudsql-existing", Options: []string{"mysql", "postgresql"},
			Default: "mysql", OptionLabels: map[string]string{"mysql": "MySQL", "postgresql": "PostgreSQL"}},
		{Name: "sourceId", Label: "Source connection profile", Type: "select", Section: cs, ShowWhen: "kind=cloudsql", Options: srcs, OptionLabels: srcLabels,
			Help: "The destination instance is created as a replica of this source."},
		{Name: "version", Label: "Database version", Type: "text", Section: cs, ShowWhen: "kind=cloudsql|alloydb", Help: "POSTGRES_16, MYSQL_8_4, ..."},
		{Name: "tier", Label: "Machine type", Type: "text", Section: cs, ShowWhen: "kind=cloudsql", Default: "db-custom-1-3840"},
		{Name: "rootPassword", Label: "Password for the root or postgres user", Type: "password", Section: cs, ShowWhen: "kind=cloudsql"},
		{Name: "clusterId", Label: "Cluster ID", Type: "text", Section: adb, ShowWhen: "kind=alloydb", Help: "Defaults to the connection profile ID."},
		{Name: "adbPassword", Label: "Password for the postgres user", Type: "password", Section: adb, ShowWhen: "kind=alloydb"},
		{Name: "network", Label: "Network", Type: "text", Section: adb, ShowWhen: "kind=alloydb", Default: "default"},
		{Name: "primaryId", Label: "Primary instance ID", Type: "text", Section: adb, ShowWhen: "kind=alloydb"},
		{Name: "cpu", Label: "CPU count", Type: "number", Section: adb, ShowWhen: "kind=alloydb", Default: "2"},
	}
}

func (p dmsProfilesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a connection profile")
	}
	parent := "projects/" + project + "/locations/" + strings.TrimSpace(v["region"])
	id := strings.TrimSpace(v["profileId"])
	pr := &dmspb.ConnectionProfile{DisplayName: v["displayName"]}
	port, _ := strconv.Atoi(strings.TrimSpace(v["port"]))
	ssl := (*dmspb.SslConfig)(nil)
	switch v["ssl"] {
	case "REQUIRED":
		ssl = &dmspb.SslConfig{Type: dmspb.SslConfig_SslType(datamigration.SSLRequired)}
	case "SERVER_ONLY":
		ssl = &dmspb.SslConfig{Type: dmspb.SslConfig_SERVER_ONLY, CaCertificate: v["caCertificate"]}
	case "SERVER_CLIENT":
		ssl = &dmspb.SslConfig{Type: dmspb.SslConfig_SERVER_CLIENT, CaCertificate: v["caCertificate"],
			ClientCertificate: v["clientCertificate"], ClientKey: v["clientKey"]}
	}
	tunnel := func(m interface{ ProtoReflect() protoreflect.Message }) error {
		if v["sshTunnel"] != "true" && v["sshTunnel"] != "on" {
			return nil
		}
		sp, _ := strconv.Atoi(strings.TrimSpace(v["sshPort"]))
		if sp == 0 {
			sp = 22
		}
		return datamigration.SetForwardSSH(m, strings.TrimSpace(v["sshHost"]), sp, strings.TrimSpace(v["sshUser"]), v["sshPassword"], v["sshKey"])
	}
	switch v["kind"] {
	case "mysql":
		if port == 0 {
			port = 3306
		}
		m := &dmspb.MySqlConnectionProfile{Host: strings.TrimSpace(v["host"]), Port: int32(port),
			Username: v["username"], Password: v["password"], Ssl: ssl}
		if err := tunnel(m); err != nil {
			return "", err
		}
		pr.ConnectionProfile = &dmspb.ConnectionProfile_Mysql{Mysql: m}
	case "postgresql":
		if port == 0 {
			port = 5432
		}
		m := &dmspb.PostgreSqlConnectionProfile{Host: strings.TrimSpace(v["host"]), Port: int32(port),
			Username: v["username"], Password: v["password"], Ssl: ssl}
		if err := tunnel(m); err != nil {
			return "", err
		}
		pr.ConnectionProfile = &dmspb.ConnectionProfile_Postgresql{Postgresql: m}
	case "cloudsql-existing":
		id := strings.TrimSpace(v["existingId"])
		if v["existingEngine"] == "postgresql" {
			pr.ConnectionProfile = &dmspb.ConnectionProfile_Postgresql{Postgresql: &dmspb.PostgreSqlConnectionProfile{CloudSqlId: id}}
		} else {
			pr.ConnectionProfile = &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{CloudSqlId: id}}
		}
		datamigration.SetExistingRole(pr)
	case "alloydb-existing":
		pr.ConnectionProfile = &dmspb.ConnectionProfile_Alloydb{Alloydb: &dmspb.AlloyDbConnectionProfile{ClusterId: strings.TrimSpace(v["existingId"])}}
	case "cloudsql":
		st := &dmspb.CloudSqlSettings{SourceId: v["sourceId"], Tier: strings.TrimSpace(v["tier"]), RootPassword: v["rootPassword"]}
		datamigration.SetCloudSQLVersionName(st, strings.TrimSpace(v["version"]))
		pr.ConnectionProfile = &dmspb.ConnectionProfile_Cloudsql{Cloudsql: &dmspb.CloudSqlConnectionProfile{Settings: st}}
	case "alloydb":
		cluster := strings.TrimSpace(v["clusterId"])
		if cluster == "" {
			cluster = id
		}
		cpu, _ := strconv.Atoi(strings.TrimSpace(v["cpu"]))
		network := strings.TrimSpace(v["network"])
		if network != "" && !strings.HasPrefix(network, "projects/") {
			network = "projects/" + project + "/global/networks/" + network
		}
		st := &dmspb.AlloyDbSettings{InitialUser: &dmspb.AlloyDbSettings_UserPassword{User: "postgres", Password: v["adbPassword"]}, VpcNetwork: network,
			PrimaryInstanceSettings: &dmspb.AlloyDbSettings_PrimaryInstanceSettings{Id: strings.TrimSpace(v["primaryId"]),
				MachineConfig: &dmspb.AlloyDbSettings_PrimaryInstanceSettings_MachineConfig{CpuCount: int32(cpu)}}}
		datamigration.SetAlloyDBVersion(st, strings.TrimSpace(v["version"]))
		pr.ConnectionProfile = &dmspb.ConnectionProfile_Alloydb{Alloydb: &dmspb.AlloyDbConnectionProfile{ClusterId: cluster, Settings: st}}
	default:
		return "", fmt.Errorf("choose a database engine")
	}
	if err := dmsWait(s)(s.CreateConnectionProfile(ctx, &dmspb.CreateConnectionProfileRequest{Parent: parent, ConnectionProfileId: id, ConnectionProfile: pr})); err != nil {
		return "", err
	}
	return parent + "/connectionProfiles/" + id, nil
}

func (p dmsProfilesProvider) profile(ctx context.Context, project, name string) (*datamigration.Server, *dmspb.ConnectionProfile, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, nil, err
	}
	pr, err := s.GetConnectionProfile(ctx, &dmspb.GetConnectionProfileRequest{Name: name})
	return s, pr, err
}

func (p dmsProfilesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	_, pr, err := p.profile(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the connection profile: " + consoleErr(err)}, nil
	}
	props := []console.Property{{Label: "Connection profile ID", Value: lastSegment(pr.GetName())}, {Label: "Connection profile name", Value: kmsOrDash(pr.GetDisplayName())},
		{Label: "Database engine", Value: dmsEngine(pr)}, {Label: "Status", Value: pr.GetState().String()}, {Label: "Region", Value: kmsLocation(pr.GetName())},
		{Label: "Created", Value: kmsTime(pr.GetCreateTime())}}
	if e := pr.GetError(); e != nil {
		props = append(props, console.Property{Label: "Error", Value: e.GetMessage()})
	}
	var conn []console.Property
	sslOf := func(c *dmspb.SslConfig) string {
		switch c.GetType() {
		case dmspb.SslConfig_SERVER_ONLY:
			return "TLS (the source's certificate verified)"
		case dmspb.SslConfig_SERVER_CLIENT:
			return "mTLS"
		case dmspb.SslConfig_SslType(datamigration.SSLRequired):
			return "Basic"
		}
		return "None"
	}
	sshOf := func(m interface{ ProtoReflect() protoreflect.Message }) []console.Property {
		hp, user := datamigration.ForwardSSH(m)
		if hp == "" {
			return nil
		}
		return []console.Property{{Label: "Forward-SSH tunnel", Value: user + "@" + hp}}
	}
	switch {
	case func() bool { _, ok := datamigration.ExistingDestination(pr); return ok }():
		id, _ := datamigration.ExistingDestination(pr)
		conn = []console.Property{{Label: "Existing destination", Value: id}, {Label: "Database engine", Value: dmsEngine(pr)}}
	case pr.GetMysql() != nil:
		m := pr.GetMysql()
		conn = append([]console.Property{{Label: "Hostname or IP address", Value: m.GetHost()}, {Label: "Port", Value: strconv.Itoa(int(m.GetPort()))}, {Label: "Username", Value: m.GetUsername()},
			{Label: "Encryption type", Value: sslOf(m.GetSsl())}}, sshOf(m)...)
	case pr.GetPostgresql() != nil:
		m := pr.GetPostgresql()
		conn = append([]console.Property{{Label: "Hostname or IP address", Value: m.GetHost()}, {Label: "Port", Value: strconv.Itoa(int(m.GetPort()))}, {Label: "Username", Value: m.GetUsername()},
			{Label: "Encryption type", Value: sslOf(m.GetSsl())}}, sshOf(m)...)
	case pr.GetCloudsql() != nil:
		c := pr.GetCloudsql()
		conn = []console.Property{{Label: "Cloud SQL instance", Value: lastSegment(pr.GetName())}, {Label: "Source connection profile", Value: c.GetSettings().GetSourceId()},
			{Label: "Machine type", Value: c.GetSettings().GetTier()}, {Label: "Public IP address", Value: kmsOrDash(c.GetPublicIp())},
			{Label: "Private IP address", Value: kmsOrDash(c.GetPrivateIp())}}
	case pr.GetAlloydb() != nil:
		a := pr.GetAlloydb()
		conn = []console.Property{{Label: "Cluster ID", Value: a.GetClusterId()}, {Label: "Primary instance ID", Value: a.GetSettings().GetPrimaryInstanceSettings().GetId()},
			{Label: "Network", Value: kmsOrDash(a.GetSettings().GetVpcNetwork())}}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Database engine", Value: dmsEngine(pr)}, {Label: "Status", Value: pr.GetState().String()}},
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
			{Heading: "Connection profile", Properties: props}, {Heading: "Connection", Properties: conn}}}},
	}, nil
}

func (p dmsProfilesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	_, pr, err := p.profile(ctx, project, path[0])
	if err != nil {
		return nil
	}
	out := []console.Action{}
	if dmsIsSource(pr) {
		host, port, user := pr.GetPostgresql().GetHost(), pr.GetPostgresql().GetPort(), pr.GetPostgresql().GetUsername()
		if m := pr.GetMysql(); m != nil {
			host, port, user = m.GetHost(), m.GetPort(), m.GetUsername()
		}
		out = append(out, console.Action{ID: "edit", Label: "Edit", Fields: []console.Field{
			{Name: "displayName", Label: "Connection profile name", Type: "text", Default: pr.GetDisplayName()},
			{Name: "host", Label: "Hostname or IP address", Type: "text", Default: host},
			{Name: "port", Label: "Port", Type: "number", Default: strconv.Itoa(int(port))},
			{Name: "username", Label: "Username", Type: "text", Default: user},
			{Name: "password", Label: "Password", Type: "password", Help: "Leave empty to keep the password."}}})
	} else {
		out = append(out, console.Action{ID: "edit", Label: "Edit", Fields: []console.Field{
			{Name: "displayName", Label: "Connection profile name", Type: "text", Default: pr.GetDisplayName()}}})
	}
	return append(out, console.Action{ID: "delete", Label: "Delete", Destructive: true, Leaves: true,
		Confirm: "A profile a migration job uses cannot be deleted."})
}

func (p dmsProfilesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no connection profile")
	}
	s, pr, err := p.profile(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "edit":
		in := &dmspb.ConnectionProfile{Name: pr.GetName(), DisplayName: v["displayName"]}
		paths := []string{"display_name"}
		if dmsIsSource(pr) {
			port, _ := strconv.Atoi(strings.TrimSpace(v["port"]))
			prefix := "postgresql."
			if pr.GetMysql() != nil {
				prefix = "mysql."
				in.ConnectionProfile = &dmspb.ConnectionProfile_Mysql{Mysql: &dmspb.MySqlConnectionProfile{Host: strings.TrimSpace(v["host"]), Port: int32(port),
					Username: v["username"], Password: v["password"]}}
			} else {
				in.ConnectionProfile = &dmspb.ConnectionProfile_Postgresql{Postgresql: &dmspb.PostgreSqlConnectionProfile{Host: strings.TrimSpace(v["host"]), Port: int32(port),
					Username: v["username"], Password: v["password"]}}
			}
			paths = append(paths, prefix+"host", prefix+"port", prefix+"username")
			if v["password"] != "" {
				paths = append(paths, prefix+"password")
			}
		}
		return dmsWait(s)(s.UpdateConnectionProfile(ctx, &dmspb.UpdateConnectionProfileRequest{ConnectionProfile: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}))
	case "delete":
		return dmsWait(s)(s.DeleteConnectionProfile(ctx, &dmspb.DeleteConnectionProfileRequest{Name: pr.GetName()}))
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- Private connectivity configurations ----

type dmsPrivateProvider struct{ svc *dataMigrationService }

func (dmsPrivateProvider) ID() string    { return "datamigration-private" }
func (dmsPrivateProvider) Title() string { return "Private connectivity configurations" }

func (p dmsPrivateProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Display name", "VPC network", "IP range", "Region", "Status", "Created"},
		NameColumn: "Configuration ID", Noun: "private connectivity configurations"}
	if project == "" {
		l.Prompt = "Database Migration Service lists a project's private connectivity configurations. Choose one in the toolbar."
		return l, nil
	}
	for _, loc := range datamigration.Locations() {
		resp, err := s.ListPrivateConnections(ctx, &dmspb.ListPrivateConnectionsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return console.Listing{}, err
		}
		for _, pc := range resp.GetPrivateConnections() {
			l.Items = append(l.Items, console.Resource{Name: pc.GetName(), Status: pc.GetState().String(), Fields: map[string]string{
				"Display name": kmsOrDash(pc.GetDisplayName()), "VPC network": pc.GetVpcPeeringConfig().GetVpcName(), "IP range": pc.GetVpcPeeringConfig().GetSubnet(),
				"Region": kmsLocation(pc.GetName()), "Status": pc.GetState().String(), "Created": kmsTime(pc.GetCreateTime())}})
		}
	}
	l.Total = len(l.Items)
	return l, nil
}

func (dmsPrivateProvider) CreateForm() (string, []console.Field) {
	return "Create configuration", []console.Field{
		{Name: "displayName", Label: "Configuration name", Type: "text"},
		{Name: "id", Label: "Configuration ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
		{Name: "region", Label: "Region", Type: "select", Required: true, Options: datamigration.Locations(), Default: "us-central1"},
		{Name: "vpc", Label: "VPC network", Type: "text", Required: true, Help: "projects/PROJECT/global/networks/NETWORK"},
		{Name: "subnet", Label: "Allocated IP range", Type: "text", Required: true, Help: "A free /29 range, such as 10.20.0.0/29."},
	}
}

func (p dmsPrivateProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a private connectivity configuration")
	}
	parent := "projects/" + project + "/locations/" + strings.TrimSpace(v["region"])
	id := strings.TrimSpace(v["id"])
	pc := &dmspb.PrivateConnection{DisplayName: v["displayName"], Connectivity: &dmspb.PrivateConnection_VpcPeeringConfig{
		VpcPeeringConfig: &dmspb.VpcPeeringConfig{VpcName: strings.TrimSpace(v["vpc"]), Subnet: strings.TrimSpace(v["subnet"])}}}
	if err := dmsWait(s)(s.CreatePrivateConnection(ctx, &dmspb.CreatePrivateConnectionRequest{Parent: parent, PrivateConnectionId: id, PrivateConnection: pc})); err != nil {
		return "", err
	}
	return parent + "/privateConnections/" + id, nil
}

func (p dmsPrivateProvider) Delete(ctx context.Context, project, name string) error {
	s, err := dmsAPI(p.svc)
	if err != nil {
		return err
	}
	if err := kmsOwned(project, name); err != nil {
		return err
	}
	return dmsWait(s)(s.DeletePrivateConnection(ctx, &dmspb.DeletePrivateConnectionRequest{Name: name}))
}

// ---- Conversion workspaces ----

// dmsConversionProvider is Google's Conversion workspaces page, which
// CloudBurrow does not serve: the page says so rather than list nothing.
type dmsConversionProvider struct{}

func (dmsConversionProvider) ID() string    { return "datamigration-conversion" }
func (dmsConversionProvider) Title() string { return "Conversion workspaces" }

func (dmsConversionProvider) List(context.Context, string) (console.Listing, error) {
	return console.Listing{Columns: []string{"Name"}, Noun: "conversion workspaces",
		Unavailable: "Conversion workspaces convert an Oracle or SQL Server schema to PostgreSQL with Google's conversion engine, which CloudBurrow does not have " +
			"(the API answers UNIMPLEMENTED; docs/gaps.md). MySQL and PostgreSQL migrations need none: create them on Migration jobs."}, nil
}
