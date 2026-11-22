package main

// Dataform's notebook actions, Cloud Storage outputs and unit test rows
// (#2105; internal/service/dataform/pipelines.go): notebooks run on the
// instance's notebook runtime (internal/nbruntime), objects are written to
// the instance's Cloud Storage with the official client, and a unit test's
// queries are read back with the official BigQuery client.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/nbruntime"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// dataformNotebooks runs notebook actions on the notebook runtime.
type dataformNotebooks struct{ m *nbruntime.Manager }

func (d dataformNotebooks) RunNotebook(ctx context.Context, key, project string, nb []byte) ([]byte, error) {
	return d.m.RunNotebook(ctx, key, project, nb)
}

// dataformStorage writes objects to the instance's Cloud Storage.
type dataformStorage struct{ tunnel func() *netfwd.Forwarder }

func (s dataformStorage) PutObject(ctx context.Context, bucket, object, contentType string, data []byte) (int64, string, error) {
	var f *netfwd.Forwarder
	if s.tunnel != nil {
		f = s.tunnel()
	}
	if f == nil || f.HostAddr() == "" {
		return 0, "", errors.New("Cloud Storage is not served on this instance")
	}
	c, err := storage.NewClient(ctx, option.WithEndpoint("http://"+f.HostAddr()+"/storage/v1/"), option.WithoutAuthentication(),
		storage.WithJSONReads(), storage.WithDisabledClientMetrics())
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = c.Close() }()
	w := c.Bucket(bucket).Object(object).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return 0, "", err
	}
	if err := w.Close(); err != nil {
		return 0, "", err
	}
	a := w.Attrs()
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], a.CRC32C)
	return a.Generation, base64.StdEncoding.EncodeToString(crc[:]), nil
}

// Rows runs a query and returns its columns and rows, each value as its
// string form.
func (b dataformBigQuery) Rows(ctx context.Context, project, location, sql string) (string, []string, [][]*string, int64, error) {
	job, it, err := b.query(ctx, project, location, sql)
	id := ""
	var processed int64
	if job != nil {
		id = job.ID()
		if st := job.LastStatus(); st != nil && st.Statistics != nil {
			processed = st.Statistics.TotalBytesProcessed
		}
	}
	if err != nil {
		return id, nil, nil, processed, err
	}
	var cols []string
	var rows [][]*string
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return id, nil, nil, processed, err
		}
		if cols == nil {
			for _, f := range it.Schema {
				cols = append(cols, f.Name)
			}
		}
		out := make([]*string, len(row))
		for i, v := range row {
			if v != nil {
				s := fmt.Sprint(v)
				out[i] = &s
			}
		}
		rows = append(rows, out)
	}
	if cols == nil {
		for _, f := range it.Schema {
			cols = append(cols, f.Name)
		}
	}
	return id, cols, rows, processed, nil
}
