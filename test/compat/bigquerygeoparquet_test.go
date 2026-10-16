//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/option"
)

// nativeGeographyTransport adds nativeGeographyExportEnabled to the
// extract jobs the client inserts: the Go client (v1.85) and its
// generated API (v0.298) have no field for it.
type nativeGeographyTransport struct{ base http.RoundTripper }

func (n nativeGeographyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/jobs") || r.Body == nil {
		return n.base.RoundTrip(r)
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	var job map[string]any
	if json.Unmarshal(body, &job) == nil {
		if cfg, _ := job["configuration"].(map[string]any); cfg != nil {
			if ex, _ := cfg["extract"].(map[string]any); ex != nil {
				ex["nativeGeographyExportEnabled"] = true
				body, _ = json.Marshal(job)
			}
		}
	}
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
	return n.base.RoundTrip(r)
}

// TestBigQueryParquetExtractOfNativeGeography (#957), through the official
// Go client: a PARQUET extract of a GEOGRAPHY column with
// nativeGeographyExportEnabled writes Parquet's GEOGRAPHY logical type
// (edges spherical) with WKB values; without it (GeoParquet metadata,
// whose form BigQuery does not document) it is 501. Arrow Go v15 has no
// GEOGRAPHY logical type, so the footer's SchemaElement is checked by its
// bytes; CLOUDBURROW_GEO_PARQUET_OUT names a file to save the export to,
// to read it with Arrow C++ (pyarrow) by hand.
func TestBigQueryParquetExtractOfNativeGeography(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	nc, err := bigquery.NewClient(ctx, project, option.WithEndpoint(h.Endpoint(EnvBigQuery)),
		option.WithHTTPClient(&http.Client{Transport: nativeGeographyTransport{http.DefaultTransport}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-geo")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = bucket.Object("geo.parquet").Delete(context.Background())
		_ = bucket.Delete(context.Background())
	})
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".geo` AS SELECT 1 AS i, ST_GEOGPOINT(1, 2) AS g "+
		"UNION ALL SELECT 2, NULL", false); err != nil {
		t.Fatal(err)
	}
	extract := func(cl *bigquery.Client) error {
		ref := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/geo.parquet")
		ref.DestinationFormat = bigquery.Parquet
		ectx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := cl.Dataset(ds.DatasetID).Table("geo").ExtractorTo(ref).Run(ectx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ectx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	// Without the flag (#957): a WKT STRING, as "GEOGRAPHY column values
	// are always formatted as WKT strings" (gis-data).
	if err := extract(c); err != nil {
		t.Fatalf("a PARQUET extract of GEOGRAPHY without nativeGeographyExportEnabled: %v", err)
	}
	if r, err := bucket.Object("geo.parquet").NewReader(ctx); err != nil {
		t.Fatal(err)
	} else {
		b, _ := io.ReadAll(r)
		r.Close()
		if !bytes.Contains(b, []byte("POINT(1 2)")) {
			t.Errorf("the PARQUET extract without the flag does not hold the WKT POINT(1 2)")
		}
	}
	if err := extract(nc); err != nil {
		t.Fatalf("a PARQUET extract of GEOGRAPHY with nativeGeographyExportEnabled: %v", err)
	}
	r, err := bucket.Object("geo.parquet").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if p := os.Getenv("CLOUDBURROW_GEO_PARQUET_OUT"); p != "" {
		_ = os.WriteFile(p, b, 0o644)
	}
	if len(b) < 12 || string(b[:4]) != "PAR1" || string(b[len(b)-4:]) != "PAR1" {
		t.Fatalf("not a Parquet file: %d bytes", len(b))
	}
	// The column g's SchemaElement: its name, then logicalType (field 10)
	// GEOGRAPHY (LogicalType field 18) with algorithm (field 2) SPHERICAL.
	if !bytes.Contains(b, []byte("\x01g\x0c\x14\x0c\x24\x25\x00\x00\x00")) {
		t.Errorf("column g is not GEOGRAPHY (edges spherical) in the footer")
	}
	// POINT(1 2) as little-endian WKB.
	if !bytes.Contains(b, []byte("\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\xf0\x3f\x00\x00\x00\x00\x00\x00\x00\x40")) {
		t.Errorf("no WKB POINT(1 2) in the file")
	}
}
