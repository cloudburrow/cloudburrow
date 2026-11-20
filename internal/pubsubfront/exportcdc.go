package pubsubfront

// Change data capture of BigQuery subscriptions (#1999), to
// https://docs.cloud.google.com/pubsub/docs/bigquery ("Change data
// capture"):
//
//   - with use_topic_schema or use_table_schema, a message whose fields
//     include _CHANGE_TYPE ("A string field set to UPSERT or DELETE") is a
//     change: an UPSERT "updates the row with the same key if it exists or
//     inserts a new row if it does not", a DELETE "deletes the row in the
//     table with the same key if it exists";
//   - _CHANGE_SEQUENCE_NUMBER (optional) orders a key's changes: "Messages
//     with sequence numbers that are less than the highest sequence number
//     processed for a row don't have any effect on the row";
//   - "the BigQuery table must have a primary key".
//
// With neither option the message data is written to the data column as it
// is, _CHANGE_TYPE and all: there is no change data capture.
//
// As Google's subscriptions do, the changes are written with the BigQuery
// Storage Write API, to the table's _default stream in the protobuf format
// with the two pseudo-columns, which the local BigQuery applies by primary
// key (internal/bigqueryfront/storagewritecdc.go). A use_topic_schema
// subscription whose schema has _CHANGE_TYPE, of a table without a primary
// key, is SCHEMA_MISMATCH (refused INVALID_ARGUMENT when created); a
// use_table_schema message with _CHANGE_TYPE to such a table is not
// written, and stays in the backlog (or is dead-lettered) like any message
// BigQuery refuses.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	storage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	changeTypeField = "_CHANGE_TYPE"
	changeSeqField  = "_CHANGE_SEQUENCE_NUMBER"
)

// noPrimaryKey is the reason a change cannot be written to a table
// without a primary key, in the words of Google's page.
const noPrimaryKey = "When using BigQuery CDC ingestion, the BigQuery table must have a primary key."

// isChangeField reports whether name is one of the two pseudo-columns.
func isChangeField(name string) bool {
	return strings.EqualFold(name, changeTypeField) || strings.EqualFold(name, changeSeqField)
}

// msgChange is a message's pseudo-columns.
type msgChange struct {
	typ, seq string
	hasSeq   bool
}

// takeChange removes the pseudo-columns from a decoded row: nil when it
// has none.
func takeChange(row map[string]any) (*msgChange, error) {
	var ch *msgChange
	for k, v := range row {
		if !isChangeField(k) {
			continue
		}
		delete(row, k)
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("the field %s must be a string", k)
		}
		if ch == nil {
			ch = &msgChange{}
		}
		if strings.EqualFold(k, changeTypeField) {
			ch.typ = s
		} else {
			ch.seq, ch.hasSeq = s, true
		}
	}
	if ch != nil && ch.typ == "" {
		return nil, fmt.Errorf("_CHANGE_TYPE (required) is not set: change data capture needs it set to UPSERT or DELETE")
	}
	return ch, nil
}

// schemaChanges reports whether a topic schema's newest revision has
// _CHANGE_TYPE, and checks the pseudo-columns are strings.
func schemaChanges(schema *topicMsgSchema) (bool, error) {
	if schema == nil || len(schema.revisions) == 0 || schema.revisions[0].root == nil {
		return false, nil
	}
	has := false
	for _, m := range schema.revisions[0].root.fields {
		if !isChangeField(m.name) {
			continue
		}
		if m.repeated || m.node == nil || m.node.kind != "string" || m.node.logical != "" {
			return false, fmt.Errorf("the field %s must be a string", m.name)
		}
		if strings.EqualFold(m.name, changeTypeField) {
			has = true
		}
	}
	return has, nil
}

// cdcRow is one change to write.
type cdcRow struct {
	row map[string]any
	ch  *msgChange
}

// storageWriter is the exporter's Storage Write API client, made once.
type storageWriter struct {
	once   sync.Once
	client *storage.BigQueryWriteClient
	err    error
}

// UseBigQueryStorage names the instance's BigQuery Storage API (host:port,
// gRPC without TLS), which BigQuery subscriptions write changes to (#1999).
// Call it before Serve.
func (f *Front) UseBigQueryStorage(addr string) { f.exp.bigqueryStorage = addr }

func (e *exporter) writeClient(ctx context.Context) (*storage.BigQueryWriteClient, error) {
	e.writer.once.Do(func() {
		if e.bigqueryStorage == "" {
			e.writer.err = fmt.Errorf("the instance's BigQuery Storage API is not known to the Pub/Sub front")
			return
		}
		// Never Google's endpoint: the instance's, without credentials.
		e.writer.client, e.writer.err = storage.NewBigQueryWriteClient(context.WithoutCancel(ctx),
			option.WithEndpoint(e.bigqueryStorage),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	})
	return e.writer.client, e.writer.err
}

// writeChanges appends the changes to the table's _default stream; it
// returns each refused change's reason by index, or an error for the whole
// request that a later delivery may get past.
func (e *exporter) writeChanges(ctx context.Context, t bqTable, cols []bqField, key []string, rows []cdcRow) (map[int]string, error) {
	refused := map[int]string{}
	if len(key) == 0 {
		for i := range rows {
			refused[i] = noPrimaryKey
		}
		return refused, nil
	}
	dp, md, err := changeDescriptor(cols)
	if err != nil {
		for i := range rows {
			refused[i] = err.Error()
		}
		return refused, nil
	}
	var serialized [][]byte
	var index []int
	for i, r := range rows {
		b, err := encodeChange(md, cols, r)
		if err != nil {
			refused[i] = err.Error()
			continue
		}
		serialized = append(serialized, b)
		index = append(index, i)
	}
	if len(serialized) == 0 {
		return refused, nil
	}
	client, err := e.writeClient(ctx)
	if err != nil {
		return nil, err
	}
	name := "projects/" + t.project + "/datasets/" + t.dataset + "/tables/" + t.table + "/streams/_default"
	rowErrs, whole, err := appendChanges(ctx, client, name, dp, serialized)
	if err != nil {
		return nil, err
	}
	switch {
	case whole != "" && len(rowErrs) == 0:
		for _, i := range index {
			refused[i] = whole
		}
	case len(rowErrs) > 0 && len(serialized) == 1:
		refused[index[0]] = rowErrs[0]
	case len(rowErrs) > 0:
		// The append is all or nothing: one refused row stops the others,
		// so write each alone, in order.
		for k, b := range serialized {
			one, whole, err := appendChanges(ctx, client, name, dp, [][]byte{b})
			switch {
			case err != nil:
				refused[index[k]] = err.Error()
			case len(one) > 0:
				refused[index[k]] = one[0]
			case whole != "":
				refused[index[k]] = whole
			}
		}
	}
	return refused, nil
}

// appendChanges makes one append of rows to the stream name: its row
// errors by index, or the append's error, or an error that is no answer.
func appendChanges(ctx context.Context, client *storage.BigQueryWriteClient, name string, dp *descriptorpb.DescriptorProto,
	rows [][]byte) (map[int]string, string, error) {
	if p, _ := ctx.Value(consumerKey{}).(string); p != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-goog-user-project", p)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stream, err := client.AppendRows(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := stream.Send(&storagepb.AppendRowsRequest{
		WriteStream: name,
		Rows: &storagepb.AppendRowsRequest_ProtoRows{ProtoRows: &storagepb.AppendRowsRequest_ProtoData{
			WriterSchema: &storagepb.ProtoSchema{ProtoDescriptor: dp},
			Rows:         &storagepb.ProtoRows{SerializedRows: rows},
		}},
	}); err != nil {
		return nil, "", err
	}
	resp, err := stream.Recv()
	_ = stream.CloseSend()
	if err != nil {
		return nil, "", err
	}
	st := resp.GetError()
	if st == nil {
		return nil, "", nil
	}
	out := map[int]string{}
	for _, re := range resp.GetRowErrors() {
		out[int(re.GetIndex())] = re.GetMessage()
	}
	return out, st.GetMessage(), nil
}

// protoName is a valid proto field name.
var protoName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// changeDescriptor is the writer schema of a table's changes: a field for
// each column (its name, or the column_name annotation BigQuery reads for
// one that is no proto name), and the two pseudo-columns.
func changeDescriptor(cols []bqField) (*descriptorpb.DescriptorProto, protoreflect.MessageDescriptor, error) {
	dp, err := messageDescriptor("cdc_row", ".cdc_row", cols)
	if err != nil {
		return nil, nil, err
	}
	n := int32(len(dp.Field))
	for i, name := range []string{changeTypeField, changeSeqField} {
		dp.Field = append(dp.Field, &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(n + int32(i) + 1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		})
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("cdc_row.proto"),
		Syntax:      proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{dp},
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	return dp, fd.Messages().ByName("cdc_row"), nil
}

func messageDescriptor(name, full string, cols []bqField) (*descriptorpb.DescriptorProto, error) {
	dp := &descriptorpb.DescriptorProto{Name: proto.String(name)}
	for i, c := range cols {
		f := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(c.Name),
			Number: proto.Int32(int32(i) + 1),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
		if !protoName.MatchString(c.Name) {
			f.Name = proto.String(fmt.Sprintf("col_%d", i+1))
			opts := &descriptorpb.FieldOptions{}
			proto.SetExtension(opts, storagepb.E_ColumnName, c.Name)
			f.Options = opts
		}
		if strings.EqualFold(c.Mode, "REPEATED") {
			f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}
		switch bqType(c.Type) {
		case "INTEGER", "TIMESTAMP":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
		case "FLOAT":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()
		case "BOOLEAN":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()
		case "BYTES":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()
		case "RECORD":
			subName := fmt.Sprintf("R%d", i+1)
			sub, err := messageDescriptor(subName, full+"."+subName, c.Fields)
			if err != nil {
				return nil, err
			}
			dp.NestedType = append(dp.NestedType, sub)
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			f.TypeName = proto.String(full + "." + subName)
		case "RANGE":
			return nil, fmt.Errorf("CloudBurrow does not write changes to a table with the RANGE column %s", c.Name)
		default: // STRING, JSON, GEOGRAPHY, NUMERIC, BIGNUMERIC, DATE, DATETIME, TIME, INTERVAL
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
		}
		dp.Field = append(dp.Field, f)
	}
	return dp, nil
}

// encodeChange is one change as a serialized row of md.
func encodeChange(md protoreflect.MessageDescriptor, cols []bqField, r cdcRow) ([]byte, error) {
	m := dynamicpb.NewMessage(md)
	if err := fillMessage(m, cols, r.row, ""); err != nil {
		return nil, err
	}
	m.Set(md.Fields().ByName(changeTypeField), protoreflect.ValueOfString(r.ch.typ))
	if r.ch.hasSeq {
		m.Set(md.Fields().ByName(changeSeqField), protoreflect.ValueOfString(r.ch.seq))
	}
	return proto.Marshal(m)
}

func fillMessage(m protoreflect.Message, cols []bqField, row map[string]any, path string) error {
	fields := m.Descriptor().Fields()
	for i, c := range cols {
		v, ok := valueOf(row, c.Name)
		if !ok || v == nil {
			continue
		}
		fd := fields.ByNumber(protoreflect.FieldNumber(i + 1))
		if fd.IsList() {
			items, _ := v.([]any)
			l := m.Mutable(fd).List()
			for _, x := range items {
				pv, err := protoValue(m, fd, c, x, path)
				if err != nil {
					return err
				}
				l.Append(pv)
			}
			continue
		}
		pv, err := protoValue(m, fd, c, v, path)
		if err != nil {
			return err
		}
		m.Set(fd, pv)
	}
	return nil
}

func valueOf(row map[string]any, name string) (any, bool) {
	if v, ok := row[name]; ok {
		return v, true
	}
	for k, v := range row {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// protoValue is a row value (as insertAll takes it) as fd's value.
func protoValue(m protoreflect.Message, fd protoreflect.FieldDescriptor, c bqField, v any, path string) (protoreflect.Value, error) {
	where := path + c.Name
	switch bqType(c.Type) {
	case "RECORD":
		sub, ok := v.(map[string]any)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("field %s is not a record", where)
		}
		var nm protoreflect.Message
		if fd.IsList() {
			nm = m.Mutable(fd).List().NewElement().Message()
		} else {
			nm = m.NewField(fd).Message()
		}
		if err := fillMessage(nm, c.Fields, sub, where+"."); err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(nm), nil
	case "INTEGER":
		n, err := strconv.ParseInt(text(v), 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(text(v), 64)
			if ferr != nil || f != math.Trunc(f) {
				return protoreflect.Value{}, fmt.Errorf("field %s: %v is not an integer", where, v)
			}
			n = int64(f)
		}
		return protoreflect.ValueOfInt64(n), nil
	case "FLOAT":
		f, err := strconv.ParseFloat(text(v), 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("field %s: %v is not a number", where, v)
		}
		return protoreflect.ValueOfFloat64(f), nil
	case "BOOLEAN":
		b, err := strconv.ParseBool(text(v))
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("field %s: %v is not a boolean", where, v)
		}
		return protoreflect.ValueOfBool(b), nil
	case "BYTES":
		b, err := base64.StdEncoding.DecodeString(text(v))
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("field %s: %v is not base64", where, v)
		}
		return protoreflect.ValueOfBytes(b), nil
	case "TIMESTAMP":
		us, err := timestampMicros(v)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("field %s: %v", where, err)
		}
		return protoreflect.ValueOfInt64(us), nil
	}
	if _, isText := v.(string); !isText {
		if b, err := json.Marshal(v); err == nil && bqType(c.Type) == "JSON" {
			return protoreflect.ValueOfString(string(b)), nil
		}
	}
	return protoreflect.ValueOfString(text(v)), nil
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// timestampLayouts are the TIMESTAMP texts a row may give.
var timestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999 MST",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02",
}

// timestampMicros is a TIMESTAMP value, text or seconds since the epoch
// (as tabledata.insertAll takes a number), in microseconds.
func timestampMicros(v any) (int64, error) {
	s := text(v)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(math.Round(f * 1e6)), nil
	}
	for _, l := range timestampLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UnixMicro(), nil
		}
	}
	return 0, fmt.Errorf("%q is not a TIMESTAMP", s)
}
