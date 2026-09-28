package main

// A Datastore entity's page reads and writes it through the v1 API (#893).
//
// The official Go client's Key has no project and no database: it reads a
// key value's partition as its namespace alone, and writes every key value in
// the client's own project and database. Add, Edit and Delete property read
// an entity through it and wrote the whole entity back, so a key value
// naming an entity in another project or another database was silently
// rewritten to name this project's default database — reproduced against the
// emulator (TestConsoleDatastoreKeepsAKeyValuesProjectAndDatabase). The entity
// and property pages therefore read the entity with Lookup, and those three
// write it back with Commit in the same transaction, changing only the
// property the form names: every other value goes back exactly as Lookup
// returned it. The client is still what creates and deletes entities, which
// rewrite no stored value.
//
// A kind's entity listing and the query builder's results read the entities
// with RunQuery on the same API (#904), so a key value is rendered the same
// way on every page: the listing's Properties cell showed a key to Order 7 in
// another project as Order/id=7, the key to this project's Order 7. An
// embedded entity's own key keeps its partition as well
// (datastoreForeignEntity).
//
// A key value in another project or database is shown with its partition
// (datastoreForeignKey) and offered no edit: the form writes a key in this
// project's default database, so saving it unchanged would change which
// entity it names.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// datastoreForeignKey is a key value naming an entity in another project or
// database than the one the page is in. Project and Database are set only
// where they differ from it.
type datastoreForeignKey struct {
	Key               *datastore.Key
	Project, Database string
}

// partition names where the key is: "project p", "database d" or both.
func (f datastoreForeignKey) partition() string {
	var in []string
	if f.Project != "" {
		in = append(in, "project "+f.Project)
	}
	if f.Database != "" {
		in = append(in, "database "+f.Database)
	}
	return strings.Join(in, ", ")
}

// String is the key as a page shows it: the key path, and the partition it
// is in, such as Order/id=7 (project other, database db2).
func (f datastoreForeignKey) String() string {
	return formatKeyPath(f.Key) + " (" + f.partition() + ")"
}

// datastoreForeignEntity is an embedded entity whose own key is in another
// project or database: the Go client's Entity.Key would drop where it is.
type datastoreForeignEntity struct {
	Key        datastoreForeignKey
	Properties datastore.PropertyList
}

// rawDatastore is the emulator's Datastore v1 API. done closes it.
func (p datastoreProvider) rawDatastore() (c datastorepb.DatastoreClient, done func(), err error) {
	conn, err := grpc.NewClient(p.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach Datastore: %w", err)
	}
	return datastorepb.NewDatastoreClient(conn), func() { _ = conn.Close() }, nil
}

// datastoreKeyPB is a key as the Go client writes it: its path and namespace,
// with no project or database, which the request's own fill in.
func datastoreKeyPB(k *datastore.Key) *datastorepb.Key {
	if k == nil {
		return nil
	}
	var path []*datastorepb.Key_PathElement
	for e := k; e != nil; e = e.Parent {
		el := &datastorepb.Key_PathElement{Kind: e.Kind}
		switch {
		case e.ID != 0:
			el.IdType = &datastorepb.Key_PathElement_Id{Id: e.ID}
		case e.Name != "":
			el.IdType = &datastorepb.Key_PathElement_Name{Name: e.Name}
		}
		path = append([]*datastorepb.Key_PathElement{el}, path...)
	}
	pk := &datastorepb.Key{Path: path}
	if k.Namespace != "" {
		pk.PartitionId = &datastorepb.PartitionId{NamespaceId: k.Namespace}
	}
	return pk
}

// datastoreKeyValue reads a key value as the page in project shows it: a
// *datastore.Key when it is in this project's default database, and a
// datastoreForeignKey otherwise.
func datastoreKeyValue(pk *datastorepb.Key, project string) any {
	ns := pk.GetPartitionId().GetNamespaceId()
	var k *datastore.Key
	for _, el := range pk.GetPath() {
		k = &datastore.Key{Kind: el.GetKind(), ID: el.GetId(), Name: el.GetName(), Parent: k, Namespace: ns}
	}
	if k == nil {
		return nil // a key with no path, which only a nested entity can hold
	}
	f := datastoreForeignKey{Key: k, Database: pk.GetPartitionId().GetDatabaseId()}
	if p := pk.GetPartitionId().GetProjectId(); p != "" && p != project {
		f.Project = p
	}
	if f.Project == "" && f.Database == "" {
		return k
	}
	return f
}

// datastoreValueGo is a stored value as the Go client reads it
// (propToValue), except that a key in another project or database is a
// datastoreForeignKey.
func datastoreValueGo(v *datastorepb.Value, project string) any {
	switch t := v.GetValueType().(type) {
	case *datastorepb.Value_BooleanValue:
		return t.BooleanValue
	case *datastorepb.Value_IntegerValue:
		return t.IntegerValue
	case *datastorepb.Value_DoubleValue:
		return t.DoubleValue
	case *datastorepb.Value_TimestampValue:
		return time.Unix(t.TimestampValue.GetSeconds(), int64(t.TimestampValue.GetNanos())).In(time.UTC)
	case *datastorepb.Value_KeyValue:
		return datastoreKeyValue(t.KeyValue, project)
	case *datastorepb.Value_StringValue:
		return t.StringValue
	case *datastorepb.Value_BlobValue:
		return t.BlobValue
	case *datastorepb.Value_GeoPointValue:
		return datastore.GeoPoint{Lat: t.GeoPointValue.GetLatitude(), Lng: t.GeoPointValue.GetLongitude()}
	case *datastorepb.Value_EntityValue:
		props := datastorePropertiesGo(t.EntityValue.GetProperties(), project)
		e := &datastore.Entity{Properties: props}
		if pk := t.EntityValue.GetKey(); pk != nil {
			switch k := datastoreKeyValue(pk, project).(type) {
			case *datastore.Key:
				e.Key = k
			case datastoreForeignKey:
				return datastoreForeignEntity{Key: k, Properties: props}
			}
		}
		return e
	case *datastorepb.Value_ArrayValue:
		out := make([]any, 0, len(t.ArrayValue.GetValues()))
		for _, e := range t.ArrayValue.GetValues() {
			out = append(out, datastoreValueGo(e, project))
		}
		return out
	}
	return nil
}

// datastorePropertiesGo is an entity's properties as the Go client reads
// them (protoToEntity), sorted by name: an array's index flag is its first
// element's.
func datastorePropertiesGo(props map[string]*datastorepb.Value, project string) datastore.PropertyList {
	out := make(datastore.PropertyList, 0, len(props))
	for name, v := range props {
		noIndex := v.GetExcludeFromIndexes()
		if values := v.GetArrayValue().GetValues(); len(values) > 0 {
			noIndex = values[0].GetExcludeFromIndexes()
		}
		out = append(out, datastore.Property{Name: name, Value: datastoreValueGo(v, project), NoIndex: noIndex})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Datastore's time range, as the Go client checks it before a write.
var (
	datastoreMinTime = time.Unix(int64(math.MinInt64)/1e6, (int64(math.MinInt64)%1e6)*1e3)
	datastoreMaxTime = time.Unix(int64(math.MaxInt64)/1e6, (int64(math.MaxInt64)%1e6)*1e3)
)

// datastoreValuePB is a value the form parsed (parseDatastoreValue) as the
// Go client writes it (interfaceToProto), with its checks and messages.
func datastoreValuePB(v any, noIndex bool) (*datastorepb.Value, error) {
	out := &datastorepb.Value{ExcludeFromIndexes: noIndex}
	switch t := v.(type) {
	case nil:
		out.ValueType = &datastorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}
	case bool:
		out.ValueType = &datastorepb.Value_BooleanValue{BooleanValue: t}
	case int64:
		out.ValueType = &datastorepb.Value_IntegerValue{IntegerValue: t}
	case float64:
		out.ValueType = &datastorepb.Value_DoubleValue{DoubleValue: t}
	case string:
		if len(t) > 1500 && !noIndex {
			return nil, errors.New("string property too long to index")
		}
		if !utf8.ValidString(t) {
			return nil, fmt.Errorf("string is not valid utf8: %q", t)
		}
		out.ValueType = &datastorepb.Value_StringValue{StringValue: t}
	case time.Time:
		if t.Before(datastoreMinTime) || t.After(datastoreMaxTime) {
			return nil, errors.New("time value out of range")
		}
		out.ValueType = &datastorepb.Value_TimestampValue{TimestampValue: timestamppb.New(t)}
	case *datastore.Key:
		if t == nil {
			out.ValueType = &datastorepb.Value_NullValue{}
		} else {
			out.ValueType = &datastorepb.Value_KeyValue{KeyValue: datastoreKeyPB(t)}
		}
	case datastore.GeoPoint:
		if !t.Valid() {
			return nil, errors.New("invalid GeoPoint value")
		}
		out.ValueType = &datastorepb.Value_GeoPointValue{GeoPointValue: &latlng.LatLng{Latitude: t.Lat, Longitude: t.Lng}}
	case *datastore.Entity:
		e := &datastorepb.Entity{Key: datastoreKeyPB(t.Key), Properties: map[string]*datastorepb.Value{}}
		for _, p := range t.Properties {
			c, err := datastoreValuePB(p.Value, p.NoIndex || noIndex)
			if err != nil {
				return nil, fmt.Errorf("%v for a Property with Name %q", err, p.Name)
			}
			if _, dup := e.Properties[p.Name]; dup {
				return nil, fmt.Errorf("duplicate Property with Name %q", p.Name)
			}
			e.Properties[p.Name] = c
		}
		out.ValueType = &datastorepb.Value_EntityValue{EntityValue: e}
	case []any:
		arr := make([]*datastorepb.Value, 0, len(t))
		for i, e := range t {
			c, err := datastoreValuePB(e, noIndex)
			if err != nil {
				return nil, fmt.Errorf("%v at index %d", err, i)
			}
			arr = append(arr, c)
		}
		out.ValueType = &datastorepb.Value_ArrayValue{ArrayValue: &datastorepb.ArrayValue{Values: arr}}
		// As the client writes it: an array's elements carry the index
		// flag, not the array.
		out.ExcludeFromIndexes = false
	default:
		return nil, fmt.Errorf("invalid Value type %T", v)
	}
	return out, nil
}

// datastorePropertyPB is a form's property as a stored value.
func datastorePropertyPB(p datastore.Property) (*datastorepb.Value, error) {
	v, err := datastoreValuePB(p.Value, p.NoIndex)
	if err != nil {
		return nil, fmt.Errorf("datastore: %v for a Property with Name %q", err, p.Name)
	}
	return v, nil
}

// lookupEntity reads one entity, in transaction tx when it is not nil; an
// entity that is not there is the client's ErrNoSuchEntity.
func lookupEntity(ctx context.Context, c datastorepb.DatastoreClient, project string, key *datastore.Key, tx []byte) (*datastorepb.Entity, error) {
	req := &datastorepb.LookupRequest{ProjectId: project, Keys: []*datastorepb.Key{datastoreKeyPB(key)}}
	if tx != nil {
		req.ReadOptions = &datastorepb.ReadOptions{ConsistencyType: &datastorepb.ReadOptions_Transaction{Transaction: tx}}
	}
	resp, err := c.Lookup(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.GetFound()) == 0 {
		if len(resp.GetDeferred()) > 0 {
			return nil, errors.New("datastore deferred the lookup; try again")
		}
		return nil, datastore.ErrNoSuchEntity
	}
	return resp.GetFound()[0].GetEntity(), nil
}

// readEntity is an entity's properties as its pages show them.
func (p datastoreProvider) readEntity(ctx context.Context, project string, key *datastore.Key) (datastore.PropertyList, error) {
	c, done, err := p.rawDatastore()
	if err != nil {
		return nil, err
	}
	defer done()
	e, err := lookupEntity(ctx, c, project, key, nil)
	if err != nil {
		return nil, err
	}
	return datastorePropertiesGo(e.GetProperties(), project), nil
}

// changeEntity reads an entity, changes its properties and writes it back
// with an update, in one transaction: an entity changed meanwhile aborts the
// transaction rather than being overwritten, and one deleted meanwhile is the
// client's "no such entity" rather than recreated. change sees the stored
// values as Lookup returned them; whatever it leaves alone is written back
// unchanged, key values' projects and databases included (#893).
func (p datastoreProvider) changeEntity(ctx context.Context, project string, scope datastoreScope, kind, id string, change func(map[string]*datastorepb.Value) error) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	key, err := datastoreEntityKey(project, scope.ns, kind, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, done, err := p.rawDatastore()
	if err != nil {
		return err
	}
	defer done()
	begin, err := c.BeginTransaction(ctx, &datastorepb.BeginTransactionRequest{ProjectId: project})
	if err != nil {
		return err
	}
	tx := begin.GetTransaction()
	finished := false
	defer func() {
		if !finished {
			_, _ = c.Rollback(context.WithoutCancel(ctx), &datastorepb.RollbackRequest{ProjectId: project, Transaction: tx})
		}
	}()
	e, err := lookupEntity(ctx, c, project, key, tx)
	if err != nil {
		return err
	}
	if e.Properties == nil {
		e.Properties = map[string]*datastorepb.Value{}
	}
	if err := change(e.Properties); err != nil {
		return err
	}
	finished = true
	_, err = c.Commit(ctx, &datastorepb.CommitRequest{
		ProjectId:           project,
		Mode:                datastorepb.CommitRequest_TRANSACTIONAL,
		TransactionSelector: &datastorepb.CommitRequest_Transaction{Transaction: tx},
		Mutations:           []*datastorepb.Mutation{{Operation: &datastorepb.Mutation_Update{Update: e}}},
	})
	if status.Code(err) == codes.Aborted {
		// As the client's RunInTransaction reports a transaction it
		// does not retry.
		return datastore.ErrConcurrentTransaction
	}
	return err
}

// renderDatastoreNested is a value inside an array or an embedded entity
// that JSON cannot hold as its own type, written as its type and the form a
// property of that type is shown in (#894): key(Order/name=id=7), as a key
// property shows Order/name=id=7, and timestamp(…), geopoint(lat, lng) and
// blob(N bytes). An embedded entity's own key is its __key__, a name no
// property can have. Go's %v showed a key as /Order,id=7, which does not
// tell a name from an ID.
func renderDatastoreNested(b *strings.Builder, v any) {
	switch t := v.(type) {
	case *datastore.Key:
		if t == nil {
			b.WriteString("null")
			return
		}
		b.WriteString("key(" + formatKeyPath(t) + ")")
	case datastoreForeignKey:
		b.WriteString("key(" + t.String() + ")")
	case time.Time:
		b.WriteString("timestamp(" + formatTimestamp(t) + ")")
	case datastore.GeoPoint:
		b.WriteString("geopoint(" + formatLatLng(t.Lat, t.Lng) + ")")
	case []byte:
		fmt.Fprintf(b, "blob(%d bytes)", len(t))
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
			return
		}
		b.WriteString(formatDouble(t))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			renderDatastoreNested(b, e)
		}
		b.WriteByte(']')
	case *datastore.Entity:
		if t == nil {
			b.WriteString("null")
			return
		}
		var key any
		if t.Key != nil {
			key = t.Key
		}
		renderDatastoreEntity(b, key, t.Properties)
	case datastoreForeignEntity:
		renderDatastoreEntity(b, t.Key, t.Properties)
	default:
		if !encodeJSON(b, v, nil) {
			fmt.Fprintf(b, "%v", v)
		}
	}
}

// renderDatastoreEntity is an embedded entity as renderDatastoreNested writes
// it: its key, when it has one, as __key__, then its properties by name.
func renderDatastoreEntity(b *strings.Builder, key any, properties datastore.PropertyList) {
	props := append(datastore.PropertyList{}, properties...)
	sort.SliceStable(props, func(a, c int) bool { return props[a].Name < props[c].Name })
	b.WriteByte('{')
	if key != nil {
		b.WriteString(`"__key__": `)
		renderDatastoreNested(b, key)
	}
	for i, p := range props {
		if i > 0 || key != nil {
			b.WriteString(", ")
		}
		name, _ := json.Marshal(p.Name)
		b.Write(name)
		b.WriteString(": ")
		renderDatastoreNested(b, p.Value)
	}
	b.WriteByte('}')
}

// --- Queries (#904) -----------------------------------------------------------

// encodeDatastoreCursor writes a query cursor as the Go client's
// Cursor.String does, so a cursor this screen issued before #904 still reads.
func encodeDatastoreCursor(c []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(c), "=")
}

// decodeDatastoreCursor reads a cursor as the client's DecodeCursor does.
func decodeDatastoreCursor(s string) ([]byte, error) {
	if n := len(s) % 4; n != 0 {
		s += strings.Repeat("=", 4-n)
	}
	return base64.URLEncoding.DecodeString(s)
}

// datastoreFieldName reads a property name as the client's query does: a
// name written as a quoted Go string literal is unquoted.
func datastoreFieldName(s string) (string, error) {
	if s == "" || (s[0] != '`' && s[0] != '"') {
		return s, nil
	}
	name, err := strconv.Unquote(s)
	if err != nil {
		return "", fmt.Errorf("datastore: invalid syntax for quoted field name %q", s)
	}
	return name, nil
}

// datastoreOperators are the query builder's operators as the v1 API names
// them.
var datastoreOperators = map[string]datastorepb.PropertyFilter_Operator{
	"=": datastorepb.PropertyFilter_EQUAL, "<": datastorepb.PropertyFilter_LESS_THAN,
	"<=": datastorepb.PropertyFilter_LESS_THAN_OR_EQUAL, ">": datastorepb.PropertyFilter_GREATER_THAN,
	">=": datastorepb.PropertyFilter_GREATER_THAN_OR_EQUAL,
}

// datastoreFilterPB is one property filter, as the client's FilterField
// writes it.
func datastoreFilterPB(field, op string, value any) (*datastorepb.Filter, error) {
	name, err := datastoreFieldName(field)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("datastore: empty query filter field name")
	}
	o, ok := datastoreOperators[strings.TrimSpace(op)]
	if !ok {
		return nil, fmt.Errorf("datastore: invalid operator %q in filter", op)
	}
	v, err := datastoreValuePB(value, false)
	if err != nil {
		return nil, fmt.Errorf("datastore: bad query filter value type: %w", err)
	}
	return &datastorepb.Filter{FilterType: &datastorepb.Filter_PropertyFilter{PropertyFilter: &datastorepb.PropertyFilter{
		Property: &datastorepb.PropertyReference{Name: name}, Op: o, Value: v}}}, nil
}

// datastoreOrderPB is one order, as the client's Order reads it: a leading
// minus sign is descending.
func datastoreOrderPB(field string) (*datastorepb.PropertyOrder, error) {
	field, dir := strings.TrimSpace(field), datastorepb.PropertyOrder_ASCENDING
	if rest, ok := strings.CutPrefix(field, "-"); ok {
		field, dir = strings.TrimSpace(rest), datastorepb.PropertyOrder_DESCENDING
	} else if strings.HasPrefix(field, "+") {
		return nil, fmt.Errorf("datastore: invalid order: %q", field)
	}
	name, err := datastoreFieldName(field)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("datastore: empty order")
	}
	return &datastorepb.PropertyOrder{Property: &datastorepb.PropertyReference{Name: name}, Direction: dir}, nil
}

// datastoreQueryResult is what runEntityQuery read: the entities, and the
// cursor just after the last of them.
type datastoreQueryResult struct {
	Entities []*datastorepb.Entity
	Cursor   []byte
}

// runEntityQuery runs a query through the v1 API's RunQuery in namespace ns,
// reading at most limit entities, from start when it is not empty. A batch
// can end before the limit with more to come (NOT_FINISHED), so it reads on
// from the batch's end cursor, as the client's iterator does.
func runEntityQuery(ctx context.Context, c datastorepb.DatastoreClient, project, ns string, q *datastorepb.Query, limit int, start []byte) (datastoreQueryResult, error) {
	var out datastoreQueryResult
	cursor := start
	for len(out.Entities) < limit {
		q.StartCursor = cursor
		q.Limit = wrapperspb.Int32(int32(limit - len(out.Entities)))
		resp, err := c.RunQuery(ctx, &datastorepb.RunQueryRequest{
			ProjectId:   project,
			PartitionId: &datastorepb.PartitionId{ProjectId: project, NamespaceId: ns},
			QueryType:   &datastorepb.RunQueryRequest_Query{Query: q},
		})
		if err != nil {
			return out, err
		}
		batch := resp.GetBatch()
		for _, r := range batch.GetEntityResults() {
			out.Entities = append(out.Entities, r.GetEntity())
			out.Cursor = r.GetCursor()
		}
		if len(batch.GetEntityResults()) == 0 || len(out.Cursor) == 0 {
			out.Cursor = batch.GetEndCursor()
		}
		if batch.GetMoreResults() != datastorepb.QueryResultBatch_NOT_FINISHED ||
			len(batch.GetEndCursor()) == 0 || string(batch.GetEndCursor()) == string(cursor) {
			break
		}
		cursor = batch.GetEndCursor()
	}
	return out, nil
}

// datastoreEntityProperties is a listed entity's key and properties as its
// row shows them, read as its own page reads them (datastorePropertiesGo).
func datastoreEntityProperties(e *datastorepb.Entity, project string) (*datastore.Key, datastore.PropertyList) {
	var key *datastore.Key
	switch k := datastoreKeyValue(e.GetKey(), project).(type) {
	case *datastore.Key:
		key = k
	case datastoreForeignKey:
		key = k.Key
	}
	return key, datastorePropertiesGo(e.GetProperties(), project)
}

// datastorePropertiesCell is a listed entity's Properties cell: each
// property by name, its value rendered as its page renders it.
func datastorePropertiesCell(props datastore.PropertyList) []string {
	var parts []string
	for _, prop := range props {
		parts = append(parts, prop.Name+": "+summarise(renderDatastoreValue(prop.Value, prop.NoIndex)))
	}
	return parts
}
