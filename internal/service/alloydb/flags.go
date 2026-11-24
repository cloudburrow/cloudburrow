package alloydb

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var kmsKeyRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)

// flag is one supported database flag: its type, bounds or values, and
// whether a change restarts the server.
type flag struct {
	name     string
	typ      alloydbpb.SupportedDatabaseFlag_ValueType
	min, max int64
	values   []string
	restart  bool
	multi    bool
}

func intFlag(name string, min, max int64, restart bool) flag {
	return flag{name: name, typ: alloydbpb.SupportedDatabaseFlag_INTEGER, min: min, max: max, restart: restart}
}

func enumFlag(name string, restart bool, values ...string) flag {
	return flag{name: name, typ: alloydbpb.SupportedDatabaseFlag_STRING, values: values, restart: restart}
}

func strFlag(name string, restart, multi bool) flag {
	return flag{name: name, typ: alloydbpb.SupportedDatabaseFlag_STRING, restart: restart, multi: multi}
}

var onOff = []string{"on", "off"}

// supportedFlags are the database flags of AlloyDB's supported-flags page
// (docs.cloud.google.com/alloydb/docs/reference/database-flags, read
// 2026-11-23) that this instance knows: the PostgreSQL settings the
// server applies, and AlloyDB's own (alloydb.*, google_columnar_engine.*,
// ...), which PostgreSQL keeps as placeholder settings with no effect.
// UNVERIFIED: the bounds the page gives in other units are PostgreSQL's.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
var supportedFlags = []flag{
	enumFlag("alloydb.enable_pgaudit", true, onOff...),
	enumFlag("alloydb.enable_pg_cron", true, onOff...),
	enumFlag("alloydb.enable_pg_hint_plan", true, onOff...),
	enumFlag("alloydb.enable_auto_explain", true, onOff...),
	enumFlag("alloydb.iam_authentication", false, onOff...),
	enumFlag("alloydb.logical_decoding", true, onOff...),
	enumFlag("alloydb.enable_pglogical", true, onOff...),
	enumFlag("alloydb.promote_cache_to_main_memory", true, onOff...),
	enumFlag("google_columnar_engine.enabled", true, onOff...),
	intFlag("google_columnar_engine.memory_size_in_mb", 128, 262144, true),
	enumFlag("google_columnar_engine.enable_auto_columnarization", false, onOff...),
	enumFlag("google_db_advisor.enabled", true, onOff...),
	enumFlag("google_ml_integration.enable_model_support", false, onOff...),
	enumFlag("password.enforce_complexity", false, onOff...),
	intFlag("password.min_pass_length", 0, 2147483647, false),
	enumFlag("autovacuum", false, onOff...),
	intFlag("autovacuum_max_workers", 1, 262143, true),
	intFlag("autovacuum_naptime", 1, 2147483, false),
	intFlag("checkpoint_timeout", 30, 86400, false),
	strFlag("cron.database_name", true, false),
	intFlag("deadlock_timeout", 1, 2147483647, false),
	intFlag("default_statistics_target", 1, 10000, false),
	intFlag("effective_cache_size", 1, 2147483647, false),
	intFlag("idle_in_transaction_session_timeout", 0, 2147483647, false),
	intFlag("lock_timeout", 0, 2147483647, false),
	enumFlag("log_connections", false, onOff...),
	enumFlag("log_disconnections", false, onOff...),
	enumFlag("log_lock_waits", false, onOff...),
	intFlag("log_min_duration_statement", -1, 2147483647, false),
	enumFlag("log_min_messages", false, "debug5", "debug4", "debug3", "debug2", "debug1", "info", "notice", "warning", "error", "log", "fatal", "panic"),
	enumFlag("log_statement", false, "none", "ddl", "mod", "all"),
	intFlag("log_temp_files", -1, 2147483647, false),
	intFlag("maintenance_work_mem", 1024, 2147483647, false),
	intFlag("max_connections", 14, 262143, true),
	intFlag("max_locks_per_transaction", 10, 2147483647, true),
	intFlag("max_parallel_workers", 0, 1024, false),
	intFlag("max_parallel_workers_per_gather", 0, 1024, false),
	intFlag("max_prepared_transactions", 0, 262143, true),
	intFlag("max_replication_slots", 0, 262143, true),
	intFlag("max_wal_senders", 0, 262143, true),
	intFlag("max_worker_processes", 0, 262143, true),
	strFlag("pgaudit.log", false, true),
	enumFlag("pgaudit.log_parameter", false, onOff...),
	intFlag("random_page_cost", 0, 2147483647, false),
	strFlag("search_path", false, false),
	strFlag("shared_preload_libraries", true, true),
	intFlag("statement_timeout", 0, 2147483647, false),
	enumFlag("synchronous_commit", false, "on", "off", "local", "remote_write", "remote_apply"),
	strFlag("timezone", false, false),
	intFlag("temp_file_limit", -1, 2147483647, false),
	enumFlag("track_activity_query_size", true, "1024", "2048", "4096", "8192", "16384", "32768", "65536", "1048576"),
	enumFlag("track_io_timing", false, onOff...),
	intFlag("work_mem", 64, 2147483647, false),
}

func flagByName(name string) (flag, bool) {
	for _, f := range supportedFlags {
		if f.name == name {
			return f, true
		}
	}
	return flag{}, false
}

// checkFlags refuses a flag this instance does not know, and a value its
// type or bounds do not allow.
func checkFlags(flags map[string]string, _ alloydbpb.DatabaseVersion) error {
	for name, v := range flags {
		f, ok := flagByName(name)
		if !ok {
			return apierror.InvalidArgument("database flag %q is not supported", name)
		}
		switch {
		case f.typ == alloydbpb.SupportedDatabaseFlag_INTEGER:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < f.min || n > f.max {
				return apierror.InvalidArgument("database flag %s must be an integer from %d to %d, not %q", name, f.min, f.max, v)
			}
		case len(f.values) > 0:
			okv := false
			for _, a := range f.values {
				if strings.EqualFold(a, v) {
					okv = true
				}
			}
			if !okv {
				return apierror.InvalidArgument("database flag %s must be one of %s, not %q", name, strings.Join(f.values, ", "), v)
			}
		}
	}
	return nil
}

func (s *Server) ListSupportedDatabaseFlags(_ context.Context, req *alloydbpb.ListSupportedDatabaseFlagsRequest) (*alloydbpb.ListSupportedDatabaseFlagsResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent(), false); err != nil {
		return nil, err
	}
	var all []*alloydbpb.SupportedDatabaseFlag
	versions := []alloydbpb.DatabaseVersion{alloydbpb.DatabaseVersion_POSTGRES_14, alloydbpb.DatabaseVersion_POSTGRES_15,
		alloydbpb.DatabaseVersion_POSTGRES_16, alloydbpb.DatabaseVersion_POSTGRES_17}
	if req.GetScope() == alloydbpb.SupportedDatabaseFlag_CONNECTION_POOL {
		// Managed connection pooling's flags (pooling.go), with Google's
		// ranges. UNVERIFIED: whether Google marks them as needing a
		// restart; here a change restarts the instance's pooler with it.
		// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/configure-managed-connection-pooling.)
		names := make([]string, 0, len(poolFlags))
		for n := range poolFlags {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			f := poolFlags[n]
			out := &alloydbpb.SupportedDatabaseFlag{Name: req.GetParent() + "/flags/" + n, FlagName: n,
				ValueType: alloydbpb.SupportedDatabaseFlag_STRING, AcceptsMultipleValues: f.list, SupportedDbVersions: versions,
				Scope: alloydbpb.SupportedDatabaseFlag_CONNECTION_POOL}
			switch {
			case len(f.values) > 0:
				out.Restrictions = &alloydbpb.SupportedDatabaseFlag_StringRestrictions_{StringRestrictions: &alloydbpb.SupportedDatabaseFlag_StringRestrictions{
					AllowedValues: f.values}}
			case !f.list:
				out.ValueType = alloydbpb.SupportedDatabaseFlag_INTEGER
				out.Restrictions = &alloydbpb.SupportedDatabaseFlag_IntegerRestrictions_{IntegerRestrictions: &alloydbpb.SupportedDatabaseFlag_IntegerRestrictions{
					MinValue: wrapperspb.Int64(f.min), MaxValue: wrapperspb.Int64(f.max)}}
			}
			all = append(all, out)
		}
		out, next, err := listPage("alloydb-pool-flags:"+req.GetParent(), "", "", all, req.GetPageToken(), req.GetPageSize())
		if err != nil {
			return nil, err
		}
		return &alloydbpb.ListSupportedDatabaseFlagsResponse{SupportedDatabaseFlags: out, NextPageToken: next}, nil
	}
	for _, f := range supportedFlags {
		out := &alloydbpb.SupportedDatabaseFlag{Name: req.GetParent() + "/flags/" + f.name, FlagName: f.name, ValueType: f.typ,
			AcceptsMultipleValues: f.multi, SupportedDbVersions: versions, RequiresDbRestart: f.restart,
			Scope: alloydbpb.SupportedDatabaseFlag_DATABASE}
		switch {
		case f.typ == alloydbpb.SupportedDatabaseFlag_INTEGER:
			out.Restrictions = &alloydbpb.SupportedDatabaseFlag_IntegerRestrictions_{IntegerRestrictions: &alloydbpb.SupportedDatabaseFlag_IntegerRestrictions{
				MinValue: wrapperspb.Int64(f.min), MaxValue: wrapperspb.Int64(f.max)}}
		case len(f.values) > 0:
			out.Restrictions = &alloydbpb.SupportedDatabaseFlag_StringRestrictions_{StringRestrictions: &alloydbpb.SupportedDatabaseFlag_StringRestrictions{
				AllowedValues: f.values}}
		}
		all = append(all, out)
	}
	out, next, err := listPage("alloydb-flags:"+req.GetParent(), "", "", all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListSupportedDatabaseFlagsResponse{SupportedDatabaseFlags: out, NextPageToken: next}, nil
}
