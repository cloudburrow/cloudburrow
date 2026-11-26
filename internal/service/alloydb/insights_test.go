package alloydb

import (
	"testing"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
)

func TestStatementOp2251(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT $1": "SELECT", "  insert into t values ($1)": "INSERT", "/* app */ UPDATE t SET a = $1": "UPDATE",
		"DELETE FROM t": "DELETE", "MERGE INTO t": "MERGE", "CREATE TABLE t (a int)": "UTILITY", "BEGIN": "UTILITY",
		"WITH x AS (SELECT 1) SELECT * FROM x": "UNKNOWN",
	} {
		if got := statementOp(q); got != want {
			t.Errorf("statementOp(%q) = %s, want %s", q, got, want)
		}
	}
}

func TestCheckInsightsAndMaintenance2251(t *testing.T) {
	for _, c := range []struct {
		cfg *alloydbpb.Instance_QueryInsightsInstanceConfig
		ok  bool
	}{
		{nil, true}, {&alloydbpb.Instance_QueryInsightsInstanceConfig{QueryStringLength: 1024, QueryPlansPerMinute: proto.Uint32(5)}, true},
		{&alloydbpb.Instance_QueryInsightsInstanceConfig{QueryStringLength: 255}, false},
		{&alloydbpb.Instance_QueryInsightsInstanceConfig{QueryStringLength: 4501}, false},
		{&alloydbpb.Instance_QueryInsightsInstanceConfig{QueryPlansPerMinute: proto.Uint32(21)}, false},
	} {
		if err := checkInsights(c.cfg); (err == nil) != c.ok {
			t.Errorf("checkInsights(%v) = %v", c.cfg, err)
		}
	}
	w := func(d dayofweek.DayOfWeek, h int32) *alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow {
		return &alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{Day: d, StartTime: &timeofday.TimeOfDay{Hours: h}}
	}
	for _, c := range []struct {
		p  *alloydbpb.MaintenanceUpdatePolicy
		ok bool
	}{
		{nil, true},
		{&alloydbpb.MaintenanceUpdatePolicy{MaintenanceWindows: []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{w(dayofweek.DayOfWeek_TUESDAY, 23)}}, true},
		{&alloydbpb.MaintenanceUpdatePolicy{MaintenanceWindows: []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{w(dayofweek.DayOfWeek_TUESDAY, 24)}}, false},
		{&alloydbpb.MaintenanceUpdatePolicy{MaintenanceWindows: []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{w(0, 1)}}, false},
		{&alloydbpb.MaintenanceUpdatePolicy{MaintenanceWindows: []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{
			w(dayofweek.DayOfWeek_MONDAY, 1), w(dayofweek.DayOfWeek_FRIDAY, 1)}}, false},
	} {
		if err := checkMaintenance(c.p); (err == nil) != c.ok {
			t.Errorf("checkMaintenance(%v) = %v", c.p, err)
		}
	}
}
