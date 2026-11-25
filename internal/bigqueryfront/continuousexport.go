package bigqueryfront

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/civil"
	pubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/spanner"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

// Where continuous queries export to (#2193): EXPORT DATA's CLOUD_PUBSUB,
// CLOUD_BIGTABLE and CLOUD_SPANNER formats, written to the instance's own
// Pub/Sub, Bigtable and Spanner, never Google's: each URI names Google's
// endpoint, and the project, instance and table in it are looked up in
// the instance's service, at the address the front was given
// (--pubsub, --bigtable, --spanner). A service the instance does not run
// is refused when the job starts.
//
// As documented (read 2026-11-23):
//
//   - Pub/Sub (https://docs.cloud.google.com/bigquery/docs/export-to-pubsub):
//     uri https://pubsub.googleapis.com/projects/P/topics/T; "a single
//     STRING or BYTES column" is each message's data, with an optional
//     _ATTRIBUTES column of JSON whose members are its attributes; "You
//     can't export data to a Pub/Sub topic that uses a schema"; "you
//     can't export records that consist of only NULL values"; a
//     continuous query exporting to Pub/Sub runs as a service account
//     (continuousjob.go).
//   - Bigtable (https://docs.cloud.google.com/bigquery/docs/export-to-bigtable):
//     uri https://bigtable.googleapis.com/projects/P/instances/I/appProfiles/A/tables/T;
//     a rowkey column of STRING or BYTES and at least one other; a NULL
//     rowkey's row is skipped; each other column is the column family of
//     its name with the empty qualifier, a STRUCT's or JSON's members
//     columns of that family; bigtable_options' columnFamilies name the
//     family, encoding (BINARY or TEXT) and qualifiers of each field;
//     INT64 8 bytes big-endian (BINARY) or decimal (TEXT), FLOAT64 IEEE
//     754 8 bytes, BOOL 0x00/0x01 or "true"/"false", STRING its bytes,
//     BYTES as is, a nested JSON value its text; NULL deletes the cell, a
//     NULL STRUCT the family; _CHANGE_TIMESTAMP is the cells' timestamp,
//     else 0, in milliseconds; auto_create_column_families makes a
//     missing family.
//   - Spanner (https://docs.cloud.google.com/bigquery/docs/export-to-spanner):
//     uri https://spanner.googleapis.com/projects/P/instances/I/databases/D;
//     spanner_options {"table": …} names an existing table, whose columns
//     the result's are, every NOT NULL one among them, a row at most 1
//     MiB; STRUCT, GEOGRAPHY, DATETIME, RANGE, TIME and BIGNUMERIC are not
//     exported; change_timestamp_column keeps a row's newest version;
//     continuous queries do not export to a PostgreSQL-dialect database
//     or a table whose key is generated.
//
// UNVERIFIED (docs re-read 2026-11-23: the pages above): the messages of
// the refusals, which Google does not give; that a missing column family
// without auto_create_column_families fails the job.

// continuousSinks are the instance's services a continuous query exports
// to, host:port each, or "".
type continuousSinks struct {
	pubsub, bigtable, spanner string
}

// cqField is a result column.
type cqField struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Mode   string    `json:"mode"`
	Fields []cqField `json:"fields"`
}

var (
	pubsubURI   = regexp.MustCompile(`^https://(?:[a-z0-9-]+-)?pubsub\.googleapis\.com/projects/([^/]+)/topics/([^/]+)$`)
	bigtableURI = regexp.MustCompile(`^https://(?:[a-z0-9-]+-)?bigtable\.googleapis\.com/projects/([^/]+)/instances/([^/]+)(?:/appProfiles/([^/]+))?/tables/([^/]+)$`)
	spannerURI  = regexp.MustCompile(`^https://(?:[a-z0-9-]+-)?spanner\.googleapis\.com/projects/([^/]+)/instances/([^/]+)/databases/([^/]+)$`)
)

// cqSink writes a window's rows.
type cqSink interface {
	write(ctx context.Context, fields []cqField, rows [][]any, now time.Time) error
	close()
}

func grpcOpts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// openSink opens the export of p, checking what can be checked before
// any row is read.
func (s continuousSinks) openSink(ctx context.Context, p *cqPlan) (cqSink, error) {
	switch p.format {
	case "CLOUD_PUBSUB":
		m := pubsubURI.FindStringSubmatch(p.uri)
		if m == nil {
			return nil, &cqRefusal{400, fmt.Sprintf("Invalid uri %q for format CLOUD_PUBSUB: it is https://pubsub.googleapis.com/projects/PROJECT_ID/topics/TOPIC_ID", p.uri)}
		}
		if s.pubsub == "" {
			return nil, &cqRefusal{501, "Not implemented here: this instance runs no Pub/Sub (start it with the pubsub service) for the continuous query to export to"}
		}
		c, err := pubsub.NewClient(ctx, m[1], grpcOpts(s.pubsub)...)
		if err != nil {
			return nil, err
		}
		topic := "projects/" + m[1] + "/topics/" + m[2]
		t, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
		if err != nil {
			_ = c.Close()
			if status.Code(err) == codes.NotFound {
				return nil, &cqRefusal{404, "Not found: Pub/Sub topic " + topic}
			}
			return nil, err
		}
		if t.GetSchemaSettings().GetSchema() != "" {
			_ = c.Close()
			return nil, &cqRefusal{400, "You can't export data to a Pub/Sub topic that uses a schema: " + topic}
		}
		return &pubsubSink{client: c, pub: c.Publisher(topic), topic: topic}, nil
	case "CLOUD_BIGTABLE":
		m := bigtableURI.FindStringSubmatch(p.uri)
		if m == nil {
			return nil, &cqRefusal{400, fmt.Sprintf("Invalid uri %q for format CLOUD_BIGTABLE: it is "+
				"https://bigtable.googleapis.com/projects/PROJECT_ID/instances/INSTANCE_ID/appProfiles/APP_PROFILE_ID/tables/TABLE", p.uri)}
		}
		if s.bigtable == "" {
			return nil, &cqRefusal{501, "Not implemented here: this instance runs no Bigtable (start it with the bigtable service) for the continuous query to export to"}
		}
		bo := bigtableOptions{}
		if raw := p.options["bigtable_options"]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &bo); err != nil {
				return nil, &cqRefusal{400, "Invalid bigtable_options: " + err.Error()}
			}
		}
		for _, f := range bo.ColumnFamilies {
			switch strings.ToUpper(f.Encoding) {
			case "", "BINARY", "TEXT":
			default:
				return nil, &cqRefusal{400, fmt.Sprintf("Invalid bigtable_options: encoding %q is BINARY or TEXT", f.Encoding)}
			}
			for _, c := range f.Columns {
				if c.QualifierString != "" && c.QualifierEncoded != "" {
					return nil, &cqRefusal{400, "Invalid bigtable_options: qualifierString and qualifierEncoded are mutually exclusive"}
				}
			}
		}
		ac, err := bigtable.NewAdminClient(ctx, m[1], m[2], grpcOpts(s.bigtable)...)
		if err != nil {
			return nil, err
		}
		info, err := ac.TableInfo(ctx, m[4])
		if err != nil {
			_ = ac.Close()
			if status.Code(err) == codes.NotFound {
				return nil, &cqRefusal{404, "Not found: Bigtable table " + m[4] + " in instance " + m[2]}
			}
			return nil, err
		}
		dc, err := localclient.Bigtable(ctx, m[1], m[2], grpcOpts(s.bigtable)...)
		if err != nil {
			_ = ac.Close()
			return nil, err
		}
		fams := map[string]bool{}
		for _, f := range info.Families {
			fams[f] = true
		}
		auto, _ := strconv.ParseBool(p.options["auto_create_column_families"])
		return &bigtableSink{admin: ac, data: dc, table: m[4], tbl: dc.Open(m[4]), families: fams, auto: auto, opts: bo}, nil
	case "CLOUD_SPANNER":
		m := spannerURI.FindStringSubmatch(p.uri)
		if m == nil {
			return nil, &cqRefusal{400, fmt.Sprintf("Invalid uri %q for format CLOUD_SPANNER: it is "+
				"https://spanner.googleapis.com/projects/PROJECT_ID/instances/INSTANCE_ID/databases/DATABASE_ID", p.uri)}
		}
		if s.spanner == "" {
			return nil, &cqRefusal{501, "Not implemented here: this instance runs no Spanner (start it with the spanner service) for the continuous query to export to"}
		}
		var so struct {
			Table           string `json:"table"`
			ChangeTimestamp string `json:"change_timestamp_column"`
			Priority        string `json:"priority"`
			Tag             string `json:"tag"`
		}
		if err := json.Unmarshal([]byte(p.options["spanner_options"]), &so); err != nil || so.Table == "" {
			return nil, &cqRefusal{400, `spanner_options is required, with the table to export to: spanner_options = '{"table": "TABLE_NAME"}'`}
		}
		switch strings.ToUpper(so.Priority) {
		case "", "LOW", "MEDIUM", "HIGH":
		default:
			return nil, &cqRefusal{400, fmt.Sprintf("Invalid spanner_options: priority %q is LOW, MEDIUM or HIGH", so.Priority)}
		}
		db := "projects/" + m[1] + "/instances/" + m[2] + "/databases/" + m[3]
		localclient.SpannerLocalOnly(s.spanner)
		c, err := localclient.Spanner(ctx, db, grpcOpts(s.spanner)...)
		if err != nil {
			return nil, err
		}
		sk := &spannerSink{client: c, table: so.Table, changeTS: so.ChangeTimestamp, columns: map[string]spannerColumn{}}
		if err := sk.describe(ctx); err != nil {
			c.Close()
			return nil, err
		}
		return sk, nil
	}
	return nil, &cqRefusal{400, "Unsupported export format " + p.format}
}

// cellBytes is a value as Bigtable and Pub/Sub take bytes: STRING's text,
// BYTES as is.
func valueBytes(f cqField, v any) ([]byte, bool) {
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	if f.Type == "BYTES" {
		b, err := base64.StdEncoding.DecodeString(s)
		return b, err == nil
	}
	return []byte(s), true
}

// pubsubSink publishes each row as a message.
type pubsubSink struct {
	client *pubsub.Client
	pub    *pubsub.Publisher
	topic  string
}

func (p *pubsubSink) close() {
	p.pub.Stop()
	_ = p.client.Close()
}

func (p *pubsubSink) write(ctx context.Context, fields []cqField, rows [][]any, _ time.Time) error {
	data, attrs := -1, -1
	for i, f := range fields {
		if f.Name == "_ATTRIBUTES" {
			if f.Type != "JSON" {
				return &cqRefusal{400, "The _ATTRIBUTES column must be of JSON type"}
			}
			attrs = i
			continue
		}
		if data >= 0 || f.Type != "STRING" && f.Type != "BYTES" || f.Mode == "REPEATED" {
			return &cqRefusal{400, "Exporting to Pub/Sub takes a single STRING or BYTES column (and an optional _ATTRIBUTES column of JSON type)"}
		}
		data = i
	}
	if data < 0 {
		return &cqRefusal{400, "Exporting to Pub/Sub takes a single STRING or BYTES column (and an optional _ATTRIBUTES column of JSON type)"}
	}
	var results []*pubsub.PublishResult
	for _, row := range rows {
		if row[data] == nil {
			return &cqRefusal{400, "You can't export records that consist of only NULL values to Pub/Sub: filter them out, for example WHERE " + fields[data].Name + " IS NOT NULL"}
		}
		b, _ := valueBytes(fields[data], row[data])
		msg := &pubsub.Message{Data: b}
		if attrs >= 0 && row[attrs] != nil {
			a, err := pubsubAttributes(row[attrs])
			if err != nil {
				return err
			}
			msg.Attributes = a
		}
		results = append(results, p.pub.Publish(ctx, msg))
	}
	for _, r := range results {
		if _, err := r.Get(ctx); err != nil {
			return fmt.Errorf("publish to %s: %w", p.topic, err)
		}
	}
	return nil
}

// pubsubAttributes are a row's _ATTRIBUTES: "The _ATTRIBUTES column must
// be of JSON type, in the format ARRAY<STRUCT<STRING, STRING>> or
// STRUCT<STRING>" (continuous-queries): an object of strings, or an array
// of two-member objects (key, value).
func pubsubAttributes(v any) (map[string]string, error) {
	s, _ := v.(string)
	var raw any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, &cqRefusal{400, "The _ATTRIBUTES column is not valid JSON"}
	}
	str := func(x any) string {
		if t, ok := x.(string); ok {
			return t
		}
		b, _ := json.Marshal(x)
		return string(b)
	}
	out := map[string]string{}
	switch r := raw.(type) {
	case map[string]any:
		for k, x := range r {
			out[k] = str(x)
		}
	case []any:
		for _, e := range r {
			o, ok := e.(map[string]any)
			if !ok || len(o) != 2 {
				return nil, &cqRefusal{400, "The _ATTRIBUTES column's array members are STRUCT<STRING, STRING>"}
			}
			var k, val string
			if kk, ok := o["key"]; ok {
				k, val = str(kk), str(o["value"])
			} else {
				var vals []string
				for _, x := range o {
					vals = append(vals, str(x))
				}
				k, val = vals[0], vals[1]
			}
			out[k] = val
		}
	case nil:
	default:
		return nil, &cqRefusal{400, "The _ATTRIBUTES column must be ARRAY<STRUCT<STRING, STRING>> or STRUCT<STRING> as JSON"}
	}
	return out, nil
}

type bigtableOptions struct {
	ColumnFamilies []struct {
		FamilyID string `json:"familyId"`
		Encoding string `json:"encoding"`
		Columns  []struct {
			QualifierString  string `json:"qualifierString"`
			QualifierEncoded string `json:"qualifierEncoded"`
			FieldName        string `json:"fieldName"`
		} `json:"columns"`
	} `json:"columnFamilies"`
}

// bigtableSink writes each row's cells.
type bigtableSink struct {
	admin    *bigtable.AdminClient
	data     *bigtable.Client
	table    string
	tbl      *bigtable.Table
	families map[string]bool
	auto     bool
	opts     bigtableOptions
}

func (b *bigtableSink) close() {
	_ = b.data.Close()
	_ = b.admin.Close()
}

// family is the family of a result column and how its values encode, and
// a field's qualifier in it.
func (b *bigtableSink) family(column string) (id string, text bool, qualifier func(field string) string) {
	for _, f := range b.opts.ColumnFamilies {
		if f.FamilyID == column {
			return f.FamilyID, strings.EqualFold(f.Encoding, "TEXT"), func(field string) string {
				for _, c := range f.Columns {
					if c.FieldName == field {
						if c.QualifierString != "" {
							return c.QualifierString
						}
						if c.QualifierEncoded != "" {
							if q, err := base64.StdEncoding.DecodeString(c.QualifierEncoded); err == nil {
								return string(q)
							}
						}
					}
				}
				return field
			}
		}
	}
	return column, false, func(field string) string { return field }
}

// bigtableValue encodes v of field f.
func bigtableValue(f cqField, v any, text bool) ([]byte, error) {
	s, _ := v.(string)
	switch f.Type {
	case "STRING", "BYTES":
		b, _ := valueBytes(f, v)
		return b, nil
	case "INTEGER", "INT64":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		if text {
			return []byte(s), nil
		}
		out := make([]byte, 8)
		binary.BigEndian.PutUint64(out, uint64(n))
		return out, nil
	case "FLOAT", "FLOAT64":
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 8)
		binary.BigEndian.PutUint64(out, math.Float64bits(x))
		return out, nil
	case "BOOLEAN", "BOOL":
		t := strings.EqualFold(s, "true")
		if text {
			return []byte(strconv.FormatBool(t)), nil
		}
		if t {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case "JSON":
		return []byte(s), nil
	}
	return nil, &cqRefusal{400, fmt.Sprintf("Column %s is of type %s, which exporting to Bigtable does not take: convert it to STRING, BYTES, INT64, FLOAT64, BOOL, STRUCT or JSON", f.Name, f.Type)}
}

func (b *bigtableSink) ensure(ctx context.Context, fam string) error {
	if b.families[fam] {
		return nil
	}
	if !b.auto {
		return &cqRefusal{400, fmt.Sprintf("Bigtable table %s has no column family %s: create it, or set auto_create_column_families = true", b.table, fam)}
	}
	if err := b.admin.CreateColumnFamily(ctx, b.table, fam); err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	b.families[fam] = true
	return nil
}

func (b *bigtableSink) write(ctx context.Context, fields []cqField, rows [][]any, _ time.Time) error {
	key, ts := -1, -1
	for i, f := range fields {
		switch f.Name {
		case "rowkey":
			if f.Type != "STRING" && f.Type != "BYTES" {
				return &cqRefusal{400, "The rowkey column must be of STRING or BYTES type"}
			}
			key = i
		case "_CHANGE_TIMESTAMP":
			if f.Type != "TIMESTAMP" {
				return &cqRefusal{400, "The _CHANGE_TIMESTAMP column must be of TIMESTAMP type"}
			}
			ts = i
		}
	}
	if key < 0 {
		return &cqRefusal{400, "Exporting to Bigtable needs a rowkey column of STRING or BYTES type"}
	}
	if len(fields)-1-boolInt(ts >= 0) < 1 {
		return &cqRefusal{400, "Exporting to Bigtable needs at least one column other than rowkey"}
	}
	var keys []string
	var muts []*bigtable.Mutation
	for _, row := range rows {
		if row[key] == nil {
			continue // "Rows with a NULL rowkey are skipped".
		}
		kb, _ := valueBytes(fields[key], row[key])
		stamp := bigtable.Timestamp(0)
		if ts >= 0 && row[ts] != nil {
			t, err := timestampValue(row[ts])
			if err != nil {
				return err
			}
			if t.Before(time.Unix(0, 0)) {
				return &cqRefusal{400, "_CHANGE_TIMESTAMP values before the Unix epoch are not supported"}
			}
			stamp = bigtable.Timestamp(t.UnixMilli() * 1000)
		}
		m := bigtable.NewMutation()
		for i, f := range fields {
			if i == key || i == ts {
				continue
			}
			fam, text, qual := b.family(f.Name)
			if err := b.ensure(ctx, fam); err != nil {
				return err
			}
			v := row[i]
			switch {
			case f.Type == "RECORD" || f.Type == "STRUCT":
				if v == nil {
					m.DeleteCellsInFamily(fam)
					continue
				}
				members, _ := v.([]any)
				for k, sf := range f.Fields {
					var mv any
					if k < len(members) {
						mv = members[k]
					}
					if mv == nil {
						m.DeleteCellsInColumn(fam, qual(sf.Name))
						continue
					}
					val, err := bigtableValue(sf, mv, text)
					if err != nil {
						return err
					}
					m.Set(fam, qual(sf.Name), stamp, val)
				}
			case f.Type == "JSON":
				if v == nil {
					m.DeleteCellsInFamily(fam)
					continue
				}
				var obj map[string]any
				s, _ := v.(string)
				if json.Unmarshal([]byte(s), &obj) != nil {
					return &cqRefusal{400, "Column " + f.Name + " of JSON type must hold a JSON object to export to Bigtable"}
				}
				for k, x := range obj {
					if x == nil {
						m.DeleteCellsInColumn(fam, qual(k))
						continue
					}
					var val []byte
					switch t := x.(type) {
					case string:
						val = []byte(t)
					default:
						val, _ = json.Marshal(t)
					}
					m.Set(fam, qual(k), stamp, val)
				}
			case f.Mode == "REPEATED":
				return &cqRefusal{400, "Column " + f.Name + " is an ARRAY, which exporting to Bigtable does not take"}
			case v == nil:
				m.DeleteCellsInColumn(fam, qual(""))
			default:
				val, err := bigtableValue(f, v, text)
				if err != nil {
					return err
				}
				m.Set(fam, qual(""), stamp, val)
			}
		}
		keys = append(keys, string(kb))
		muts = append(muts, m)
	}
	if len(keys) == 0 {
		return nil
	}
	errs, err := b.tbl.ApplyBulk(ctx, keys, muts)
	if err != nil {
		return err
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// timestampValue is a TIMESTAMP cell, as the REST API writes it: seconds
// since the epoch, with a fraction.
func timestampValue(v any) (time.Time, error) {
	s, _ := v.(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if t, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("timestamp %q: %w", s, err)
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).UTC(), nil
}

// spannerColumn is a Spanner table's column.
type spannerColumn struct {
	typ     string
	notNull bool
}

// spannerSink writes each row with InsertOrUpdate.
type spannerSink struct {
	client   *spanner.Client
	table    string
	changeTS string
	columns  map[string]spannerColumn
	keys     []string
}

func (s *spannerSink) close() { s.client.Close() }

// describe reads the table's columns and key from INFORMATION_SCHEMA,
// refusing a PostgreSQL-dialect database (whose INFORMATION_SCHEMA is
// not read with GoogleSQL's names) and a table whose key is generated.
func (s *spannerSink) describe(ctx context.Context) error {
	tx := s.client.Single()
	defer tx.Close()
	it := tx.Query(ctx, spanner.Statement{SQL: "SELECT option_value FROM information_schema.database_options WHERE option_name = 'database_dialect'"})
	row, err := it.Next()
	if err == nil {
		var d string
		if row.Columns(&d) == nil && strings.EqualFold(d, "POSTGRESQL") {
			it.Stop()
			return &cqRefusal{400, "Continuous queries can't export to a PostgreSQL-dialect Spanner database"}
		}
	}
	it.Stop()
	if err != nil && !errors.Is(err, iterator.Done) {
		if status.Code(err) == codes.NotFound {
			return &cqRefusal{404, "Not found: Spanner database: " + err.Error()}
		}
		return err
	}
	tx2 := s.client.Single()
	defer tx2.Close()
	cit := tx2.Query(ctx, spanner.Statement{SQL: "SELECT column_name, spanner_type, is_nullable, IFNULL(column_default, ''), IFNULL(is_identity, 'NO'), IFNULL(generation_expression, '') " +
		"FROM information_schema.columns WHERE table_schema = '' AND table_name = @t", Params: map[string]any{"t": s.table}})
	defaults := map[string]bool{}
	err = cit.Do(func(r *spanner.Row) error {
		var name, typ, nullable, def, ident, gen string
		if err := r.Columns(&name, &typ, &nullable, &def, &ident, &gen); err != nil {
			return err
		}
		s.columns[name] = spannerColumn{typ: typ, notNull: nullable == "NO" && def == "" && gen == ""}
		up := strings.ToUpper(def)
		defaults[name] = ident == "YES" || strings.Contains(up, "GET_NEXT_SEQUENCE_VALUE") || strings.Contains(up, "GENERATE_UUID") || gen != ""
		return nil
	})
	if err != nil {
		return err
	}
	if len(s.columns) == 0 {
		return &cqRefusal{404, "Not found: Spanner table " + s.table + ": the table must exist before the export"}
	}
	tx3 := s.client.Single()
	defer tx3.Close()
	kit := tx3.Query(ctx, spanner.Statement{SQL: "SELECT column_name FROM information_schema.index_columns WHERE table_schema = '' AND table_name = @t AND index_type = 'PRIMARY_KEY' ORDER BY ordinal_position",
		Params: map[string]any{"t": s.table}})
	err = kit.Do(func(r *spanner.Row) error {
		var name string
		if err := r.Columns(&name); err != nil {
			return err
		}
		s.keys = append(s.keys, name)
		if defaults[name] {
			return &cqRefusal{400, "Continuous queries can't export to a Spanner table with an auto-generated primary key: " + s.table}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.changeTS != "" {
		if c, ok := s.columns[s.changeTS]; !ok || c.typ != "TIMESTAMP" {
			return &cqRefusal{400, "spanner_options change_timestamp_column " + s.changeTS + " is not a TIMESTAMP column of " + s.table}
		}
	}
	return nil
}

// spannerUnsupported are the BigQuery types Spanner does not take.
var spannerUnsupported = map[string]bool{"RECORD": true, "STRUCT": true, "GEOGRAPHY": true, "DATETIME": true, "RANGE": true, "TIME": true, "BIGNUMERIC": true}

func (s *spannerSink) write(ctx context.Context, fields []cqField, rows [][]any, _ time.Time) error {
	present := map[string]bool{}
	for _, f := range fields {
		if spannerUnsupported[f.Type] {
			return &cqRefusal{400, fmt.Sprintf("Column %s is of type %s, which exporting to Spanner does not take", f.Name, f.Type)}
		}
		if _, ok := s.columns[f.Name]; !ok {
			return &cqRefusal{400, fmt.Sprintf("Column %s is not a column of the Spanner table %s", f.Name, s.table)}
		}
		present[f.Name] = true
	}
	for name, c := range s.columns {
		if c.notNull && !present[name] {
			return &cqRefusal{400, fmt.Sprintf("The NOT NULL column %s of the Spanner table %s is not among the query's columns", name, s.table)}
		}
	}
	var muts []*spanner.Mutation
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = f.Name
	}
	for _, row := range rows {
		vals := make([]any, len(fields))
		size := 0
		for i, f := range fields {
			v, n, err := spannerValue(f, s.columns[f.Name].typ, row[i])
			if err != nil {
				return err
			}
			vals[i], size = v, size+n
		}
		if size > 1<<20 {
			return &cqRefusal{400, "A row exported to Spanner can be at most 1 MiB"}
		}
		if s.changeTS != "" {
			newer, err := s.newer(ctx, fields, row)
			if err != nil {
				return err
			}
			if !newer {
				continue
			}
		}
		muts = append(muts, spanner.InsertOrUpdate(s.table, cols, vals))
	}
	if len(muts) == 0 {
		return nil
	}
	_, err := s.client.Apply(ctx, muts)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "Parent row") || strings.Contains(strings.ToLower(msg), "parent row"):
			return &cqRefusal{400, "Parent row is missing. Row cannot be written"}
		case strings.Contains(strings.ToLower(msg), "foreign key"):
			return &cqRefusal{400, "Foreign key constraint is violated"}
		}
	}
	return err
}

// newer reports whether row's change timestamp is after the stored
// row's, or the row is not stored.
func (s *spannerSink) newer(ctx context.Context, fields []cqField, row []any) (bool, error) {
	var key spanner.Key
	var ts any
	for _, k := range s.keys {
		found := false
		for i, f := range fields {
			if f.Name == k {
				v, _, err := spannerValue(f, s.columns[k].typ, row[i])
				if err != nil {
					return false, err
				}
				key, found = append(key, v), true
			}
		}
		if !found {
			return true, nil
		}
	}
	for i, f := range fields {
		if f.Name == s.changeTS {
			ts = row[i]
		}
	}
	if ts == nil {
		return true, nil
	}
	t, err := timestampValue(ts)
	if err != nil {
		return false, err
	}
	r, err := s.client.Single().ReadRow(ctx, s.table, key, []string{s.changeTS})
	if spanner.ErrCode(err) == codes.NotFound {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var old spanner.NullTime
	if err := r.Columns(&old); err != nil {
		return false, err
	}
	return !old.Valid || t.After(old.Time), nil
}

// spannerValue is a result cell as the Spanner client writes it to a
// column of type typ, and its size in bytes.
func spannerValue(f cqField, typ string, v any) (any, int, error) {
	if f.Mode == "REPEATED" {
		arr, _ := v.([]any)
		var out []any
		n := 0
		for _, e := range arr {
			x, k, err := spannerValue(cqField{Name: f.Name, Type: f.Type}, strings.TrimSuffix(strings.TrimPrefix(typ, "ARRAY<"), ">"), e)
			if err != nil {
				return nil, 0, err
			}
			out, n = append(out, x), n+k
		}
		return out, n, nil
	}
	s, _ := v.(string)
	if v == nil {
		switch {
		case strings.HasPrefix(typ, "INT64"), strings.HasPrefix(typ, "ENUM"):
			return spanner.NullInt64{}, 0, nil
		case strings.HasPrefix(typ, "FLOAT32"):
			return spanner.NullFloat32{}, 0, nil
		case strings.HasPrefix(typ, "FLOAT64"):
			return spanner.NullFloat64{}, 0, nil
		case strings.HasPrefix(typ, "BOOL"):
			return spanner.NullBool{}, 0, nil
		case strings.HasPrefix(typ, "TIMESTAMP"):
			return spanner.NullTime{}, 0, nil
		case strings.HasPrefix(typ, "DATE"):
			return spanner.NullDate{}, 0, nil
		case strings.HasPrefix(typ, "NUMERIC"):
			return spanner.NullNumeric{}, 0, nil
		case strings.HasPrefix(typ, "JSON"):
			return spanner.NullJSON{}, 0, nil
		case strings.HasPrefix(typ, "BYTES"), strings.HasPrefix(typ, "PROTO"):
			return []byte(nil), 0, nil
		}
		return spanner.NullString{}, 0, nil
	}
	bad := func(err error) (any, int, error) {
		return nil, 0, &cqRefusal{400, fmt.Sprintf("Column %s of type %s cannot be written to the Spanner column of type %s: %v", f.Name, f.Type, typ, err)}
	}
	switch {
	case strings.HasPrefix(typ, "INT64"), strings.HasPrefix(typ, "ENUM"):
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return bad(err)
		}
		return n, 8, nil
	case strings.HasPrefix(typ, "FLOAT32"):
		x, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return bad(err)
		}
		return float32(x), 4, nil
	case strings.HasPrefix(typ, "FLOAT64"):
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return bad(err)
		}
		return x, 8, nil
	case strings.HasPrefix(typ, "BOOL"):
		return strings.EqualFold(s, "true"), 1, nil
	case strings.HasPrefix(typ, "TIMESTAMP"):
		t, err := timestampValue(v)
		if err != nil {
			return bad(err)
		}
		return t, 12, nil
	case strings.HasPrefix(typ, "DATE"):
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return bad(err)
		}
		return spanner.NullDate{Date: civil.DateOf(d), Valid: true}, 4, nil
	case strings.HasPrefix(typ, "NUMERIC"):
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return bad(errors.New("not a number"))
		}
		return r, 16, nil
	case strings.HasPrefix(typ, "JSON"):
		var x any
		if err := json.Unmarshal([]byte(s), &x); err != nil {
			return bad(err)
		}
		return spanner.NullJSON{Value: x, Valid: true}, len(s), nil
	case strings.HasPrefix(typ, "BYTES"), strings.HasPrefix(typ, "PROTO"):
		b, ok := valueBytes(f, v)
		if !ok {
			return bad(errors.New("not bytes"))
		}
		return b, len(b), nil
	}
	return s, len(s), nil
}
