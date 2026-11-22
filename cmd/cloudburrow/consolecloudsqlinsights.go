package main

// Cloud SQL's Query insights and System insights pages (#2141), read from
// the instance's own server. Google's Query insights shows the top
// queries with their Database, Load by total time, Avg execution time
// (ms), Times called and Avg rows returned, filtered by Database and User
// (https://docs.cloud.google.com/sql/docs/postgres/using-query-insights,
// read 2026-11-22); its System insights summary cards are CPU
// utilization, Peak connections, Transaction ID utilization, Disk
// utilization and Log errors
// (https://docs.cloud.google.com/sql/docs/postgres/use-system-insights,
// read 2026-11-22). Google charts them over a time range from Cloud
// Monitoring; here they are the server's totals since it started.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlInsightsEnabled says Query insights is on for in. The instances `up`
// runs are not edited here, so their queries are always shown.
func sqlInsightsEnabled(in *sqladminapi.DatabaseInstance) bool {
	if upSQLInstance(in.Name) {
		return true
	}
	return in.Settings != nil && in.Settings.InsightsConfig != nil && in.Settings.InsightsConfig.QueryInsightsEnabled
}

// enableQueryInsights is the Query insights page's Enable, on an instance
// where it is off.
func enableQueryInsights(in *sqladminapi.DatabaseInstance) []console.Action {
	if sqlInsightsEnabled(in) || in.InstanceType == "ON_PREMISES_INSTANCE" || in.InstanceType == "READ_REPLICA_INSTANCE" {
		return nil
	}
	return []console.Action{{ID: "enable-query-insights", Label: "Enable Query insights", Title: "Enable Query insights",
		Submit: "Enable", Fields: sqlInsightsFields("")}}
}

// sqlInsightsFields are Google's Query insights options, on the create
// page and in Enable: Store client IP addresses, Store application tags,
// Customize query lengths (1024 on Enterprise) and the maximum sampling
// rate (5 on Enterprise).
func sqlInsightsFields(section string) []console.Field {
	show := map[string]string{"queryInsightsEnabled": "true"}
	if section == "" {
		show = nil
	}
	return []console.Field{
		{Name: "recordClientAddress", Label: "Store client IP addresses", Type: "checkbox", Section: section, VisibleWhen: show,
			Help: "Recorded on the instance (insightsConfig.recordClientAddress)."},
		{Name: "recordApplicationTags", Label: "Store application tags", Type: "checkbox", Section: section, VisibleWhen: show,
			Help: "Recorded on the instance (insightsConfig.recordApplicationTags)."},
		{Name: "queryStringLength", Label: "Customize query lengths", Type: "number", Default: "1024", Section: section, VisibleWhen: show,
			Help: "256 to 4500 bytes (insightsConfig.queryStringLength)."},
		{Name: "queryPlansPerMinute", Label: "Set the maximum sampling rate", Type: "number", Default: "5", Section: section, VisibleWhen: show,
			Help: "0 to 20 query plans a minute (insightsConfig.queryPlansPerMinute). No query plans are sampled locally."},
	}
}

// sqlInsightsConfig is the insightsConfig v's options make.
func sqlInsightsConfig(v map[string]string) (*sqladminapi.InsightsConfig, error) {
	c := &sqladminapi.InsightsConfig{QueryInsightsEnabled: true, RecordClientAddress: v["recordClientAddress"] == "true",
		RecordApplicationTags: v["recordApplicationTags"] == "true",
		ForceSendFields:       []string{"QueryInsightsEnabled", "RecordClientAddress", "RecordApplicationTags"}}
	if s := strings.TrimSpace(v["queryStringLength"]); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 256 || n > 4500 {
			return nil, fmt.Errorf("query length %q must be 256 to 4500", s)
		}
		c.QueryStringLength = n
	}
	if s := strings.TrimSpace(v["queryPlansPerMinute"]); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || n > 20 {
			return nil, fmt.Errorf("sampling rate %q must be 0 to 20", s)
		}
		c.QueryPlansPerMinute = n
		c.ForceSendFields = append(c.ForceSendFields, "QueryPlansPerMinute")
	}
	return c, nil
}

// sqlQueryInsightsSection is the Query insights page: the top queries by
// total execution time, each one's share of the load.
func sqlQueryInsightsSection(ctx context.Context, srv *sqladmin.Server, metrics monitoringAdmin, project string, in *sqladminapi.DatabaseInstance) console.Section {
	sec := console.Section{ID: "query-insights", Label: "Query insights", Actions: enableQueryInsights(in)}
	l := console.Listing{TableTitle: "Top queries", NameColumn: "Query", Noun: "queries",
		Columns: []string{"Database", "User", "Load by total time", "Avg execution time (ms)", "Times called", "Avg rows returned"},
		Note: "Each normalized query's totals since the server started, from " + sqlStatsSource(in) +
			"; Google's page totals them over a chosen time range."}
	if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
		// MySQL's statement digests do not keep the user.
		l.Columns = append(l.Columns[:1:1], l.Columns[2:]...)
	}
	switch {
	case in.InstanceType == "ON_PREMISES_INSTANCE":
		l.Unavailable = "An external server's queries are not read."
	case !sqlInsightsEnabled(in):
		l.Prompt = "Query insights is not enabled for this instance. Enable it to see the instance's top queries."
	case srv == nil:
		l.Unavailable = "the Cloud SQL Admin API is not running in this process"
	case in.State != "RUNNABLE":
		l.Unavailable = "The instance is " + in.State + ": its server is not running."
	default:
		stats, err := srv.QueryInsights(ctx, project, in.Name)
		if err != nil {
			l.Unavailable = "cannot read the server's statistics: " + err.Error()
			break
		}
		var total float64
		for _, q := range stats {
			total += q.TotalMs
		}
		sort.SliceStable(stats, func(i, j int) bool { return stats[i].TotalMs > stats[j].TotalMs })
		for i, q := range stats {
			load := "0%"
			if total > 0 {
				load = fmt.Sprintf("%.1f%%", q.TotalMs*100/total)
			}
			rows := "0"
			if q.Calls > 0 {
				rows = strconv.FormatFloat(float64(q.Rows)/float64(q.Calls), 'f', -1, 64)
				if len(rows) > 8 {
					rows = fmt.Sprintf("%.2f", float64(q.Rows)/float64(q.Calls))
				}
			}
			l.Items = append(l.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{
				"Query": q.Query, "Database": kmsOrDash(q.Database), "User": kmsOrDash(q.User), "Load by total time": load,
				"Avg execution time (ms)": fmt.Sprintf("%.3f", q.MeanMs), "Times called": strconv.FormatInt(q.Calls, 10),
				"Avg rows returned": rows}})
		}
		l.Total = len(l.Items)
	}
	sec.Listing = l
	// The Database load chart over the queries (#2141), PostgreSQL's,
	// from the server's own statement and wait figures.
	if !strings.HasPrefix(in.DatabaseVersion, "MYSQL_") && !strings.HasPrefix(in.DatabaseVersion, "SQLSERVER_") &&
		in.InstanceType != "ON_PREMISES_INSTANCE" && sqlInsightsEnabled(in) {
		sec.SeriesTitle = "Database load - all top queries"
		sec.Series = sqlLoadSeries(ctx, metrics, project, in)
		sec.Listing.Note += " The Database load chart is the time the server's statements spent each second, read every minute: their execution time " +
			"(pg_stat_statements), its IO wait (counted with the track_io_timing flag on) and its lock wait (backends seen waiting on a lock in pg_stat_activity at each reading), " +
			"from the postgresql/insights/aggregate metrics in Cloud Monitoring."
	}
	return sec
}

func sqlStatsSource(in *sqladminapi.DatabaseInstance) string {
	if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
		return "performance_schema's statement digests"
	}
	return "pg_stat_statements"
}

// sqlSystemInsightsSection is the System insights page's summary cards.
func sqlSystemInsightsSection(ctx context.Context, srv *sqladmin.Server, metrics monitoringAdmin, project string, in *sqladminapi.DatabaseInstance) console.Section {
	sec := console.Section{ID: "system-insights", Label: "System insights", Kind: console.KindProperties,
		Note: "The summary cards are the server's figures now (CPU over the last day); the charts are Cloud Monitoring's cloudsql.googleapis.com series over the last hour, read from the running server."}
	notLocal := func(label, why string) console.Property {
		return console.Property{Label: label, Value: "Not available locally: " + why}
	}
	cpu := notLocal("CPU utilization - P99 | P50", "no cpu/utilization readings in Cloud Monitoring yet: the first is two samples (two minutes) after the server starts.")
	if v, ok := sqlCPUPercentiles(ctx, metrics, project, in.Name); ok {
		cpu = console.Property{Label: "CPU utilization - P99 | P50", Value: v}
	}
	logs := console.Property{Label: "Log errors", Value: "The server's log lines are on Logs.", Link: "/cloudsql-instances/" + in.Name + "?tab=logs"}
	switch {
	case in.InstanceType == "ON_PREMISES_INSTANCE":
		sec.Unavailable = "An external server's figures are not read."
		return sec
	case srv == nil:
		sec.Unavailable = "the Cloud SQL Admin API is not running in this process"
		return sec
	case in.State != "RUNNABLE":
		sec.Unavailable = "The instance is " + in.State + ": its server is not running."
		return sec
	}
	st, err := srv.SystemInsights(ctx, project, in.Name)
	if err != nil {
		sec.Unavailable = "cannot read the server's figures: " + err.Error()
		return sec
	}
	conns := console.Property{Label: "Peak connections", Value: fmt.Sprintf("%d of %d (%d open now)", st.PeakConnections, st.MaxConnections, st.Connections)}
	disk := console.Property{Label: "Disk utilization", Value: humanBytes(st.DataBytes)}
	if in.Settings != nil && in.Settings.DataDiskSizeGb > 0 {
		capacity := in.Settings.DataDiskSizeGb << 30
		disk.Value = fmt.Sprintf("%.2f%% (%s of %d GB)", float64(st.DataBytes)*100/float64(capacity), humanBytes(st.DataBytes), in.Settings.DataDiskSizeGb)
	}
	props := []console.Property{cpu, conns}
	if st.TransactionIDUtilization >= 0 {
		props = append(props, console.Property{Label: "Transaction ID utilization", Value: fmt.Sprintf("%.4f%%", st.TransactionIDUtilization)})
	}
	props = append(props, disk, logs)
	sec.Groups = []console.PropertyGroup{{Heading: "Summary", Properties: props}}
	if series, why := sqlCharts(ctx, metrics, project, in, sqlSystemCharts); why == "" {
		sec.Series = series
	}
	return sec
}

// sqlInsightsLayout puts the two insights pages in place of the layout's.
func (p cloudSQLInstancesProvider) sqlInsightsLayout(ctx context.Context, project string, in *sqladminapi.DatabaseInstance, d console.Detail) console.Detail {
	for i, s := range d.Sections {
		switch s.ID {
		case "query-insights":
			d.Sections[i] = sqlQueryInsightsSection(ctx, p.srv, p.metrics, project, in)
		case "system-insights":
			d.Sections[i] = sqlSystemInsightsSection(ctx, p.srv, p.metrics, project, in)
		}
	}
	return d
}
