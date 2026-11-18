package bigqueryfront

import (
	"strings"
	"testing"
	"time"
)

func TestObjectTableDDL(t *testing.T) {
	s, ok := parseObjectTableDDL("CREATE OR REPLACE EXTERNAL TABLE `d.t` WITH CONNECTION `p.us.c` OPTIONS(object_metadata = 'SIMPLE', " +
		"uris = ['gs://b/img/*', 'gs://b/x.png'], max_staleness = INTERVAL 4 HOUR, metadata_cache_mode = 'MANUAL', description = 'x')")
	if !ok || !s.replace || s.connection != "p.us.c" || len(s.uris) != 2 || s.stalenessMs != 4*3600000 || s.cacheMode != "MANUAL" ||
		s.description == nil || s.check() != "" || s.unsupported != "" {
		t.Fatalf("%+v ok %v check %q", s, ok, s.check())
	}
	if _, ok := parseObjectTableDDL("CREATE EXTERNAL TABLE d.t OPTIONS(format = 'CSV', uris = ['gs://b/x.csv'])"); ok {
		t.Error("a CSV external table was read as an object table")
	}
	for sql, want := range map[string]string{
		"CREATE EXTERNAL TABLE d.t OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'])":                                                                                                "needs a connection",
		"CREATE EXTERNAL TABLE d.t WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'DIRECTORY', uris = ['gs://b/*'])":                                                                     "object_metadata",
		"CREATE EXTERNAL TABLE d.t WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*/*'])":                                                                      "one * wildcard",
		"CREATE EXTERNAL TABLE d.t WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'], max_staleness = INTERVAL 1 HOUR)":                                       "metadata_cache_mode must be set",
		"CREATE EXTERNAL TABLE d.t WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'], max_staleness = INTERVAL 10 MINUTE, metadata_cache_mode = 'AUTOMATIC')": "between 30 minutes and 7 days",
		"CREATE EXTERNAL TABLE d.t (x INT64) WITH CONNECTION DEFAULT OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'])":                                                              "fixed schema",
	} {
		s, ok := parseObjectTableDDL(sql)
		if !ok || !strings.Contains(s.check(), want) {
			t.Errorf("%s: ok %v, %q", sql, ok, s.check())
		}
	}
}

func TestObjectTableIntervals(t *testing.T) {
	if ms, ok := parseIntervalMs("0-0 0 4:0:0"); !ok || ms != 4*3600000 || formatIntervalMs(ms) != "0-0 0 4:0:0" {
		t.Errorf("4 hours: %d %v", ms, ok)
	}
	if ms, ok := parseIntervalMs("0-0 7 0:0:0"); !ok || formatIntervalMs(ms) != "0-0 7 0:0:0" {
		t.Errorf("7 days: %d %v", ms, ok)
	}
	if _, ok := parseIntervalMs("1-0 0 0:0:0"); ok {
		t.Error("a year was read")
	}
	now := time.UnixMilli(10 * 3600000)
	for _, c := range []struct {
		ot   objectTable
		want bool
	}{
		{objectTable{}, true},
		{objectTable{StalenessMs: 4 * 3600000, CacheMode: "MANUAL", Cached: now.UnixMilli() - 3600000}, false},
		{objectTable{StalenessMs: 4 * 3600000, CacheMode: "MANUAL", Cached: now.UnixMilli() - 5*3600000}, true},
		{objectTable{StalenessMs: 4 * 3600000, CacheMode: "AUTOMATIC", Cached: now.UnixMilli() - 3600000}, true},
		{objectTable{StalenessMs: 4 * 3600000, CacheMode: "AUTOMATIC", Cached: now.UnixMilli() - 60000}, false},
	} {
		if got := c.ot.stale(now); got != c.want {
			t.Errorf("%+v: stale %v", c.ot, got)
		}
	}
	if p, ok, msg := refreshCacheStatement("CALL BQ.REFRESH_EXTERNAL_METADATA_CACHE('p.d.t')"); !ok || msg != "" || len(p) != 3 {
		t.Errorf("refresh: %v %v %q", p, ok, msg)
	}
	if p := writeTarget("INSERT INTO `d.t` (uri) VALUES ('x')"); len(p) != 2 || p[1] != "t" {
		t.Errorf("writeTarget %v", p)
	}
}

func TestObjectTableHiveRefused(t *testing.T) {
	for _, sql := range []string{
		"CREATE EXTERNAL TABLE d.t WITH PARTITION COLUMNS WITH CONNECTION `p.us.c` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'])",
		"CREATE EXTERNAL TABLE d.t WITH PARTITION COLUMNS (k STRING) WITH CONNECTION `p.us.c` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'])",
		"CREATE EXTERNAL TABLE d.t WITH CONNECTION `p.us.c` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://b/*'], hive_partition_uri_prefix = 'gs://b')",
	} {
		s, ok := parseObjectTableDDL(sql)
		if !ok || s.check() != objectHiveRefusal {
			t.Errorf("%s: ok %v, check %q", sql, ok, s.check())
		}
	}
}
