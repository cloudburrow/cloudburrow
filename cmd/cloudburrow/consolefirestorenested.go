package main

// Firestore values inside a map or an array, and bytes (#995): what Datastore
// has had since #905, #911 and #912.
//
// Edit field holds a map or array as JSON, which has no timestamp, geopoint,
// reference or bytes, so a map holding one was shown and offered no edit
// (#796): it could only be deleted and written again through the API.
// Google's Firestore console edits the values inside one in place. A field
// whose value is a map or an array now lists every value inside it, at any
// depth, on an Elements tab, each with its own page: Edit value replaces one,
// typed as Edit field is and prefilled, Add value adds one to a map or array
// (at an index of an array, or appended; under a new key of a map), and
// Remove value, confirmed as a delete is, removes one.
//
// A value is addressed by its steps from the field, as Datastore's are
// (datastoreElementSegment): a JSON array of indexes and keys, [2] the third
// value of an array and ["line", "when"] the when key of the map line.
//
// Every change is read and written through the v1 API the official client
// calls, BatchGetDocuments and Commit, on the stored protocol buffers, so the
// values it leaves alone go back exactly as they were read: a Go round trip
// through the client could only be trusted as far as its conversions are.
// Firestore addresses a write by field path, and a field path can name a key
// of a map but not an index of an array, so the write's update mask is the
// changed value's path as far as it runs through maps — the value itself
// inside maps, the array holding it otherwise — and nothing else in the
// document is written. The write carries the document's update time as its
// precondition, so a document changed between the read and the write is the
// emulator's FAILED_PRECONDITION, and each action carries a digest of the
// field as its page drew it, so a field changed since the page was loaded is
// refused before anything is written (datastoreExpected's pattern, #923).
//
// Bytes are shown and edited as base64, standard encoding with padding, as
// the v1 REST API's bytesValue is written and as Datastore's blobs are
// (#912). A reference to a document in another project or database is listed
// and offered no edit: the form writes a reference into this project's
// default database, so saving it would change which document it names.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Edit value form's types: one value, not a map or an array, whose
// values are each edited on their own.
const (
	firestoreElementTypeList    = "string, number, boolean, null, timestamp, geopoint, reference or bytes"
	firestoreElementTypePattern = `^(string|number|boolean|null|timestamp|geopoint|reference|bytes)$`
)

var firestoreElementType = regexp.MustCompile(firestoreElementTypePattern)

// firestoreElementScan bounds the values an Elements tab lists.
const firestoreElementScan = 1000

// firestoreElement is one value inside a map or array field.
type firestoreElement struct {
	Steps []any // int for an array's index, string for a map's key
	Value *firestorepb.Value
}

// rawFirestore is the v1 API's client, with the emulator's owner token, as
// the official client's is.
func (p firestoreProvider) rawFirestore() (c firestorepb.FirestoreClient, done func(), err error) {
	conn, err := grpc.NewClient(p.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(emulatorOwner{}))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach Firestore: %w", err)
	}
	return firestorepb.NewFirestoreClient(conn), func() { _ = conn.Close() }, nil
}

func firestoreDatabaseName(project string) string {
	return "projects/" + project + "/databases/(default)"
}

// firestoreDocumentName is a document's full name, after checking that
// collection is a collection path and id a document ID.
func firestoreDocumentName(project, collection, id string) (string, error) {
	parts := strings.Split(collection, "/")
	if len(parts)%2 == 0 || slicesContainsEmpty(parts) {
		return "", fmt.Errorf("%q is not a collection path: a collection is an ID, or document path/ID, "+
			"with an odd number of segments", collection)
	}
	if id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("%q is not a document ID: an ID has no slash", id)
	}
	return firestoreDocumentsPrefix(project) + collection + "/" + id, nil
}

func slicesContainsEmpty(parts []string) bool {
	for _, s := range parts {
		if s == "" {
			return true
		}
	}
	return false
}

// readRawDocument reads one document with BatchGetDocuments, as the official
// client's Get does, and returns it as stored.
func readRawDocument(ctx context.Context, c firestorepb.FirestoreClient, project, name string) (*firestorepb.Document, error) {
	stream, err := c.BatchGetDocuments(ctx, &firestorepb.BatchGetDocumentsRequest{
		Database: firestoreDatabaseName(project), Documents: []string{name}})
	if err != nil {
		return nil, err
	}
	var doc *firestorepb.Document
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if f := resp.GetFound(); f != nil {
			doc = f
		}
	}
	if doc == nil {
		return nil, status.Errorf(codes.NotFound, "document %s does not exist", strings.TrimPrefix(name, firestoreDocumentsPrefix(project)))
	}
	return doc, nil
}

// lookupFirestoreField reads a document and one of its fields as stored.
func (p firestoreProvider) lookupFirestoreField(ctx context.Context, project, collection, id, field string) (*firestorepb.Document, *firestorepb.Value, error) {
	name, err := firestoreDocumentName(project, collection, id)
	if err != nil {
		return nil, nil, err
	}
	c, done, err := p.rawFirestore()
	if err != nil {
		return nil, nil, err
	}
	defer done()
	doc, err := readRawDocument(ctx, c, project, name)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the document: %w", err)
	}
	prop, has := doc.GetFields()[field]
	if !has {
		return nil, nil, fmt.Errorf("document %s has no field %q", id, field)
	}
	return doc, prop, nil
}

// isFirestoreContainer is whether a value is a map or an array, which values
// are added to.
func isFirestoreContainer(v *firestorepb.Value) bool {
	switch v.GetValueType().(type) {
	case *firestorepb.Value_ArrayValue, *firestorepb.Value_MapValue:
		return true
	}
	return false
}

// firestoreElements lists the values inside a map or array, at any depth,
// depth first, a map's keys in order. A map or array inside is listed itself,
// before the values inside it, so values can be added to it and it can be
// removed.
func firestoreElements(v *firestorepb.Value) []firestoreElement {
	var out []firestoreElement
	var walk func(v *firestorepb.Value, steps []any)
	walk = func(v *firestorepb.Value, steps []any) {
		at := func(s any) []any { return append(append([]any{}, steps...), s) }
		if len(steps) > 0 {
			out = append(out, firestoreElement{Steps: steps, Value: v})
		}
		switch t := v.GetValueType().(type) {
		case *firestorepb.Value_ArrayValue:
			for i, e := range t.ArrayValue.GetValues() {
				walk(e, at(i))
			}
		case *firestorepb.Value_MapValue:
			fields := t.MapValue.GetFields()
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(fields[k], at(k))
			}
		}
	}
	walk(v, nil)
	return out
}

// firestoreValueAt is the value steps address inside v, which is v itself
// for no steps.
func firestoreValueAt(v *firestorepb.Value, steps []any, label string) (*firestorepb.Value, error) {
	for _, step := range steps {
		switch s := step.(type) {
		case int:
			arr := v.GetArrayValue()
			if arr == nil || s >= len(arr.GetValues()) {
				return nil, fmt.Errorf("the field holds no value at %s", label)
			}
			v = arr.GetValues()[s]
		case string:
			child, has := v.GetMapValue().GetFields()[s]
			if !has {
				return nil, fmt.Errorf("the field holds no value at %s", label)
			}
			v = child
		default:
			return nil, fmt.Errorf("the field holds no value at %s", label)
		}
	}
	return v, nil
}

// firestoreValueGo is a stored value as the official client reads it, for
// its type and its rendering: a reference as a DocumentRef naming it.
func firestoreValueGo(v *firestorepb.Value) any {
	switch t := v.GetValueType().(type) {
	case *firestorepb.Value_BooleanValue:
		return t.BooleanValue
	case *firestorepb.Value_IntegerValue:
		return t.IntegerValue
	case *firestorepb.Value_DoubleValue:
		return t.DoubleValue
	case *firestorepb.Value_TimestampValue:
		return t.TimestampValue.AsTime()
	case *firestorepb.Value_StringValue:
		return t.StringValue
	case *firestorepb.Value_BytesValue:
		if t.BytesValue == nil {
			return []byte{}
		}
		return t.BytesValue
	case *firestorepb.Value_ReferenceValue:
		return &firestore.DocumentRef{Path: t.ReferenceValue, ID: t.ReferenceValue[strings.LastIndex(t.ReferenceValue, "/")+1:]}
	case *firestorepb.Value_GeoPointValue:
		return t.GeoPointValue
	case *firestorepb.Value_ArrayValue:
		out := make([]any, len(t.ArrayValue.GetValues()))
		for i, e := range t.ArrayValue.GetValues() {
			out[i] = firestoreValueGo(e)
		}
		return out
	case *firestorepb.Value_MapValue:
		out := make(map[string]any, len(t.MapValue.GetFields()))
		for k, e := range t.MapValue.GetFields() {
			out[k] = firestoreValueGo(e)
		}
		return out
	}
	return nil
}

// firestoreValuePB is a value parseFirestoreValue read, as the official
// client writes it.
func firestoreValuePB(v any) (*firestorepb.Value, error) {
	switch t := v.(type) {
	case nil:
		return &firestorepb.Value{ValueType: &firestorepb.Value_NullValue{NullValue: structpb.NullValue_NULL_VALUE}}, nil
	case bool:
		return &firestorepb.Value{ValueType: &firestorepb.Value_BooleanValue{BooleanValue: t}}, nil
	case int64:
		return &firestorepb.Value{ValueType: &firestorepb.Value_IntegerValue{IntegerValue: t}}, nil
	case float64:
		return &firestorepb.Value{ValueType: &firestorepb.Value_DoubleValue{DoubleValue: t}}, nil
	case string:
		return &firestorepb.Value{ValueType: &firestorepb.Value_StringValue{StringValue: t}}, nil
	case []byte:
		return &firestorepb.Value{ValueType: &firestorepb.Value_BytesValue{BytesValue: t}}, nil
	case time.Time:
		return &firestorepb.Value{ValueType: &firestorepb.Value_TimestampValue{TimestampValue: timestamppb.New(t)}}, nil
	case *latlng.LatLng:
		return &firestorepb.Value{ValueType: &firestorepb.Value_GeoPointValue{GeoPointValue: t}}, nil
	case *firestore.DocumentRef:
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{ReferenceValue: t.Path}}, nil
	case []any:
		arr := &firestorepb.ArrayValue{}
		for _, e := range t {
			if _, nested := e.([]any); nested {
				return nil, errors.New("an array cannot hold an array; Firestore refuses one")
			}
			ev, err := firestoreValuePB(e)
			if err != nil {
				return nil, err
			}
			arr.Values = append(arr.Values, ev)
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ArrayValue{ArrayValue: arr}}, nil
	case map[string]any:
		m := &firestorepb.MapValue{Fields: make(map[string]*firestorepb.Value, len(t))}
		for k, e := range t {
			ev, err := firestoreValuePB(e)
			if err != nil {
				return nil, err
			}
			m.Fields[k] = ev
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: m}}, nil
	}
	return nil, fmt.Errorf("a %T is not a value this form writes", v)
}

// parseFirestorePB reads a form's type and value as the stored value the
// official client would write for it. A reference is always one into this
// project's default database.
func parseFirestorePB(project, typ, raw string) (*firestorepb.Value, error) {
	if strings.TrimSpace(typ) == "reference" {
		rel, err := firestoreReferencePath(project, raw)
		if err != nil {
			return nil, err
		}
		return &firestorepb.Value{ValueType: &firestorepb.Value_ReferenceValue{
			ReferenceValue: firestoreDocumentsPrefix(project) + rel}}, nil
	}
	// Every other type is read without a client: only a reference needs one.
	v, err := parseFirestoreValue(nil, project, typ, raw)
	if err != nil {
		return nil, err
	}
	return firestoreValuePB(v)
}

// firestoreLocalReference is the document path a reference names inside
// this project's default database, and false for one in another project or
// database.
func firestoreLocalReference(project, name string) (string, bool) {
	rel, ok := strings.CutPrefix(name, firestoreDocumentsPrefix(project))
	return rel, ok && rel != ""
}

// firestoreForeignReferenceNote says why a reference is offered no edit.
func firestoreForeignReferenceNote(name string) string {
	return "This reference names " + name + ", a document in another project or database than this one's " +
		"default. The form writes a reference into this project's default database, so saving it would change " +
		"which document it names; it cannot be edited here. It can be removed."
}

// formatFirestorePB is a stored value inside a field as its Edit value form
// holds it: the type the form names and the text parseFirestorePB reads
// back to the same value. ok is false, with why, for a value the form cannot
// hold without changing it.
func formatFirestorePB(project string, v *firestorepb.Value) (typ, raw, why string, ok bool) {
	switch t := v.GetValueType().(type) {
	case *firestorepb.Value_ArrayValue, *firestorepb.Value_MapValue:
		return "", "", "A map or array inside a field is changed value by value: each value inside it is listed " +
			"after it, with Edit value, and Add value and Remove value change what it holds.", false
	case *firestorepb.Value_ReferenceValue:
		rel, local := firestoreLocalReference(project, t.ReferenceValue)
		if !local {
			return "", "", firestoreForeignReferenceNote(t.ReferenceValue), false
		}
		return "reference", rel, "", true
	case *firestorepb.Value_NullValue:
		return "null", "", "", true
	}
	typ, raw, ok = formatFirestoreValue(firestoreValueGo(v))
	if !ok {
		return "", "", "This value cannot be edited here.", false
	}
	return typ, raw, "", true
}

// firestoreRowValue is a value as a listing's row shows it: in full, as its
// page does, except bytes' base64, which is shortened.
func firestoreRowValue(v any) string {
	s := renderFirestoreValue(v)
	if _, isBytes := v.([]byte); isBytes {
		return summarise(s)
	}
	return s
}

// firestoreElementSteps reads an element page's path segment.
func firestoreElementSteps(seg string) ([]any, error) {
	return parseDatastoreElement(seg)
}

// firestoreElementLabel names an element as its row and page show it: the
// field, then [i] for an array index and .key for a map's key, a key that is
// not a plain identifier written ["…"] — Datastore's rendering, and
// Firestore's field-path rule for a simple name is the same.
func firestoreElementLabel(field string, steps []any) string {
	return datastoreElementLabel(field, steps)
}

// firestoreExpectedField names the hidden field a value action carries its
// field's digest in.
const firestoreExpectedField = datastoreExpectedField

// withFirestoreExpected is an action carrying the digest of the field as the
// page drew it.
func withFirestoreExpected(a console.Action, prop *firestorepb.Value) console.Action {
	a.Fields = append(append([]console.Field{}, a.Fields...), console.Field{Name: firestoreExpectedField,
		Label: "Drawn from", Type: "hidden", Default: protoDigest(prop)})
	return a
}

// checkFirestoreExpected refuses a value action whose field is not the one
// its form or row was drawn from, or that does not say.
func checkFirestoreExpected(prop *firestorepb.Value, field string, values map[string]string) error {
	want := strings.TrimSpace(values[firestoreExpectedField])
	if want == "" {
		return errors.New("the action does not say which version of the field it was drawn from; reload the page and try again")
	}
	if protoDigest(prop) != want {
		return fmt.Errorf("field %q changed since the page was loaded, so the value this action addresses may "+
			"not be the one shown; reload the page and try again", field)
	}
	return nil
}

// firestoreAddValueAction is Add value on a map or array: a key or an index,
// then a value typed as Edit field's is.
func firestoreAddValueAction(target *firestorepb.Value) console.Action {
	var where console.Field
	if arr := target.GetArrayValue(); arr != nil {
		n := len(arr.GetValues())
		where = console.Field{Name: "index", Label: "Index", Type: "text", Pattern: `^\s*[0-9]*\s*$`,
			Help: fmt.Sprintf("Where the value goes: 0 puts it first and %d, or empty, after the last. "+
				"The values from that index on move up one.", n)}
	} else {
		where = console.Field{Name: "field", Label: "Key", Type: "text", Required: true,
			Help: "A key this map does not have yet."}
	}
	return console.Action{ID: "addvalue", Label: "Add value", Fields: []console.Field{where,
		{Name: "type", Label: "Type", Type: "text", Required: true, Default: "string", Pattern: firestoreTypePattern,
			Help: "One of " + firestoreTypeList + ". An array cannot hold an array."},
		{Name: "value", Label: "Value", Type: "textarea", Help: firestoreValueHelp},
	}}
}

// firestoreElementActions are what an element's row and page offer: Edit
// value, prefilled, when the form can hold the value; Add value on a map or
// array; Remove value on every value. Each carries the digest of drawn, the
// whole field as read. The action route checks the same things again before
// writing.
func firestoreElementActions(project string, el firestoreElement, drawn *firestorepb.Value) []console.Action {
	var out []console.Action
	if typ, raw, _, ok := formatFirestorePB(project, el.Value); ok {
		out = append(out, console.Action{ID: "editvalue", Label: "Edit value", Fields: []console.Field{
			{Name: "type", Label: "Type", Type: "text", Required: true, Default: typ, Pattern: firestoreElementTypePattern,
				Help: "One of " + firestoreElementTypeList + "."},
			{Name: "value", Label: "Value", Type: "textarea", Default: raw, Help: firestoreValueHelp},
		}})
	}
	if isFirestoreContainer(el.Value) {
		out = append(out, firestoreAddValueAction(el.Value))
	}
	out = append(out, console.Action{ID: "removevalue", Label: "Remove value", Destructive: true, Leaves: true})
	for i := range out {
		out[i] = withFirestoreExpected(out[i], drawn)
	}
	return out
}

// firestoreElementsSection is a map or array field's Elements tab: every
// value inside it, each opening its own page. base is where v is inside the
// field: none for the field itself, and an element's steps on its page.
func firestoreElementsSection(project, collection, id, field string, base []any, v, drawn *firestorepb.Value) (console.Section, bool) {
	if !isFirestoreContainer(v) {
		return console.Section{}, false
	}
	list := console.Listing{Columns: []string{"Type", "Value"}, NameColumn: "Element", Noun: "values", RowsOpenable: true}
	els := firestoreElements(v)
	for _, el := range els {
		if len(list.Items) >= firestoreElementScan {
			break
		}
		el.Steps = append(append([]any{}, base...), el.Steps...)
		gv := firestoreValueGo(el.Value)
		at := []string{collection, id, field, datastoreElementSegment(el.Steps)}
		list.Items = append(list.Items, console.Resource{
			Name:    firestoreElementLabel(field, el.Steps),
			Fields:  map[string]string{"Type": firestoreType(gv), "Value": firestoreRowValue(gv)},
			Opens:   at,
			Actions: firestoreElementActions(project, el, drawn),
			ActsOn:  at,
		})
	}
	list.Total = len(list.Items)
	switch {
	case len(els) > firestoreElementScan:
		list.Note = fmt.Sprintf("The first %d of %d values.", firestoreElementScan, len(els))
	case list.Total == 0:
		list.Note = "This " + map[bool]string{true: "array", false: "map"}[v.GetArrayValue() != nil] +
			" holds no value. Add value, on this page, adds one."
	default:
		list.Note = "Edit value writes back that one value; Add value, on a map or array and on this page, adds one " +
			"to it, and Remove value removes one. Nothing else in the document is written. A reference to a " +
			"document in another project or database is offered no edit."
	}
	return console.Section{ID: "elements", Label: "Elements", Listing: list}, true
}

// elementDetail is one value inside a map or array field.
func (p firestoreProvider) elementDetail(ctx context.Context, project, collection, id, field, seg string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	steps, err := firestoreElementSteps(seg)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	doc, prop, err := p.lookupFirestoreField(ctx, project, collection, id, field)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	label := firestoreElementLabel(field, steps)
	v, err := firestoreValueAt(prop, steps, label)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	gv := firestoreValueGo(v)
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Document", Value: strings.TrimPrefix(doc.GetName(), firestoreDocumentsPrefix(project))},
			{Label: "Field", Value: field},
			{Label: "Element", Value: label},
			{Label: "Type", Value: firestoreType(gv)},
		},
		Sections: []console.Section{{ID: "value", Label: "Value", Kind: console.KindText, Text: renderFirestoreValue(gv)}},
		Trail:    firestoreElementTrail(collection, id, field, label),
	}
	if _, _, why, ok := formatFirestorePB(project, v); !ok {
		d.Sections[0].Note = why
	} else {
		d.Sections[0].Note = "Edit value writes back this value only; nothing else in the document is written."
	}
	if sec, ok := firestoreElementsSection(project, collection, id, field, steps, v, prop); ok {
		d.Sections = append(d.Sections, sec)
	}
	return d, nil
}

// firestoreElementTrail is an element page's breadcrumb: every collection
// and document above it, the field, and the element by its label rather
// than its address.
func firestoreElementTrail(collection, id, field, label string) []console.Crumb {
	trail := firestoreTrail(collection, id, field)
	if trail == nil {
		trail = []console.Crumb{
			{Label: collection, Path: []string{collection}},
			{Label: id, Path: []string{collection, id}},
			{Label: field},
		}
	}
	trail[len(trail)-1].Path = []string{collection, id, field}
	return append(trail, console.Crumb{Label: label})
}

// fieldActions is a field page's actions: Add value on a map or array field,
// carrying the field's digest, and Delete field.
func (p firestoreProvider) fieldActions(ctx context.Context, project string, path []string) []console.Action {
	del := console.Action{ID: "deletefield", Label: "Delete field", Destructive: true, Leaves: true}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, prop, err := p.lookupFirestoreField(ctx, project, path[0], path[1], path[2])
	if err != nil || !isFirestoreContainer(prop) {
		return []console.Action{del}
	}
	return []console.Action{withFirestoreExpected(firestoreAddValueAction(prop), prop), del}
}

// elementActions is an element page's actions (firestoreElementActions).
func (p firestoreProvider) elementActions(ctx context.Context, project string, path []string) []console.Action {
	steps, err := firestoreElementSteps(path[3])
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, prop, err := p.lookupFirestoreField(ctx, project, path[0], path[1], path[2])
	if err != nil {
		return nil
	}
	v, err := firestoreValueAt(prop, steps, firestoreElementLabel(path[2], steps))
	if err != nil {
		return nil
	}
	return firestoreElementActions(project, firestoreElement{Steps: steps, Value: v}, prop)
}

// setFirestoreElement replaces the value steps address inside the field with
// the form's value, and returns the steps of what changed.
func setFirestoreElement(prop *firestorepb.Value, project, field string, steps []any, values map[string]string) ([]any, error) {
	label := firestoreElementLabel(field, steps)
	if len(steps) == 0 {
		return nil, errors.New("a field is changed with Edit field")
	}
	typ := strings.TrimSpace(values["type"])
	if typ == "map" || typ == "array" {
		return nil, errors.New("Edit value writes one value; a map or array is added with Add value, or written as a whole with Edit field")
	}
	if !firestoreElementType.MatchString(typ) {
		return nil, fmt.Errorf("type %q is not one of %s", typ, firestoreElementTypeList)
	}
	parent, err := firestoreValueAt(prop, steps[:len(steps)-1], label)
	if err != nil {
		return nil, err
	}
	old, err := firestoreValueAt(parent, steps[len(steps)-1:], label)
	if err != nil {
		return nil, err
	}
	// Checked again here: the form was drawn earlier.
	if _, _, why, ok := formatFirestorePB(project, old); !ok {
		return nil, fmt.Errorf("%s: %s", label, why)
	}
	nv, err := parseFirestorePB(project, typ, values["value"])
	if err != nil {
		return nil, fmt.Errorf("%s: %v", label, err)
	}
	switch s := steps[len(steps)-1].(type) {
	case int:
		parent.GetArrayValue().Values[s] = nv
	case string:
		parent.GetMapValue().Fields[s] = nv
	}
	return steps, nil
}

// addFirestoreElement adds a value to the map or array steps address inside
// the field, as Add value's form names it, and returns the steps of what
// changed: the new key of a map, or the array.
func addFirestoreElement(prop *firestorepb.Value, project, field string, steps []any, values map[string]string) ([]any, error) {
	label := firestoreElementLabel(field, steps)
	target, err := firestoreValueAt(prop, steps, label)
	if err != nil {
		return nil, err
	}
	if !isFirestoreContainer(target) {
		return nil, fmt.Errorf("%s is not a map or an array; a value is added to one", label)
	}
	typ := strings.TrimSpace(values["type"])
	if arr := target.GetArrayValue(); arr != nil {
		at := len(arr.GetValues())
		if raw := strings.TrimSpace(values["index"]); raw != "" {
			i, err := strconv.Atoi(raw)
			if err != nil || i < 0 || i > at {
				return nil, fmt.Errorf("index %q is not one of 0 to %d: %s holds %d values", raw, at, label, at)
			}
			at = i
		}
		if typ == "array" {
			// Firestore's rule, refused here so the form says it plainly.
			return nil, errors.New("an array cannot hold an array; Firestore refuses one")
		}
		nv, err := parseFirestorePB(project, typ, values["value"])
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %v", label, at, err)
		}
		arr.Values = append(append(append([]*firestorepb.Value{}, arr.GetValues()[:at]...), nv), arr.GetValues()[at:]...)
		return steps, nil
	}
	key := values["field"]
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("a key is required")
	}
	m := target.GetMapValue()
	if _, has := m.GetFields()[key]; has {
		return nil, fmt.Errorf("the map %s already has a key %q; change it with Edit value", label, key)
	}
	at := append(append([]any{}, steps...), key)
	nv, err := parseFirestorePB(project, typ, values["value"])
	if err != nil {
		return nil, fmt.Errorf("%s: %v", firestoreElementLabel(field, at), err)
	}
	if m.Fields == nil {
		m.Fields = map[string]*firestorepb.Value{}
	}
	m.Fields[key] = nv
	return at, nil
}

// removeFirestoreElement removes the value steps address inside the field:
// from its array, whose later values move down one, or from its map. It
// returns the steps of what changed: the key removed, or the array.
func removeFirestoreElement(prop *firestorepb.Value, field string, steps []any) ([]any, error) {
	label := firestoreElementLabel(field, steps)
	if len(steps) == 0 {
		return nil, errors.New("a field is removed with Delete field")
	}
	parent, err := firestoreValueAt(prop, steps[:len(steps)-1], label)
	if err != nil {
		return nil, err
	}
	switch s := steps[len(steps)-1].(type) {
	case int:
		arr := parent.GetArrayValue()
		if arr == nil || s >= len(arr.GetValues()) {
			return nil, fmt.Errorf("the field holds no value at %s", label)
		}
		arr.Values = append(arr.Values[:s], arr.Values[s+1:]...)
		return steps[:len(steps)-1], nil
	case string:
		m := parent.GetMapValue()
		if _, has := m.GetFields()[s]; !has {
			return nil, fmt.Errorf("the field holds no value at %s", label)
		}
		delete(m.Fields, s)
		return steps, nil
	}
	return nil, fmt.Errorf("the field holds no value at %s", label)
}

// firestoreSimpleField is a field-path segment written without quoting.
var firestoreSimpleField = regexp.MustCompile(`^[_a-zA-Z][_a-zA-Z0-9]*$`)

// firestoreFieldPath writes segments as a Firestore field path: a segment
// that is not a simple name in backticks, with a backtick or backslash in it
// escaped by a backslash.
func firestoreFieldPath(segments []string) string {
	out := make([]string, len(segments))
	for i, s := range segments {
		if firestoreSimpleField.MatchString(s) {
			out[i] = s
			continue
		}
		out[i] = "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
	}
	return strings.Join(out, ".")
}

// firestoreFieldWrite is the one write a value action commits: the changed
// field, masked to the path of what changed as far as it runs through maps
// (Firestore addresses a map's key, not an array's index), preconditioned on
// the document's update time as read. A path absent from the changed field,
// a removed key, is absent from the write, which deletes it.
func firestoreFieldWrite(name, field string, changed *firestorepb.Value, at []any, updateTime *timestamppb.Timestamp) *firestorepb.Write {
	mask := []string{field}
	for _, s := range at {
		k, isKey := s.(string)
		if !isKey {
			break
		}
		mask = append(mask, k)
	}
	fields := map[string]*firestorepb.Value{}
	v, present := changed, true
	for _, k := range mask[1:] {
		child, has := v.GetMapValue().GetFields()[k]
		if !has {
			present = false
			break
		}
		v = child
	}
	if present {
		for i := len(mask) - 1; i >= 1; i-- {
			v = &firestorepb.Value{ValueType: &firestorepb.Value_MapValue{MapValue: &firestorepb.MapValue{
				Fields: map[string]*firestorepb.Value{mask[i]: v}}}}
		}
		fields[field] = v
	}
	return &firestorepb.Write{
		Operation:       &firestorepb.Write_Update{Update: &firestorepb.Document{Name: name, Fields: fields}},
		UpdateMask:      &firestorepb.DocumentMask{FieldPaths: []string{firestoreFieldPath(mask)}},
		CurrentDocument: &firestorepb.Precondition{ConditionType: &firestorepb.Precondition_UpdateTime{UpdateTime: updateTime}},
	}
}

// changeFirestoreField is a value action's write: the document read as
// stored, the field checked against the digest its page drew, change applied
// to a copy of it, and the part that changed committed with the document's
// update time as the precondition.
func (p firestoreProvider) changeFirestoreField(ctx context.Context, project string, path []string, values map[string]string, change func(prop *firestorepb.Value) ([]any, error)) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	collection, id, field := path[0], path[1], path[2]
	name, err := firestoreDocumentName(project, collection, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, done, err := p.rawFirestore()
	if err != nil {
		return err
	}
	defer done()
	doc, err := readRawDocument(ctx, c, project, name)
	if err != nil {
		return fmt.Errorf("cannot read the document: %w", err)
	}
	prop, has := doc.GetFields()[field]
	if !has {
		return fmt.Errorf("document %s has no field %q", id, field)
	}
	if err := checkFirestoreExpected(prop, field, values); err != nil {
		return err
	}
	changed := proto.Clone(prop).(*firestorepb.Value)
	at, err := change(changed)
	if err != nil {
		return err
	}
	_, err = c.Commit(ctx, &firestorepb.CommitRequest{Database: firestoreDatabaseName(project),
		Writes: []*firestorepb.Write{firestoreFieldWrite(name, field, changed, at, doc.GetUpdateTime())}})
	return err
}

// actOnFirestoreValue is ActAt for Edit value, Add value and Remove value;
// handled is false for any other action.
func (p firestoreProvider) actOnFirestoreValue(ctx context.Context, project string, path []string, action string, values map[string]string) (handled bool, err error) {
	var steps []any
	if len(path) == 4 {
		if steps, err = firestoreElementSteps(path[3]); err != nil {
			return true, err
		}
	}
	switch {
	case action == "editvalue" && len(path) == 4:
		return true, p.changeFirestoreField(ctx, project, path, values, func(prop *firestorepb.Value) ([]any, error) {
			return setFirestoreElement(prop, project, path[2], steps, values)
		})
	case action == "addvalue" && (len(path) == 3 || len(path) == 4):
		return true, p.changeFirestoreField(ctx, project, path, values, func(prop *firestorepb.Value) ([]any, error) {
			return addFirestoreElement(prop, project, path[2], steps, values)
		})
	case action == "removevalue" && len(path) == 4:
		return true, p.changeFirestoreField(ctx, project, path, values, func(prop *firestorepb.Value) ([]any, error) {
			return removeFirestoreElement(prop, path[2], steps)
		})
	}
	return false, nil
}
