package pubsubfront

// A topic's schema, as a BigQuery subscription with use_topic_schema maps
// it to a table (#1874), to Google's type mapping
// (https://docs.cloud.google.com/pubsub/docs/bigquery, "Schema
// compatibility"): every field of the schema must be a column of a type
// Google lists for it, unless drop_unknown_fields; a field's repetition
// must be the column's; a union that is not of null and one type, and a
// oneof, map to no column.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// bqField is a column of a BigQuery table's schema, as tables.get gives it.
type bqField struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Mode   string    `json:"mode,omitempty"`
	Fields []bqField `json:"fields,omitempty"`
}

// bqType is a column's type by its legacy name.
func bqType(t string) string {
	switch strings.ToUpper(t) {
	case "INT64":
		return "INTEGER"
	case "FLOAT64":
		return "FLOAT"
	case "BOOL":
		return "BOOLEAN"
	case "STRUCT":
		return "RECORD"
	}
	return strings.ToUpper(t)
}

func column(fields []bqField, name string) (bqField, bool) {
	for _, c := range fields {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return bqField{}, false
}

// schemaNode is one type of a topic's schema, Avro or Protocol Buffer.
type schemaNode struct {
	// kind is the Avro type name, or "message", "map", and the proto kinds.
	kind     string
	logical  string
	fields   []schemaMember
	items    *schemaNode // array or repeated element, or map value
	key      *schemaNode // proto map key
	symbols  []string
	branches []*schemaNode
	scale    int
	// unmappable says why the type maps to no column.
	unmappable string
}

type schemaMember struct {
	name     string
	node     *schemaNode
	repeated bool
	nullable bool
}

// allowed are the BigQuery types Google maps a schema type to.
func (n *schemaNode) allowed() []string {
	num := []string{"NUMERIC", "BIGNUMERIC"}
	switch n.logical {
	case "timestamp-micros", "timestamp-millis":
		return []string{"TIMESTAMP"}
	case "date":
		return []string{"DATE"}
	case "time-micros", "time-millis":
		return []string{"TIME"}
	case "duration":
		return []string{"INTERVAL"}
	case "decimal":
		return num
	}
	switch n.kind {
	case "boolean", "bool":
		return []string{"BOOLEAN"}
	case "int", "long":
		return append([]string{"INTEGER"}, num...)
	case "int32", "sint32", "sfixed32", "uint32", "fixed32":
		return append([]string{"INTEGER"}, append(num, "DATE")...)
	case "int64", "sint64", "sfixed64":
		return append([]string{"INTEGER"}, append(num, "DATE", "DATETIME", "TIMESTAMP")...)
	case "uint64", "fixed64":
		return num
	case "float", "double":
		return append([]string{"FLOAT"}, num...)
	case "bytes", "fixed":
		return append([]string{"BYTES"}, num...)
	case "string":
		return append([]string{"STRING", "JSON", "TIMESTAMP", "DATETIME", "DATE", "TIME"}, num...)
	case "enum":
		return []string{"INTEGER"}
	case "record", "message", "map":
		return []string{"RECORD"}
	}
	return nil
}

// compatible checks the members against the table's columns.
func compatible(members []schemaMember, cols []bqField, drop bool, path string) error {
	for _, m := range members {
		c, ok := column(cols, m.name)
		if !ok {
			if drop {
				continue
			}
			return fmt.Errorf("field %s%s is not a column of the table", path, m.name)
		}
		if m.node.unmappable != "" {
			return fmt.Errorf("field %s%s: %s", path, m.name, m.node.unmappable)
		}
		repeated := strings.EqualFold(c.Mode, "REPEATED")
		if m.repeated != repeated {
			return fmt.Errorf("field %s%s is %s, and column %s is %s", path, m.name, rep(m.repeated), c.Name, rep(repeated))
		}
		if m.nullable && strings.EqualFold(c.Mode, "REQUIRED") {
			return fmt.Errorf("field %s%s is nullable, and column %s is REQUIRED", path, m.name, c.Name)
		}
		node := m.node
		if node.kind == "map" {
			if bqType(c.Type) != "RECORD" {
				return fmt.Errorf("field %s%s is a map, and column %s is %s", path, m.name, c.Name, c.Type)
			}
			kv := []schemaMember{{name: "key", node: node.key}, {name: "value", node: node.items}}
			if err := compatible(kv, c.Fields, drop, path+m.name+"."); err != nil {
				return err
			}
			continue
		}
		ok = false
		for _, t := range node.allowed() {
			if t == bqType(c.Type) {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("field %s%s (%s) cannot be written to column %s of type %s", path, m.name, node.name(), c.Name, c.Type)
		}
		if node.kind == "record" || node.kind == "message" {
			if err := compatible(node.fields, c.Fields, drop, path+m.name+"."); err != nil {
				return err
			}
		}
	}
	for _, c := range cols {
		if !strings.EqualFold(c.Mode, "REQUIRED") {
			continue
		}
		found := false
		for _, m := range members {
			if strings.EqualFold(m.name, c.Name) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("column %s%s is REQUIRED and not a field of the schema", path, c.Name)
		}
	}
	return nil
}

func rep(r bool) string {
	if r {
		return "repeated"
	}
	return "not repeated"
}

func (n *schemaNode) name() string {
	if n.logical != "" {
		return n.kind + "." + n.logical
	}
	return n.kind
}

// topicSchema is a topic's schema and its messages' encoding.
type topicMsgSchema struct {
	enc pubsubpb.Encoding
	// revisions are the schema's revisions a message may be in, newest
	// first.
	revisions []schemaRevision
}

type schemaRevision struct {
	root  *schemaNode
	codec *goavro.Codec
	md    protoreflect.MessageDescriptor
}

// errNoTopicSchema is a topic with no schema.
var errNoTopicSchema = errors.New("the topic has no schema")

// topicMessageSchema reads a topic's schema, from the front for a
// Protocol Buffer one and from the emulator for an Avro one.
func (f *Front) topicMessageSchema(ctx context.Context, topic string) (*topicMsgSchema, error) {
	ss, ok := f.topicSchema(topic)
	var revs []*pubsubpb.Schema
	if ok {
		kept, found := f.keptSchema(ss.GetSchema())
		if !found {
			return nil, errNoTopicSchema
		}
		revs = kept
	} else {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		t, err := f.topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
		if err != nil {
			return nil, err
		}
		ss = t.GetSchemaSettings()
		if ss.GetSchema() == "" || ss.GetSchema() == deletedSchemaName {
			return nil, errNoTopicSchema
		}
		revs, err = schemaRevisions(ctx, pubsubpb.NewSchemaServiceClient(f.upstream), ss.GetSchema())
		if err != nil {
			return nil, err
		}
	}
	out := &topicMsgSchema{enc: ss.GetEncoding()}
	in := ss.GetFirstRevisionId() == ""
	var picked []*pubsubpb.Schema
	for _, r := range revs {
		if r.GetRevisionId() == ss.GetFirstRevisionId() {
			in = true
		}
		if in {
			picked = append(picked, r)
		}
		if r.GetRevisionId() == ss.GetLastRevisionId() {
			break
		}
	}
	for i := len(picked) - 1; i >= 0; i-- {
		r := picked[i]
		var rev schemaRevision
		if r.GetType() == pubsubpb.Schema_PROTOCOL_BUFFER {
			md, err := parseProtoSchema(r.GetDefinition())
			if err != nil {
				continue
			}
			rev.md, rev.root = md, protoNode(md)
		} else {
			codec, err := goavro.NewCodec(r.GetDefinition())
			if err != nil {
				continue
			}
			root, err := parseAvroSchema(r.GetDefinition())
			if err != nil {
				continue
			}
			rev.codec, rev.root = codec, root
		}
		out.revisions = append(out.revisions, rev)
	}
	if len(out.revisions) == 0 {
		return nil, errNoTopicSchema
	}
	return out, nil
}

// parseAvroSchema reads an Avro schema into a schemaNode tree.
func parseAvroSchema(def string) (*schemaNode, error) {
	var raw any
	if err := json.Unmarshal([]byte(def), &raw); err != nil {
		return nil, err
	}
	names := map[string]*schemaNode{}
	return avroNode(raw, "", names)
}

func avroNode(raw any, ns string, names map[string]*schemaNode) (*schemaNode, error) {
	switch v := raw.(type) {
	case string:
		switch v {
		case "null", "boolean", "int", "long", "float", "double", "bytes", "string":
			return &schemaNode{kind: v}, nil
		}
		if n, ok := names[v]; ok {
			return n, nil
		}
		if n, ok := names[ns+"."+v]; ok {
			return n, nil
		}
		return nil, fmt.Errorf("unknown Avro type %q", v)
	case []any:
		n := &schemaNode{kind: "union"}
		for _, b := range v {
			bn, err := avroNode(b, ns, names)
			if err != nil {
				return nil, err
			}
			n.branches = append(n.branches, bn)
		}
		return n, nil
	case map[string]any:
		t, _ := v["type"].(string)
		logical, _ := v["logicalType"].(string)
		if sub, ok := v["type"].(map[string]any); ok {
			return avroNode(sub, ns, names)
		}
		if sub, ok := v["type"].([]any); ok {
			return avroNode(sub, ns, names)
		}
		register := func(n *schemaNode) {
			name, _ := v["name"].(string)
			space, _ := v["namespace"].(string)
			if space == "" {
				space = ns
			}
			names[name] = n
			if space != "" {
				names[space+"."+name] = n
			}
		}
		switch t {
		case "record", "error":
			n := &schemaNode{kind: "record"}
			register(n)
			space, _ := v["namespace"].(string)
			if space == "" {
				space = ns
			}
			fs, _ := v["fields"].([]any)
			for _, fr := range fs {
				fm, _ := fr.(map[string]any)
				name, _ := fm["name"].(string)
				fn, err := avroNode(fm["type"], space, names)
				if err != nil {
					return nil, err
				}
				n.fields = append(n.fields, avroMember(name, fn))
			}
			return n, nil
		case "enum":
			n := &schemaNode{kind: "enum"}
			for _, s := range anySlice(v["symbols"]) {
				str, _ := s.(string)
				n.symbols = append(n.symbols, str)
			}
			register(n)
			return n, nil
		case "fixed":
			n := &schemaNode{kind: "fixed", logical: logical}
			register(n)
			return n, nil
		case "array":
			items, err := avroNode(v["items"], ns, names)
			if err != nil {
				return nil, err
			}
			return &schemaNode{kind: "array", items: items}, nil
		case "map":
			values, err := avroNode(v["values"], ns, names)
			if err != nil {
				return nil, err
			}
			return &schemaNode{kind: "map", key: &schemaNode{kind: "string"}, items: values}, nil
		default:
			n, err := avroNode(t, ns, names)
			if err != nil {
				return nil, err
			}
			c := *n
			c.logical = logical
			if s, ok := v["scale"].(float64); ok {
				c.scale = int(s)
			}
			return &c, nil
		}
	}
	return nil, fmt.Errorf("unreadable Avro schema")
}

func anySlice(v any) []any { s, _ := v.([]any); return s }

// avroMember is a record field: an array is a repeated column, and a union
// of null and one type a nullable one.
func avroMember(name string, n *schemaNode) schemaMember {
	m := schemaMember{name: name, node: n}
	if n.kind == "union" {
		var other []*schemaNode
		for _, b := range n.branches {
			if b.kind != "null" {
				other = append(other, b)
			}
		}
		if len(other) == 1 && len(n.branches) == 2 {
			m.node, m.nullable = other[0], true
		} else {
			m.node = &schemaNode{kind: "union", unmappable: "a union that is not of null and one type is unmappable"}
		}
	}
	if m.node.kind == "array" {
		m.node, m.repeated = m.node.items, true
	}
	if m.node.kind == "map" {
		m.repeated = true
	}
	return m
}

// protoNode is a Protocol Buffer message as a schemaNode.
func protoNode(md protoreflect.MessageDescriptor) *schemaNode {
	n := &schemaNode{kind: "message"}
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		m := schemaMember{name: string(fd.Name()), node: protoFieldNode(fd)}
		if fd.ContainingOneof() != nil && !fd.ContainingOneof().IsSynthetic() {
			m.node = &schemaNode{kind: "oneof", unmappable: "a oneof is unmappable"}
		}
		m.repeated = fd.IsList() || fd.IsMap()
		n.fields = append(n.fields, m)
	}
	return n
}

func protoFieldNode(fd protoreflect.FieldDescriptor) *schemaNode {
	if fd.IsMap() {
		return &schemaNode{kind: "map", key: protoFieldNode(fd.MapKey()), items: protoFieldNode(fd.MapValue())}
	}
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return protoNode(fd.Message())
	case protoreflect.EnumKind:
		return &schemaNode{kind: "enum"}
	}
	return &schemaNode{kind: fd.Kind().String()}
}

// decode reads a message's data in the topic's schema, as the row the
// columns get; it tries each revision, newest first.
func (s *topicMsgSchema) decode(data []byte, cols []bqField) (map[string]any, error) {
	var last error
	for _, r := range s.revisions {
		row, err := r.decode(data, s.enc, cols)
		if err == nil {
			return row, nil
		}
		last = err
	}
	return nil, last
}

func (r schemaRevision) decode(data []byte, enc pubsubpb.Encoding, cols []bqField) (map[string]any, error) {
	if r.md != nil {
		m := dynamicpb.NewMessage(r.md)
		var err error
		if enc == pubsubpb.Encoding_JSON {
			err = protojson.Unmarshal(data, m)
		} else {
			err = proto.Unmarshal(data, m)
		}
		if err != nil {
			return nil, fmt.Errorf("the message does not match the topic's schema: %v", err)
		}
		return protoRow(m, cols), nil
	}
	var native any
	var err error
	if enc == pubsubpb.Encoding_JSON {
		native, _, err = r.codec.NativeFromTextual(data)
	} else {
		native, _, err = r.codec.NativeFromBinary(data)
	}
	if err != nil {
		return nil, fmt.Errorf("the message does not match the topic's schema: %v", err)
	}
	rec, _ := native.(map[string]any)
	return avroRecord(r.root, rec, cols), nil
}

func avroRecord(n *schemaNode, rec map[string]any, cols []bqField) map[string]any {
	out := map[string]any{}
	for _, f := range n.fields {
		v, ok := rec[f.name]
		if !ok || v == nil {
			continue
		}
		c, _ := column(cols, f.name)
		out[f.name] = avroValue(f, v, c)
	}
	return out
}

func avroValue(m schemaMember, v any, c bqField) any {
	if u, ok := v.(map[string]any); ok && m.nullable {
		for _, x := range u {
			v = x
		}
	}
	if m.node.kind == "map" {
		var out []any
		for k, x := range v.(map[string]any) {
			vc, _ := column(c.Fields, "value")
			out = append(out, map[string]any{"key": k, "value": avroValue(schemaMember{node: m.node.items}, x, vc)})
		}
		return out
	}
	if m.repeated {
		var out []any
		for _, x := range anySlice(v) {
			out = append(out, avroScalar(m.node, x, c))
		}
		return out
	}
	return avroScalar(m.node, v, c)
}

func avroScalar(n *schemaNode, v any, c bqField) any {
	if u, ok := v.(map[string]any); ok && n.kind != "record" {
		for _, x := range u {
			v = x
		}
	}
	switch x := v.(type) {
	case time.Time:
		switch n.logical {
		case "date":
			return x.UTC().Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case time.Duration:
		if n.logical == "time-millis" || n.logical == "time-micros" {
			d := x
			h := d / time.Hour
			d -= h * time.Hour
			mi := d / time.Minute
			d -= mi * time.Minute
			sec := d / time.Second
			d -= sec * time.Second
			return fmt.Sprintf("%02d:%02d:%02d.%06d", h, mi, sec, d/time.Microsecond)
		}
		return x.String()
	case *big.Rat:
		return x.FloatString(n.scale)
	case []byte:
		if t := bqType(c.Type); t == "NUMERIC" || t == "BIGNUMERIC" {
			return new(big.Int).SetBytes(x).String()
		}
		return base64.StdEncoding.EncodeToString(x)
	case string:
		if n.kind == "enum" {
			for i, s := range n.symbols {
				if s == x {
					return i
				}
			}
		}
		return x
	case map[string]any:
		return avroRecord(n, x, c.Fields)
	}
	return v
}

// protoRow is a Protocol Buffer message as a row: 64-bit integers as
// decimal strings, enums by number, bytes in base64, maps as repeated
// key/value records.
func protoRow(m protoreflect.Message, cols []bqField) map[string]any {
	out := map[string]any{}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		c, _ := column(cols, string(fd.Name()))
		switch {
		case fd.IsMap():
			var rows []any
			kc, _ := column(c.Fields, "key")
			vc, _ := column(c.Fields, "value")
			v.Map().Range(func(k protoreflect.MapKey, x protoreflect.Value) bool {
				rows = append(rows, map[string]any{"key": protoScalar(fd.MapKey(), k.Value(), kc),
					"value": protoScalar(fd.MapValue(), x, vc)})
				return true
			})
			out[string(fd.Name())] = rows
		case fd.IsList():
			var rows []any
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				rows = append(rows, protoScalar(fd, l.Get(i), c))
			}
			out[string(fd.Name())] = rows
		default:
			out[string(fd.Name())] = protoScalar(fd, v, c)
		}
		return true
	})
	return out
}

func protoScalar(fd protoreflect.FieldDescriptor, v protoreflect.Value, c bqField) any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return protoRow(v.Message(), c.Fields)
	case protoreflect.EnumKind:
		return int64(v.Enum())
	case protoreflect.BytesKind:
		if t := bqType(c.Type); t == "NUMERIC" || t == "BIGNUMERIC" {
			return new(big.Int).SetBytes(v.Bytes()).String()
		}
		return base64.StdEncoding.EncodeToString(v.Bytes())
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10)
	}
	n := v.Int()
	switch bqType(c.Type) {
	case "DATE":
		return time.Unix(n*86400, 0).UTC().Format("2006-01-02")
	case "TIMESTAMP":
		return time.UnixMicro(n).UTC().Format(time.RFC3339Nano)
	case "DATETIME":
		return time.UnixMicro(n).UTC().Format("2006-01-02T15:04:05.999999")
	}
	return strconv.FormatInt(n, 10)
}
