//go:build compat

package compat

import (
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryDDLPartitionExpirationAndKMSKey (#1390): the table options
// partition_expiration_days and kms_key_name (stored) of BigQuery's
// table_option_list
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#table_option_list),
// set by CREATE TABLE and ALTER TABLE SET OPTIONS through jobs.query and
// jobs.insert, read back through Table.Metadata (timePartitioning's
// expirationMs, encryptionConfiguration) and INFORMATION_SCHEMA.TABLE_OPTIONS.
// partition_expiration_days on a table that is not time-partitioned is
// 400; NULL clears either.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLPartitionExpirationAndKMSKey(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	const key = "projects/p/locations/us/keyRings/r/cryptoKeys/k"
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	ref := "`" + ds.DatasetID + ".opt_part`"
	run("CREATE TABLE "+ref+" (day DATE, x INT64) PARTITION BY day OPTIONS (partition_expiration_days = 3, "+
		"kms_key_name = '"+key+"', description = 'd')", false)
	md, err := ds.Table("opt_part").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "day" || md.TimePartitioning.Expiration != 72*time.Hour ||
		md.EncryptionConfig == nil || md.EncryptionConfig.KMSKeyName != key || md.Description != "d" {
		t.Errorf("after CREATE TABLE: %+v %+v %q", md.TimePartitioning, md.EncryptionConfig, md.Description)
	}
	options := func() map[string]string {
		t.Helper()
		it, err := c.Query("SELECT option_name, option_value FROM `" + ds.DatasetID +
			"`.INFORMATION_SCHEMA.TABLE_OPTIONS WHERE table_name = 'opt_part'").Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out[row[0].(string)] = row[1].(string)
		}
		return out
	}
	if o := options(); o["partition_expiration_days"] != "3" || o["kms_key_name"] != `"`+key+`"` {
		t.Errorf("TABLE_OPTIONS after CREATE TABLE: %v", o)
	}

	run("ALTER TABLE "+ref+" SET OPTIONS (partition_expiration_days = 1, kms_key_name = NULL)", true)
	md, err = ds.Table("opt_part").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "day" || md.TimePartitioning.Expiration != 24*time.Hour ||
		md.EncryptionConfig != nil || md.Description != "d" {
		t.Errorf("after ALTER TABLE: %+v %+v %q", md.TimePartitioning, md.EncryptionConfig, md.Description)
	}
	if o := options(); o["partition_expiration_days"] != "1" || o["kms_key_name"] != "" {
		t.Errorf("TABLE_OPTIONS after ALTER TABLE: %v", o)
	}
	run("ALTER TABLE "+ref+" SET OPTIONS (partition_expiration_days = NULL, kms_key_name = '"+key+"')", false)
	md, err = ds.Table("opt_part").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Expiration != 0 || md.EncryptionConfig == nil ||
		md.EncryptionConfig.KMSKeyName != key {
		t.Errorf("after clearing: %+v %+v", md.TimePartitioning, md.EncryptionConfig)
	}

	plain := "`" + ds.DatasetID + ".opt_plain`"
	run("CREATE TABLE "+plain+" (x INT64) OPTIONS (kms_key_name = '"+key+"')", true)
	if md, err := ds.Table("opt_plain").Metadata(ctx); err != nil || md.EncryptionConfig == nil || md.EncryptionConfig.KMSKeyName != key {
		t.Errorf("opt_plain: %+v %v", md, err)
	}
	for sql, code := range map[string]int{
		"CREATE TABLE `" + ds.DatasetID + ".opt_bad` (x INT64) OPTIONS (partition_expiration_days = 1)": http.StatusBadRequest,
		"ALTER TABLE " + plain + " SET OPTIONS (partition_expiration_days = 1)":                         http.StatusBadRequest,
	} {
		wantHTTPStatus(t, sql, bqRun(ctx, c, sql, false), code)
	}
	if tableExists(t, ctx, ds.Table("opt_bad")) {
		t.Error("opt_bad was made, though its CREATE TABLE was refused")
	}
}

// TestBigQueryDDLOptionsInAScript (#1390): a CREATE TABLE, CREATE VIEW and
// ALTER VIEW SET OPTIONS inside a multi-statement script set their
// OPTIONS as a lone statement does (description, friendly_name, labels,
// expiration_timestamp, and partition_expiration_days and kms_key_name on a
// table), through jobs.query and jobs.insert, read back through
// Table.Metadata; a script with an option CloudBurrow does not set is 501
// at that statement.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLOptionsInAScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		tn, vn := "script_t_q", "script_v_q"
		if insert {
			tn, vn = "script_t_j", "script_v_j"
		}
		tr, vr := "`"+ds.DatasetID+"."+tn+"`", "`"+ds.DatasetID+"."+vn+"`"
		sql := "DECLARE n INT64 DEFAULT 1;\n" +
			"CREATE TABLE " + tr + " (day DATE, x INT64) PARTITION BY day OPTIONS (description = 'in a script', " +
			"labels = [('k', 'v')], partition_expiration_days = 2, expiration_timestamp = TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 2 DAY));\n" +
			"INSERT INTO " + tr + " (day, x) VALUES (CURRENT_DATE(), n);\n" +
			"CREATE VIEW " + vr + " OPTIONS (friendly_name = 'V') AS SELECT x FROM " + tr + ";\n" +
			"ALTER VIEW " + vr + " SET OPTIONS (description = 'altered');\n" +
			"SELECT COUNT(*) FROM " + vr
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("insert %v: %v", insert, err)
		}
		md, err := ds.Table(tn).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.Description != "in a script" || md.Labels["k"] != "v" || md.ExpirationTime.IsZero() ||
			md.TimePartitioning == nil || md.TimePartitioning.Expiration != 48*time.Hour {
			t.Errorf("insert %v: table %q %v %v %+v", insert, md.Description, md.Labels, md.ExpirationTime, md.TimePartitioning)
		}
		vm, err := ds.Table(vn).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if vm.Name != "V" || vm.Description != "altered" {
			t.Errorf("insert %v: view %q %q", insert, vm.Name, vm.Description)
		}
	}
	err := bqRun(ctx, c, "SELECT 1; CREATE TABLE `"+ds.DatasetID+".script_bad` (x INT64) OPTIONS (max_staleness = INTERVAL 1 HOUR)", false)
	wantHTTPStatus(t, "an option CloudBurrow does not set, in a script", err, http.StatusNotImplemented)
	if tableExists(t, ctx, ds.Table("script_bad")) {
		t.Error("script_bad was made")
	}
}
