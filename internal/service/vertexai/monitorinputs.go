package vertexai

// Model Monitoring's data sources beyond Cloud Storage CSV and JSONL
// (#2214): BigQuery tables and queries, read through this instance's
// BigQuery; TFRecord files of tf.train.Example records in its Cloud
// Storage; and Agent Platform managed (tabular) datasets (datasets.go),
// read from their BigQuery table or Cloud Storage CSV files.
//
// Documented behaviour (ModelMonitoringInput.ModelMonitoringDataset and
// ModelMonitoringObjectiveConfig.TrainingDataset protos; "Monitor feature
// skew and drift", https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/using-model-monitoring,
// read 2026-11-24):
//   - a BigQuery source is a table URI ("bq://project.dataset.table") or,
//     in v2, a standard SQL query;
//   - Cloud Storage data is CSV, JSONL or TFRecord ("tf-record" in v1);
//   - a managed dataset is named projects/{p}/locations/{l}/datasets/{d}.
//
// TFRecord's framing (TensorFlow's tfrecord format,
// https://www.tensorflow.org/tutorials/load_data/tfrecord): each record is
// a little-endian uint64 length, the masked CRC-32C of the length, the
// data, and the masked CRC-32C of the data; a mask is
// ((crc >> 15) | (crc << 17)) + 0xa282ead8. A record is a serialized
// tf.train.Example: features (1) holds a map<string, Feature> (1), and a
// Feature is one of bytes_list (1), float_list (2) or int64_list (3), each
// a repeated value (1).
//
// UNVERIFIED (Google does not say; re-read 2026-11-24,
// https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/overview):
// a one-element list is read as its value (a feature is a
// scalar per row), a longer one as a list; bytes are read as UTF-8 text.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// bigQueryURIRows reads the rows of the table bq://project.dataset.table.
func bigQueryURIRows(ctx context.Context, store objectStore, field, uri string) ([]map[string]any, error) {
	m := bqTableURI(uri)
	if m == nil {
		return nil, apierror.InvalidArgument("%s %q is not bq://project.dataset.table", field, uri)
	}
	runner, _ := store.(PipelineRunner)
	if runner == nil {
		return nil, apierror.FailedPrecondition("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	return bigQueryRows(ctx, runner, m[1], m[2], m[3])
}

var bqTableRE = regexp.MustCompile(`^bq://([a-z0-9:.-]+)\.([A-Za-z0-9_]+)\.([^.:]+)$`)

// bqTableURI is bq://project.dataset.table's three parts, or nil.
func bqTableURI(uri string) []string {
	return bqTableRE.FindStringSubmatch(uri)
}

// bigQueryQueryRows runs a standard SQL query in project and reads its
// result's rows.
func bigQueryQueryRows(ctx context.Context, store objectStore, project, query string) ([]map[string]any, error) {
	bq, ok := store.(PipelineBigQuery)
	if !ok {
		return nil, apierror.FailedPrecondition("BigQuery is not served on this instance (start with --services including bigquery)")
	}
	body, _ := json.Marshal(map[string]any{"query": query, "useLegacySql": false, "timeoutMs": 60000, "maxResults": 10000})
	code, out, err := bq.BigQueryREST(ctx, "POST", "/bigquery/v2/projects/"+url.PathEscape(project)+"/queries", body)
	if err != nil {
		return nil, err
	}
	type page struct {
		JobComplete  bool     `json:"jobComplete"`
		Schema       bqSchema `json:"schema"`
		Rows         []bqRow  `json:"rows"`
		PageToken    string   `json:"pageToken"`
		JobReference struct {
			JobID    string `json:"jobId"`
			Location string `json:"location"`
		} `json:"jobReference"`
	}
	var rows []map[string]any
	var schema bqSchema
	for deadline := time.Now().Add(10 * time.Minute); ; {
		if code/100 != 2 {
			return nil, apierror.InvalidArgument("the BigQuery query failed: %s", strings.TrimSpace(string(out)))
		}
		var p page
		if err := json.Unmarshal(out, &p); err != nil {
			return nil, apierror.Internal(err, "decoding BigQuery's answer: %v", err)
		}
		if len(p.Schema.Fields) > 0 {
			schema = p.Schema
		}
		if p.JobComplete {
			for _, r := range p.Rows {
				rows = append(rows, schema.object(r))
			}
		}
		if p.JobComplete && p.PageToken == "" {
			return rows, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, apierror.Aborted("the BigQuery query did not finish in 10 minutes")
		}
		q := "/bigquery/v2/projects/" + url.PathEscape(project) + "/queries/" + url.PathEscape(p.JobReference.JobID) +
			"?maxResults=10000&location=" + url.QueryEscape(p.JobReference.Location)
		if p.JobComplete {
			q += "&pageToken=" + url.QueryEscape(p.PageToken)
		}
		if code, out, err = bq.BigQueryREST(ctx, "GET", q, nil); err != nil {
			return nil, err
		}
	}
}

// datasetRows reads a managed tabular dataset's rows from its source.
func (s *Server) datasetRows(ctx context.Context, store objectStore, name string) ([]map[string]any, error) {
	v, err := s.datasetView(name)
	if err != nil {
		return nil, err
	}
	if k := datasetKind(v.dataset.GetMetadataSchemaUri()); k != "tabular" {
		return nil, apierror.InvalidArgument("Dataset %s is a %s dataset: model monitoring reads tabular datasets only", name, dashIfEmptyKind(k))
	}
	if uri := tabularBigQuery(v.dataset); uri != "" {
		return bigQueryURIRows(ctx, store, "dataset "+name+" bigquerySource.uri", uri)
	}
	uris := tabularGCS(v.dataset)
	if len(uris) == 0 {
		return nil, apierror.FailedPrecondition("Dataset %s has no data source: import a BigQuery table or CSV files first", name)
	}
	var rows []map[string]any
	for _, u := range uris {
		r, err := readGCSRows(ctx, store, u, "csv")
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

func dashIfEmptyKind(k string) string {
	if k == "" {
		return "non-tabular"
	}
	return k
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func maskedCRC(b []byte) uint32 {
	c := crc32.Checksum(b, crcTable)
	return ((c >> 15) | (c << 17)) + 0xa282ead8
}

// parseTFRecord reads a TFRecord file's tf.train.Example records as rows.
func parseTFRecord(data []byte) ([]map[string]any, error) {
	var rows []map[string]any
	for off, n := 0, 1; off < len(data); n++ {
		if len(data)-off < 12 {
			return nil, apierror.InvalidArgument("record %d is truncated: TFRecord framing needs 12 bytes of length and CRC", n)
		}
		head := data[off : off+8]
		size := binary.LittleEndian.Uint64(head)
		if binary.LittleEndian.Uint32(data[off+8:]) != maskedCRC(head) {
			return nil, apierror.InvalidArgument("record %d's length CRC does not match: not a TFRecord file", n)
		}
		off += 12
		if size > uint64(len(data)-off) || uint64(len(data)-off)-size < 4 {
			return nil, apierror.InvalidArgument("record %d is truncated", n)
		}
		rec := data[off : off+int(size)]
		off += int(size)
		if binary.LittleEndian.Uint32(data[off:]) != maskedCRC(rec) {
			return nil, apierror.InvalidArgument("record %d's data CRC does not match", n)
		}
		off += 4
		row, err := parseExample(rec)
		if err != nil {
			return nil, apierror.InvalidArgument("record %d is not a tf.train.Example: %v", n, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// fields calls f with each field of a serialized message.
func fields(b []byte, f func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var v []byte
		var x uint64
		switch typ {
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			x, n = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var y uint32
			y, n = protowire.ConsumeFixed32(b)
			x = uint64(y)
		case protowire.Fixed64Type:
			x, n = protowire.ConsumeFixed64(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := f(num, typ, v, x); err != nil {
			return err
		}
	}
	return nil
}

func parseExample(b []byte) (map[string]any, error) {
	row := map[string]any{}
	err := fields(b, func(num protowire.Number, typ protowire.Type, features []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		return fields(features, func(num protowire.Number, typ protowire.Type, entry []byte, _ uint64) error {
			if num != 1 || typ != protowire.BytesType {
				return nil
			}
			var key string
			var val any
			err := fields(entry, func(num protowire.Number, _ protowire.Type, v []byte, _ uint64) error {
				switch num {
				case 1:
					key = string(v)
				case 2:
					f, err := parseFeature(v)
					val = f
					return err
				}
				return nil
			})
			if err == nil && key != "" {
				row[key] = val
			}
			return err
		})
	})
	return row, err
}

// parseFeature is a tf.train.Feature's values: one value, or a list.
func parseFeature(b []byte) (any, error) {
	var vals []any
	err := fields(b, func(kind protowire.Number, _ protowire.Type, list []byte, _ uint64) error {
		return fields(list, func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error {
			if num != 1 {
				return nil
			}
			switch {
			case kind == 1 && typ == protowire.BytesType:
				vals = append(vals, string(v))
			case kind == 2 && typ == protowire.BytesType: // packed floats
				for len(v) >= 4 {
					vals = append(vals, float64(math.Float32frombits(binary.LittleEndian.Uint32(v))))
					v = v[4:]
				}
			case kind == 2 && typ == protowire.Fixed32Type:
				vals = append(vals, float64(math.Float32frombits(uint32(x))))
			case kind == 3 && typ == protowire.BytesType: // packed int64s
				for len(v) > 0 {
					y, n := protowire.ConsumeVarint(v)
					if n < 0 {
						return protowire.ParseError(n)
					}
					vals = append(vals, float64(int64(y)))
					v = v[n:]
				}
			case kind == 3 && typ == protowire.VarintType:
				vals = append(vals, float64(int64(x)))
			}
			return nil
		})
	})
	switch len(vals) {
	case 0:
		return nil, err
	case 1:
		return vals[0], err
	}
	return vals, err
}
