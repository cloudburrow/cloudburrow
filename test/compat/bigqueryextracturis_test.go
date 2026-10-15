//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/iterator"
)

// TestBigQueryExtractToSeveralURIs (#957), through the official Go client,
// the files read back from the instance's Cloud Storage: a wildcard URI is
// written as its first file, 000000000000 in place of the *, as BigQuery
// documents ("replaced with a number (starting at 0), left-padded to 12
// digits"), and several URIs share the table's rows out between them, each
// row in one file, in CSV (with a header in each), NEWLINE_DELIMITED_JSON
// and AVRO; destinationUriFileCounts gives one file a URI. How BigQuery
// splits the rows between the URIs is not documented, so only that every
// row is written once is checked.
func TestBigQueryExtractToSeveralURIs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-uris")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		it := bucket.Objects(bg, nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = bucket.Object(o.Name).Delete(bg)
		}
		_ = bucket.Delete(bg)
	})
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".src` AS SELECT n AS id, CONCAT('s', CAST(n AS STRING)) AS s "+
		"FROM UNNEST(GENERATE_ARRAY(1, 5)) AS n", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"1|s1", "2|s2", "3|s3", "4|s4", "5|s5"}
	extract := func(format bigquery.DataFormat, names ...string) []int64 {
		t.Helper()
		var uris []string
		for _, n := range names {
			uris = append(uris, "gs://"+bucket.BucketName()+"/"+n)
		}
		ref := bigquery.NewGCSReference(uris...)
		ref.DestinationFormat = format
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table("src").ExtractorTo(ref).Run(ectx)
		if err != nil {
			t.Fatalf("extract to %v: %v", names, err)
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("extract to %v: %v", names, err)
		}
		ex, _ := st.Statistics.Details.(*bigquery.ExtractStatistics)
		if ex == nil {
			t.Fatalf("extract to %v: no extract statistics", names)
		}
		// jobs.get gives the client's URIs, wildcards and all.
		if cfg, err := job.Config(); err != nil || !reflect.DeepEqual(cfg.(*bigquery.ExtractConfig).Dst.URIs, uris) {
			t.Errorf("extract to %v: the job's URIs %v (%v)", names, cfg, err)
		}
		return ex.DestinationURIFileCounts
	}
	object := func(name string) []byte {
		t.Helper()
		r, err := bucket.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return b
	}
	check := func(what string, got []string, counts []int64, files int) {
		t.Helper()
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %q, want %q", what, got, want)
		}
		if len(counts) != files {
			t.Errorf("%s: destinationUriFileCounts %v, want %d of 1", what, counts, files)
		}
		for _, n := range counts {
			if n != 1 {
				t.Errorf("%s: destinationUriFileCounts %v, want %d of 1", what, counts, files)
			}
		}
	}

	// CSV to one wildcard URI, then to two.
	csvRows := func(name string) []string {
		t.Helper()
		recs, err := csv.NewReader(bytes.NewReader(object(name))).ReadAll()
		if err != nil || len(recs) == 0 || !reflect.DeepEqual(recs[0], []string{"id", "s"}) {
			t.Fatalf("%s: %q (%v), want a header row first", name, recs, err)
		}
		var out []string
		for _, r := range recs[1:] {
			out = append(out, strings.Join(r, "|"))
		}
		return out
	}
	counts := extract(bigquery.CSV, "one-*.csv")
	check("CSV to one wildcard URI", csvRows("one-000000000000.csv"), counts, 1)
	counts = extract(bigquery.CSV, "a-*.csv", "b-*.csv")
	a, b := csvRows("a-000000000000.csv"), csvRows("b-000000000000.csv")
	if len(a) == 0 || len(b) == 0 {
		t.Errorf("CSV to two URIs: %d and %d rows, want both some", len(a), len(b))
	}
	check("CSV to two wildcard URIs", append(a, b...), counts, 2)

	// NEWLINE_DELIMITED_JSON to two URIs without wildcards.
	jsonRows := func(name string) []string {
		t.Helper()
		var out []string
		for _, line := range strings.Split(strings.TrimSuffix(string(object(name)), "\n"), "\n") {
			if line == "" {
				continue
			}
			var v struct{ ID, S string }
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatalf("%s: %q: %v", name, line, err)
			}
			out = append(out, v.ID+"|"+v.S)
		}
		return out
	}
	counts = extract(bigquery.JSON, "p.json", "q.json")
	check("JSON to two URIs", append(jsonRows("p.json"), jsonRows("q.json")...), counts, 2)

	// AVRO to three wildcard URIs.
	avroRows := func(name string) []string {
		t.Helper()
		ocf, err := goavro.NewOCFReader(bytes.NewReader(object(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out []string
		for ocf.Scan() {
			v, err := ocf.Read()
			if err != nil {
				t.Fatal(err)
			}
			m := v.(map[string]any)
			id, _ := m["id"].(map[string]any)["long"].(int64)
			s, _ := m["s"].(map[string]any)["string"].(string)
			out = append(out, strings.Join([]string{strconv.FormatInt(id, 10), s}, "|"))
		}
		return out
	}
	counts = extract(bigquery.Avro, "x-*.avro", "y-*.avro", "z-*.avro")
	var all []string
	for _, n := range []string{"x", "y", "z"} {
		all = append(all, avroRows(n+"-000000000000.avro")...)
	}
	check("AVRO to three wildcard URIs", all, counts, 3)

	// Nothing else was written: a wildcard is not left in a name.
	it := bucket.Objects(ctx, nil)
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(o.Name, "*") {
			t.Errorf("an object named %s was written", o.Name)
		}
	}
}
