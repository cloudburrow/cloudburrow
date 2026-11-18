//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryAutodetectDialect (#1547): a CSV load with autodetect into a
// new table detects its columns from data read by the load's own quote,
// fieldDelimiter and encoding, each as JobConfigurationLoad documents it
// for a load ("The value that is used to quote data sections", "The
// separator character for fields", the encodings UTF-8, ISO-8859-1,
// UTF-16BE, UTF-16LE, UTF-32BE and UTF-32LE), and a detected header
// under columnNameCharacterMap V1 or V2 that the front detects (here with
// skipLeadingRows 2) is renamed, each unsupported character "replaced
// with underscores" (LOAD DATA, column_name_character_map), as the
// emulator's detected names are (#1326). A name V1 still refuses once
// replaced stays 501. Measured first: each of these loads was 501.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryAutodetectDialect(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table string, data []byte, set func(*bigquery.ReaderSource), charMap bigquery.ColumnNameCharacterMap) error {
		src := bigquery.NewReaderSource(strings.NewReader(string(data)))
		src.SourceFormat = bigquery.CSV
		src.AutoDetect = true
		set(src)
		l := ds.Table(table).LoaderFrom(src)
		l.ColumnNameCharacterMap = charMap
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		job, err := l.Run(lctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(lctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	check := func(table string, names []string, want string) {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || len(md.Schema) != len(names) {
			t.Fatalf("%s: Table.Metadata: %+v %v", table, md, err)
		}
		for i, name := range names {
			if md.Schema[i].Name != name {
				t.Errorf("%s: column %d is %q, want %q", table, i, md.Schema[i].Name, name)
			}
		}
		if typ := md.Schema[len(names)-1].Type; typ != bigquery.IntegerFieldType {
			t.Errorf("%s: the last column is %s, want INTEGER", table, typ)
		}
		got := rows(t, ctx, c, fmt.Sprintf("SELECT * FROM `%s.%s` ORDER BY `%s`", ds.DatasetID, table, names[len(names)-1]))
		if fmt.Sprint(got) != want {
			t.Errorf("%s reads %v, want %s", table, got, want)
		}
	}

	for _, tc := range []struct {
		table   string
		data    []byte
		set     func(*bigquery.ReaderSource)
		charMap bigquery.ColumnNameCharacterMap
		names   []string
		want    string
	}{
		{"quote", []byte("name,n\n'a,b',3\n'c',4\n"), func(s *bigquery.ReaderSource) { s.Quote = "'" }, "",
			[]string{"name", "n"}, "[[a,b 3] [c 4]]"},
		{"latin1", []byte("caf\xe9;n\nx\xe9;3\n"), func(s *bigquery.ReaderSource) {
			s.Encoding, s.FieldDelimiter = bigquery.ISO_8859_1, ";"
		}, "", []string{"café", "n"}, "[[xé 3]]"},
		{"utf16le", utf16Bytes("\ufeffname;n\nété;3\n", false), func(s *bigquery.ReaderSource) {
			s.Encoding, s.FieldDelimiter = "UTF-16LE", ";"
		}, "", []string{"name", "n"}, "[[été 3]]"},
		{"utf32be", utf32Bytes("name,n\nx,3\n", true), func(s *bigquery.ReaderSource) { s.Encoding = "UTF-32BE" }, "",
			[]string{"name", "n"}, "[[x 3]]"},
		{"v2_skip", []byte("title\nfirst name,a!,n\nbob,x,3\n"), func(s *bigquery.ReaderSource) { s.SkipLeadingRows = 2 },
			bigquery.V2ColumnNameCharacterMap, []string{"first name", "a_", "n"}, "[[bob x 3]]"},
		{"v1_skip", []byte("title\nfirst name,a!,n\nbob,x,3\n"), func(s *bigquery.ReaderSource) { s.SkipLeadingRows = 2 },
			bigquery.V1ColumnNameCharacterMap, []string{"first_name", "a_", "n"}, "[[bob x 3]]"},
	} {
		if err := load(tc.table, tc.data, tc.set, tc.charMap); err != nil {
			t.Errorf("%s: the load failed: %v", tc.table, err)
			continue
		}
		check(tc.table, tc.names, tc.want)
	}

	err := load("v1_digit", []byte("title\n1a,n\nx,3\n"), func(s *bigquery.ReaderSource) { s.SkipLeadingRows = 2 },
		bigquery.V1ColumnNameCharacterMap)
	wantHTTPStatus(t, "a V1 load whose detected name V1 still refuses once renamed", err, http.StatusNotImplemented)
	if tableExists(t, ctx, ds.Table("v1_digit")) {
		t.Error("the refused load left v1_digit")
	}
}
