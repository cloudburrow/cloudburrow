package pubsubfront

// BigQuery subscriptions (#1874), to
// https://docs.cloud.google.com/pubsub/docs/bigquery and
// https://docs.cloud.google.com/pubsub/docs/create-bigquery-subscription:
//
//   - with neither use_topic_schema nor use_table_schema, the table must
//     have "a column called data of type BYTES, STRING, or JSON", and
//     the message data is written there;
//   - use_topic_schema writes the fields of a message in the topic's
//     schema to the matching columns, with Google's type mapping
//     (exportschema.go), checked when the subscription is created;
//   - use_table_schema writes the fields of a JSON message to the
//     matching columns;
//   - write_metadata also writes subscription_name (STRING), message_id
//     (STRING), publish_time (TIMESTAMP) and attributes (STRING or JSON,
//     the attributes as a JSON object);
//   - drop_unknown_fields drops fields with no column; without it, "messages
//     with extra fields are not written to BigQuery and remain in the
//     subscription backlog".
//
// Rows are streamed with tabledata.insertAll, each with the message ID as
// its insertId, so a message delivered again is not written twice.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// bqTable is a table a BigQuery subscription names.
type bqTable struct{ project, dataset, table string }

func (t bqTable) String() string { return t.project + "." + t.dataset + "." + t.table }

// parseTable reads "{project}.{dataset}.{table}" or
// "{project}:{dataset}.{table}".
func parseTable(s string) (bqTable, error) {
	p, rest, ok := strings.Cut(s, ":")
	if !ok {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) != 3 {
			return bqTable{}, apierror.InvalidArgument("Invalid BigQuery table name: %s. It must be of the form {projectId}.{datasetId}.{tableId}.", s)
		}
		p, rest = parts[0], parts[1]+"."+parts[2]
	}
	d, t, ok := strings.Cut(rest, ".")
	if p == "" || !ok || d == "" || t == "" || strings.Contains(t, ".") {
		return bqTable{}, apierror.InvalidArgument("Invalid BigQuery table name: %s. It must be of the form {projectId}.{datasetId}.{tableId}.", s)
	}
	return bqTable{p, d, t}, nil
}

func (e *exporter) tableURL(t bqTable) string {
	return e.bigquery + "/bigquery/v2/projects/" + url.PathEscape(t.project) + "/datasets/" +
		url.PathEscape(t.dataset) + "/tables/" + url.PathEscape(t.table)
}

// tableSchema reads a table's columns.
func (e *exporter) tableSchema(ctx context.Context, t bqTable) ([]bqField, error) {
	if e.bigquery == "" {
		return nil, &errDestination{int32(pubsubpb.BigQueryConfig_PERMISSION_DENIED),
			"Cannot write to the BigQuery table: bigquery.googleapis.com is not enabled for the project."}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.tableURL(t), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, &errDestination{int32(pubsubpb.BigQueryConfig_NOT_FOUND), "Cannot write to the BigQuery table " + t.String() + ": it does not exist."}
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, &errDestination{int32(pubsubpb.BigQueryConfig_PERMISSION_DENIED), "Cannot write to the BigQuery table " + t.String() + ": permission denied."}
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("BigQuery tables.get %s: %s", t, resp.Status)
	}
	var tbl struct {
		Schema struct {
			Fields []bqField `json:"fields"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &tbl); err != nil {
		return nil, err
	}
	return tbl.Schema.Fields, nil
}

func mismatch(format string, a ...any) error {
	return &errDestination{int32(pubsubpb.BigQueryConfig_SCHEMA_MISMATCH), fmt.Sprintf(format, a...)}
}

// metadataColumns are write_metadata's columns and the types each may be.
var metadataColumns = []struct {
	name  string
	types []string
}{
	{"subscription_name", []string{"STRING"}},
	{"message_id", []string{"STRING"}},
	{"publish_time", []string{"TIMESTAMP"}},
	{"attributes", []string{"STRING", "JSON"}},
}

var dataTypes = []string{"BYTES", "STRING", "JSON"}

func hasType(cols []bqField, name string, types []string) bool {
	c, ok := column(cols, name)
	if !ok {
		return false
	}
	for _, t := range types {
		if bqType(c.Type) == t {
			return true
		}
	}
	return false
}

// checkTable checks the table fits the configuration; schema is the
// topic's, for use_topic_schema.
func checkTable(cfg *pubsubpb.BigQueryConfig, cols []bqField, schema *topicMsgSchema) error {
	if !cfg.GetUseTopicSchema() && !cfg.GetUseTableSchema() && !hasType(cols, "data", dataTypes) {
		return mismatch("The BigQuery table must have a column called data of type BYTES, STRING, or JSON.")
	}
	if cfg.GetWriteMetadata() {
		for _, m := range metadataColumns {
			if !hasType(cols, m.name, m.types) {
				return mismatch("With write_metadata, the BigQuery table must have a column %s of type %s.", m.name, strings.Join(m.types, " or "))
			}
		}
	}
	if cfg.GetUseTopicSchema() && schema != nil {
		var last error
		for _, r := range schema.revisions {
			if err := compatible(r.root.fields, metadataFree(cols, cfg), cfg.GetDropUnknownFields(), ""); err != nil {
				last = err
				continue
			}
			return nil
		}
		return mismatch("The topic's schema is not compatible with the BigQuery table: %v.", last)
	}
	return nil
}

// metadataFree are the columns without write_metadata's, whose REQUIRED
// mode the schema need not meet.
func metadataFree(cols []bqField, cfg *pubsubpb.BigQueryConfig) []bqField {
	if !cfg.GetWriteMetadata() {
		return cols
	}
	var out []bqField
	for _, c := range cols {
		meta := false
		for _, m := range metadataColumns {
			if strings.EqualFold(c.Name, m.name) {
				meta = true
			}
		}
		if !meta {
			out = append(out, c)
		}
	}
	return out
}

// checkBigQueryConfig is CreateSubscription's and UpdateSubscription's
// check of a bigquery_config.
func (f *Front) checkBigQueryConfig(ctx context.Context, s *pubsubpb.Subscription) error {
	cfg := s.GetBigqueryConfig()
	if cfg.GetUseTopicSchema() && cfg.GetUseTableSchema() {
		return apierror.InvalidArgument("At most one of use_topic_schema and use_table_schema can be set.")
	}
	if err := checkServiceAccount(cfg.GetServiceAccountEmail()); err != nil {
		return err
	}
	t, err := parseTable(cfg.GetTable())
	if err != nil {
		return err
	}
	f.exp.init()
	var schema *topicMsgSchema
	if cfg.GetUseTopicSchema() {
		schema, err = f.topicMessageSchema(ctx, s.GetTopic())
		if err == errNoTopicSchema {
			return apierror.InvalidArgument("use_topic_schema is set, and the topic %s has no schema.", s.GetTopic())
		}
		if err != nil {
			return nil // the emulator answers for the topic
		}
	}
	cols, err := f.exp.tableSchema(ctx, t)
	if st, ok := destinationState(err); ok {
		switch st {
		case int32(pubsubpb.BigQueryConfig_NOT_FOUND):
			return apierror.NotFound("Cannot find the BigQuery table %s.", t)
		case int32(pubsubpb.BigQueryConfig_PERMISSION_DENIED):
			return apierror.FailedPrecondition("%s", err.Error())
		}
	}
	if err != nil {
		return apierror.Internal(err, "Cannot read the BigQuery table %s.", t)
	}
	if err := checkTable(cfg, cols, schema); err != nil {
		return apierror.InvalidArgument("Incompatible table schema: %s", err.Error())
	}
	return nil
}

// runBigQueryExport delivers s until ctx ends.
func (f *Front) runBigQueryExport(ctx context.Context, s *pubsubpb.Subscription) {
	cfg := s.GetBigqueryConfig()
	t, err := parseTable(cfg.GetTable())
	if err != nil {
		f.exp.setState(s.GetName(), int32(pubsubpb.BigQueryConfig_NOT_FOUND))
		return
	}
	for ctx.Err() == nil {
		var schema *topicMsgSchema
		if cfg.GetUseTopicSchema() {
			schema, err = f.topicMessageSchema(ctx, s.GetTopic())
			if err != nil {
				f.exp.setState(s.GetName(), int32(pubsubpb.BigQueryConfig_SCHEMA_MISMATCH))
				if !sleep(ctx, exportCheck) {
					return
				}
				continue
			}
		}
		cols, err := f.exp.tableSchema(ctx, t)
		if err == nil {
			err = checkTable(cfg, cols, schema)
		}
		if err != nil {
			if st, ok := destinationState(err); ok {
				f.exp.setState(s.GetName(), st)
			}
			if !sleep(ctx, exportCheck) {
				return
			}
			continue
		}
		f.exp.setState(s.GetName(), int32(pubsubpb.BigQueryConfig_ACTIVE))
		ps, err := f.pull(ctx, s.GetName(), 500)
		if err != nil || len(ps) == 0 {
			if !sleep(ctx, exportPoll) {
				return
			}
			continue
		}
		f.writeRows(ctx, s, t, cols, schema, ps)
	}
}

// row is one message's row, or why it has none.
func bqRow(s *pubsubpb.Subscription, cols []bqField, schema *topicMsgSchema, m *pubsubpb.PubsubMessage) (map[string]any, error) {
	cfg := s.GetBigqueryConfig()
	row := map[string]any{}
	switch {
	case cfg.GetUseTopicSchema():
		r, err := schema.decode(m.GetData(), cols)
		if err != nil {
			return nil, err
		}
		row = r
	case cfg.GetUseTableSchema():
		var r map[string]any
		d := json.NewDecoder(bytes.NewReader(m.GetData()))
		d.UseNumber()
		if err := d.Decode(&r); err != nil || r == nil {
			return nil, fmt.Errorf("the message is not a JSON object, which use_table_schema requires")
		}
		row = r
	default:
		c, _ := column(cols, "data")
		switch bqType(c.Type) {
		case "BYTES":
			row["data"] = base64.StdEncoding.EncodeToString(m.GetData())
		case "JSON":
			if !json.Valid(m.GetData()) {
				return nil, fmt.Errorf("the message data is not valid JSON, which the JSON column data requires")
			}
			row["data"] = string(m.GetData())
		default:
			row["data"] = string(m.GetData())
		}
	}
	if err := fitColumns(row, cols, cfg.GetDropUnknownFields() && (cfg.GetUseTopicSchema() || cfg.GetUseTableSchema()), ""); err != nil {
		return nil, err
	}
	if cfg.GetWriteMetadata() {
		attrs := m.GetAttributes()
		if attrs == nil {
			attrs = map[string]string{}
		}
		a, _ := json.Marshal(attrs)
		row["subscription_name"] = s.GetName()
		row["message_id"] = m.GetMessageId()
		row["publish_time"] = m.GetPublishTime().AsTime().UTC().Format(time.RFC3339Nano)
		row["attributes"] = string(a)
	}
	return row, nil
}

// fitColumns drops, or refuses, the fields of a row that have no column,
// and writes JSON columns' values as JSON text.
func fitColumns(row map[string]any, cols []bqField, drop bool, path string) error {
	for k, v := range row {
		c, ok := column(cols, k)
		if !ok {
			if drop {
				delete(row, k)
				continue
			}
			return fmt.Errorf("field %s%s is not a column of the table, and drop_unknown_fields is not set", path, k)
		}
		switch bqType(c.Type) {
		case "JSON":
			if _, isString := v.(string); !isString {
				b, err := json.Marshal(v)
				if err != nil {
					return err
				}
				row[k] = string(b)
			}
		case "RECORD":
			if err := fitRecords(v, c.Fields, drop, path+k+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func fitRecords(v any, cols []bqField, drop bool, path string) error {
	switch x := v.(type) {
	case map[string]any:
		return fitColumns(x, cols, drop, path)
	case []any:
		for _, e := range x {
			if err := fitRecords(e, cols, drop, path); err != nil {
				return err
			}
		}
	}
	return nil
}

type insertRow struct {
	InsertID string         `json:"insertId"`
	JSON     map[string]any `json:"json"`
}

// insert streams rows into the table; it returns each refused row's
// reason by index, or an error for the whole request.
func (e *exporter) insert(ctx context.Context, t bqTable, rows []insertRow) (map[int]string, error) {
	body, err := json.Marshal(map[string]any{"rows": rows})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tableURL(t)+"/insertAll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, &errDestination{int32(pubsubpb.BigQueryConfig_NOT_FOUND), "Cannot write to the BigQuery table " + t.String() + ": it does not exist."}
	case resp.StatusCode/100 != 2:
		_, msg, _, _ := apierror.DecodeJSON(bytes.NewReader(rb))
		if len(rows) == 1 && resp.StatusCode == http.StatusBadRequest {
			return map[int]string{0: msg}, nil
		}
		return nil, fmt.Errorf("BigQuery insertAll %s: %s: %s", t, resp.Status, msg)
	}
	var r struct {
		InsertErrors []struct {
			Index  int `json:"index"`
			Errors []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"insertErrors"`
	}
	_ = json.Unmarshal(rb, &r)
	out := map[int]string{}
	for _, ie := range r.InsertErrors {
		var msgs []string
		for _, er := range ie.Errors {
			msgs = append(msgs, strings.TrimSpace(er.Reason+": "+er.Message))
		}
		out[ie.Index] = strings.Join(msgs, "; ")
	}
	return out, nil
}

// writeRows writes the messages pulled, acknowledges those written and
// fails the others.
func (f *Front) writeRows(ctx context.Context, s *pubsubpb.Subscription, t bqTable, cols []bqField, schema *topicMsgSchema, ps []pulled) {
	var rows []insertRow
	var held []pulled
	var bad []failed
	for _, p := range ps {
		row, err := bqRow(s, cols, schema, p.msg)
		if err != nil {
			bad = append(bad, failed{p, err.Error()})
			continue
		}
		rows = append(rows, insertRow{InsertID: p.msg.GetMessageId(), JSON: row})
		held = append(held, p)
	}
	var ok []pulled
	if len(rows) > 0 {
		refused, err := f.exp.insert(ctx, t, rows)
		if err != nil {
			if st, isDest := destinationState(err); isDest {
				f.exp.setState(s.GetName(), st)
			}
			f.logf("pubsub front: export %s: %v", s.GetName(), err)
			var ids []string
			for _, p := range held {
				ids = append(ids, p.ackID)
			}
			f.extend(ctx, s.GetName(), ids, 0)
			f.fail(ctx, s, bad)
			return
		}
		if len(refused) > 0 && len(rows) > 1 {
			// One refused row stops the others: write each alone.
			for i, r := range rows {
				one, err := f.exp.insert(ctx, t, []insertRow{r})
				switch {
				case err != nil:
					bad = append(bad, failed{held[i], err.Error()})
				case len(one) > 0:
					bad = append(bad, failed{held[i], one[0]})
				default:
					ok = append(ok, held[i])
				}
			}
		} else {
			for i, p := range held {
				if reason, no := refused[i]; no {
					bad = append(bad, failed{p, reason})
				} else {
					ok = append(ok, p)
				}
			}
		}
	}
	var ids []string
	for _, p := range ok {
		ids = append(ids, p.ackID)
	}
	f.ack(ctx, s.GetName(), ids)
	f.exp.forgetAttempts(s.GetName(), ok)
	f.fail(ctx, s, bad)
}
