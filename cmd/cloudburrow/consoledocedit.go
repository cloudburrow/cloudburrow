package main

// Firestore documents and Datastore entities: create, edit and delete (#796).
//
// Firestore/Commit and Datastore/Commit are verified with the official
// clients, and both screens were read-only: a document or an entity could be
// opened and never added, changed or removed. Every write here goes through the
// same official client the screens read with, against the forwarded emulator
// port, so the console accepts what the emulator accepts and a refusal is the
// emulator's (or the client's) own message.
//
// A value is typed, as Google's consoles type it: the form names the type and
// the value is parsed as that type, never guessed from its text, because "42"
// and 42 are different writes. A value is prefilled on its edit form in the
// same form it is parsed from, so saving a field unchanged stores the same
// type and value it held (TestEditFormsRoundTripTheStoredType). A value that
// form cannot hold without loss — bytes, or a map holding a timestamp — is
// shown and offered no edit, rather than an edit that would change its type.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// --- typed values, shared ----------------------------------------------------

// wholeNumber is a number written without a decimal point or exponent, which
// is how an integer is told from a double on a form that has one "number".
var wholeNumber = regexp.MustCompile(`^[+-]?[0-9]+$`)

// formatDouble writes a double so it reads back as a double: always with a
// decimal point or an exponent, so 2.0 is not saved again as the integer 2.
func formatDouble(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64) // +Inf, -Inf, NaN
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if len(s) > 21 {
		s = strconv.FormatFloat(f, 'g', -1, 64)
	}
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// parseNumber reads a number the way formatDouble writes one.
func parseNumber(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if wholeNumber.MatchString(raw) {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is out of range for a 64-bit integer; write it with a decimal point to store a double", raw)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", raw)
	}
	return f, nil
}

func parseBool(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("a boolean is true or false, not %q", raw)
}

func parseTimestamp(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time such as 2026-09-27T15:04:05Z", raw)
	}
	return t, nil
}

func formatTimestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseLatLng reads "latitude, longitude". The ranges are the emulator's to
// enforce, and its message is the one shown.
func parseLatLng(raw string) (lat, lng float64, err error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 2 {
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lng, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 == nil && err2 == nil {
			return lat, lng, nil
		}
	}
	return 0, 0, fmt.Errorf("a geopoint is latitude, longitude, such as 51.5, -0.12; not %q", raw)
}

func formatLatLng(lat, lng float64) string {
	return strconv.FormatFloat(lat, 'f', -1, 64) + ", " + strconv.FormatFloat(lng, 'f', -1, 64)
}

// decodeJSON reads a map or array value. Numbers keep the integer/double
// distinction formatDouble writes; objects and arrays are returned as
// map[string]any and []any for the caller to convert.
func decodeJSON(raw, want string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("a %s is written as JSON: %v", want, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("a %s is one JSON value", want)
	}
	return v, nil
}

// jsonNumber converts a decoded JSON number: a whole number is an integer,
// anything with a point or an exponent a double.
func jsonNumber(n json.Number) (any, error) {
	return parseNumber(n.String())
}

// encodeJSON writes a nested value as JSON, reporting false for anything JSON
// cannot hold as its own type: a timestamp, geopoint, reference, key or bytes
// nested in a map would come back as a string, and a non-finite double not at
// all.
func encodeJSON(b *strings.Builder, v any, entity func(any) (map[string]any, bool)) bool {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return false
		}
		b.WriteString(formatDouble(t))
	case string:
		encoded, _ := json.Marshal(t)
		b.Write(encoded)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			if !encodeJSON(b, e, entity) {
				return false
			}
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		for i, k := range sortedAnyKeys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			key, _ := json.Marshal(k)
			b.Write(key)
			b.WriteString(": ")
			if !encodeJSON(b, t[k], entity) {
				return false
			}
		}
		b.WriteByte('}')
	default:
		if entity != nil {
			if m, ok := entity(v); ok {
				return encodeJSON(b, m, entity)
			}
		}
		return false
	}
	return true
}

// valueFields is the Field, Type and Value inputs every add and edit form
// shares. requireField is false on Add document and Create entity, where a
// document or entity with no field is a real thing to create.
func valueFields(noun, types, typeHelp, typePattern string, requireField bool, typ, value string) []console.Field {
	fieldHelp := "The " + noun + "'s name."
	if !requireField {
		fieldHelp = "Optional: the first " + noun + ". Leave it empty to create one with none, and add " +
			noun + "s on its page."
	}
	if typ == "" {
		typ = "string"
	}
	return []console.Field{
		{Name: "field", Label: capitalise(noun), Type: "text", Required: requireField, Help: fieldHelp},
		{Name: "type", Label: "Type", Type: "text", Required: true, Default: typ, Pattern: typePattern,
			Help: "One of " + types + "."},
		{Name: "value", Label: "Value", Type: "textarea", Default: value, Help: typeHelp},
	}
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// --- Firestore ---------------------------------------------------------------

const (
	firestoreTypeList    = "string, number, boolean, null, timestamp, geopoint, reference, map or array"
	firestoreTypePattern = `^(string|number|boolean|null|timestamp|geopoint|reference|map|array)$`
	firestoreValueHelp   = "As the type reads it: a number with a decimal point is a double and one without an " +
		"integer; a boolean is true or false; a timestamp is RFC 3339, such as 2026-09-27T15:04:05Z; a geopoint is " +
		"latitude, longitude; a reference is a document path such as users/alice; a map or array is JSON, whose " +
		"values are strings, numbers, booleans, null, maps and arrays. Empty for null."
	// firestoreIDPattern refuses a slash, which would make an ID a path. The
	// slash is escaped because the browser compiles a pattern with the v
	// flag, where an unescaped one inside a class is invalid.
	firestoreIDPattern = `^[^\/]+$`
)

// client is the Firestore client every screen uses, with the emulator's owner
// token: a console is an administrator, and security rules do not apply to it
// in Google's either.
func (p firestoreProvider) client(ctx context.Context, project string) (*firestore.Client, error) {
	opts := append(localOpts(p.endpoint),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(emulatorOwner{})))
	c, err := firestore.NewClient(ctx, project, opts...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Firestore: %w", err)
	}
	return c, nil
}

// firestoreCollection is the collection at a path: a top-level collection's
// ID, or a subcollection's whole path, users/alice/orders (#854).
func firestoreCollection(c *firestore.Client, path string) (*firestore.CollectionRef, error) {
	col := c.Collection(path)
	if col == nil {
		return nil, fmt.Errorf("%q is not a collection path: a collection is an ID, or document path/ID, "+
			"with an odd number of segments", path)
	}
	return col, nil
}

// firestoreTrail is the breadcrumb of a page inside a subcollection: each
// collection and document above it, each a link to its own page, then the
// document and field of the path's other segments. A top-level collection's
// pages need none; one crumb per segment is already right for them.
func firestoreTrail(collection string, rest ...string) []console.Crumb {
	if !strings.Contains(collection, "/") {
		return nil
	}
	parts := strings.Split(collection, "/")
	var trail []console.Crumb
	for i, part := range parts {
		if i%2 == 0 {
			trail = append(trail, console.Crumb{Label: part, Path: []string{strings.Join(parts[:i+1], "/")}})
		} else {
			trail = append(trail, console.Crumb{Label: part, Path: []string{strings.Join(parts[:i], "/"), part}})
		}
	}
	for i, r := range rest {
		trail = append(trail, console.Crumb{Label: r, Path: append([]string{collection}, rest[:i+1]...)})
	}
	trail[len(trail)-1].Path = nil
	return trail
}

// firestoreSubcollections is a document's Collections tab: its
// subcollections, from ListCollectionIds, each opening to its own page by its
// path.
func firestoreSubcollections(ctx context.Context, doc *firestore.DocumentRef, docPath string) console.Section {
	sec := console.Section{ID: "collections", Label: "Collections"}
	list := console.Listing{Columns: []string{"Documents"}, NameColumn: "Collection", Noun: "collections"}
	it := doc.Collections(ctx)
	for {
		col, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			sec.Unavailable = "listing subcollections: " + err.Error()
			return sec
		}
		count, more := countDocuments(ctx, col)
		shown := fmt.Sprint(count)
		if more {
			shown += "+"
		}
		list.Items = append(list.Items, console.Resource{
			Name: col.ID, Fields: map[string]string{"Documents": shown},
			Opens: []string{docPath + "/" + col.ID},
		})
		if len(list.Items) >= detailLimit {
			list.Note = truncatedNote(len(list.Items), "collections")
			break
		}
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	list.Total = len(list.Items)
	if list.Total == 0 {
		list.Note = "This document has no subcollections. Start collection adds one with its first document."
	}
	sec.Listing = list
	return sec
}

// firestoreDocumentsPrefix is the part of a document's full name before its
// path inside the database.
func firestoreDocumentsPrefix(project string) string {
	return "projects/" + project + "/databases/(default)/documents/"
}

// parseFirestoreValue reads a form's value as the type the form names.
func parseFirestoreValue(c *firestore.Client, project, typ, raw string) (any, error) {
	switch strings.TrimSpace(typ) {
	case "string":
		return raw, nil
	case "number":
		return parseNumber(raw)
	case "boolean":
		return parseBool(raw)
	case "null":
		if v := strings.TrimSpace(raw); v != "" && v != "null" {
			return nil, fmt.Errorf("a null has no value; leave Value empty")
		}
		return nil, nil
	case "timestamp":
		return parseTimestamp(raw)
	case "geopoint":
		lat, lng, err := parseLatLng(raw)
		if err != nil {
			return nil, err
		}
		return &latlng.LatLng{Latitude: lat, Longitude: lng}, nil
	case "reference":
		path := strings.Trim(strings.TrimSpace(raw), "/")
		path = strings.TrimPrefix(path, firestoreDocumentsPrefix(project))
		if strings.HasPrefix(path, "projects/") {
			return nil, fmt.Errorf("a reference names a document in this project's database, such as users/alice")
		}
		ref := c.Doc(path)
		if ref == nil {
			return nil, fmt.Errorf("%q is not a document path: a reference is collection/document, "+
				"with an even number of segments", raw)
		}
		return ref, nil
	case "map", "array":
		v, err := decodeJSON(raw, typ)
		if err != nil {
			return nil, err
		}
		if _, isMap := v.(map[string]any); typ == "map" && !isMap {
			return nil, errors.New("a map is written as a JSON object")
		}
		if _, isArray := v.([]any); typ == "array" && !isArray {
			return nil, errors.New("an array is written as a JSON array")
		}
		return firestoreFromJSON(v)
	}
	return nil, fmt.Errorf("type %q is not one of %s", typ, firestoreTypeList)
}

func firestoreFromJSON(v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		return jsonNumber(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			c, err := firestoreFromJSON(e)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			c, err := firestoreFromJSON(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

// formatFirestoreValue is a stored value as its edit form holds it: the type
// the form names and the text parseFirestoreValue reads back to the same
// value. ok is false for a value the form cannot hold without changing it.
func formatFirestoreValue(v any) (typ, raw string, ok bool) {
	switch t := v.(type) {
	case nil:
		return "null", "", true
	case bool:
		return "boolean", strconv.FormatBool(t), true
	case int64:
		return "number", strconv.FormatInt(t, 10), true
	case float64:
		return "number", formatDouble(t), true
	case string:
		return "string", t, true
	case time.Time:
		return "timestamp", formatTimestamp(t), true
	case *latlng.LatLng:
		return "geopoint", formatLatLng(t.GetLatitude(), t.GetLongitude()), true
	case *firestore.DocumentRef:
		_, rel, found := strings.Cut(t.Path, "/documents/")
		if !found {
			return "", "", false
		}
		return "reference", rel, true
	case map[string]any, []any:
		var b strings.Builder
		if !encodeJSON(&b, t, nil) {
			return "", "", false
		}
		if _, isMap := t.(map[string]any); isMap {
			return "map", b.String(), true
		}
		return "array", b.String(), true
	}
	return "", "", false
}

// renderFirestoreValue is a field's value on its document's page and its
// own: as the edit form writes it where the form can hold it, so a geopoint
// reads "51.5, -0.12" and a double in a map keeps its point, and a reference
// as its full name.
func renderFirestoreValue(v any) string {
	if typ, raw, ok := formatFirestoreValue(v); ok && typ != "null" && typ != "reference" {
		return raw
	}
	return renderValue(v)
}

// firestoreFirstField reads the optional first field of Add document.
func firestoreFirstField(c *firestore.Client, project string, values map[string]string) (map[string]any, error) {
	data := map[string]any{}
	name := values["field"]
	if strings.TrimSpace(name) == "" {
		if strings.TrimSpace(values["value"]) != "" {
			return nil, errors.New("a value needs a field name")
		}
		return data, nil
	}
	v, err := parseFirestoreValue(c, project, values["type"], values["value"])
	if err != nil {
		return nil, err
	}
	data[name] = v
	return data, nil
}

// addDocument creates a document with Create, so an ID already taken is the
// emulator's ALREADY_EXISTS rather than an overwrite. An empty ID is an auto
// ID, which is NewDoc's.
func (p firestoreProvider) addDocument(ctx context.Context, project, collection string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return "", err
	}
	defer c.Close()
	col, err := firestoreCollection(c, collection)
	if err != nil {
		return "", err
	}
	data, err := firestoreFirstField(c, project, values)
	if err != nil {
		return "", err
	}
	doc := col.NewDoc()
	if id := strings.TrimSpace(values["documentId"]); id != "" {
		if doc = col.Doc(id); doc == nil {
			return "", fmt.Errorf("%q is not a document ID: an ID has no slash", id)
		}
	}
	if _, err := doc.Create(ctx, data); err != nil {
		return "", err
	}
	return doc.ID, nil
}

// firestoreDocumentFields are Add document's inputs.
func firestoreDocumentFields() []console.Field {
	return append([]console.Field{
		{Name: "documentId", Label: "Document ID", Type: "text", Pattern: firestoreIDPattern,
			Help: "Optional. Empty gives the document an auto ID, as Firestore's own does."},
	}, valueFields("field", firestoreTypeList, firestoreValueHelp, firestoreTypePattern, false, "", "")...)
}

// CreateForm implements console.Creator: Start collection, which is Google's
// — a collection exists because a document is in it, so starting one is
// adding its first document.
func (firestoreProvider) CreateForm() (string, []console.Field) {
	return "Start collection", firestoreStartCollectionFields()
}

// firestoreStartCollectionFields are Start collection's inputs, on the
// Firestore screen and on a document's page, where the collection is a
// subcollection of that document (#854).
func firestoreStartCollectionFields() []console.Field {
	return append([]console.Field{
		{Name: "collection", Label: "Collection ID", Type: "text", Required: true, Pattern: firestoreIDPattern,
			Help: "The collection's ID, without a slash. It exists once its first document does."},
	}, firestoreDocumentFields()...)
}

// Create implements console.Creator: the collection's first document.
func (p firestoreProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	collection := strings.TrimSpace(values["collection"])
	if collection == "" {
		return "", errors.New("a collection ID is required")
	}
	if _, err := p.addDocument(ctx, project, collection, values); err != nil {
		return "", err
	}
	return collection, nil
}

// readDocument reads a document for a field's page and forms.
func (p firestoreProvider) readDocument(ctx context.Context, c *firestore.Client, collection, id string) (*firestore.DocumentRef, *firestore.DocumentSnapshot, error) {
	col, err := firestoreCollection(c, collection)
	if err != nil {
		return nil, nil, err
	}
	doc := col.Doc(id)
	if doc == nil {
		return nil, nil, fmt.Errorf("%q is not a document ID", id)
	}
	snap, err := doc.Get(ctx)
	if err != nil {
		return nil, nil, err
	}
	return doc, snap, nil
}

// fieldDetail is one field of a document: its type, its whole value, and
// the forms that change or remove it.
func (p firestoreProvider) fieldDetail(ctx context.Context, project, collection, id, field string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	_, snap, err := p.readDocument(ctx, c, collection, id)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the document: " + err.Error()}, nil
	}
	v, ok := snap.Data()[field]
	if !ok {
		return console.Detail{Unavailable: fmt.Sprintf("document %s has no field %q", snap.Ref.ID, field)}, nil
	}
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Document", Value: snap.Ref.Path},
			{Label: "Field", Value: field},
			{Label: "Type", Value: firestoreType(v)},
		},
		Sections: []console.Section{{ID: "value", Label: "Value", Kind: console.KindText, Text: renderFirestoreValue(v)}},
	}
	if typ, raw, ok := formatFirestoreValue(v); ok {
		d.Edit = &console.EditForm{
			Label:  "Edit field",
			Fields: firestoreEditFields(field, typ, raw),
			Note: "Saved with an update of this one field, which fails if the document changed since this " +
				"page read it. To rename a field, add it under the new name and delete this one.",
		}
	} else {
		d.Sections[0].Note = "This value cannot be edited here: the form writes a map or array as JSON, which " +
			"cannot hold the bytes, timestamp, geopoint or reference it contains without changing its type. " +
			"It can be deleted."
	}
	return d, nil
}

// firestoreEditFields is Edit field, prefilled with the stored type and value.
func firestoreEditFields(field, typ, raw string) []console.Field {
	fields := valueFields("field", firestoreTypeList, firestoreValueHelp, firestoreTypePattern, true, typ, raw)
	fields[0].Default, fields[0].Immutable = field, true
	fields[0].Help = "A field is renamed by adding it under the new name and deleting this one."
	return fields
}

// updateField sets or deletes one top-level field, with the document's update
// time as the precondition: a field is changed as the page showed it, and a
// document changed meanwhile is FAILED_PRECONDITION from the emulator rather
// than overwritten.
func (p firestoreProvider) updateField(ctx context.Context, project, collection, id, field string, mustExist bool, value func(*firestore.Client) (any, error)) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	if field == "" {
		return errors.New("a field name is required")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	doc, snap, err := p.readDocument(ctx, c, collection, id)
	if err != nil {
		return err
	}
	_, exists := snap.Data()[field]
	switch {
	case mustExist && !exists:
		return fmt.Errorf("document %s has no field %q", id, field)
	case !mustExist && exists:
		return fmt.Errorf("document %s already has a field %q; change it on its own page", id, field)
	}
	v, err := value(c)
	if err != nil {
		return err
	}
	_, err = doc.Update(ctx, []firestore.Update{{FieldPath: firestore.FieldPath{field}, Value: v}},
		firestore.LastUpdateTime(snap.UpdateTime))
	return err
}

// Edit implements console.Editor for one field of a document.
func (p firestoreProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 3 {
		return errors.New("a field is edited on its own page; a document's fields are added with Add field")
	}
	return p.updateField(ctx, project, path[0], path[1], path[2], true, func(c *firestore.Client) (any, error) {
		return parseFirestoreValue(c, project, values["type"], values["value"])
	})
}

// DetailActions offers Add document on a collection, Add field, Start
// collection and Delete document on a document, and Delete field on a field.
func (p firestoreProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if project == "" {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{{ID: "adddocument", Label: "Add document", Fields: firestoreDocumentFields()}}
	case 2:
		return []console.Action{
			{ID: "addfield", Label: "Add field",
				Fields: valueFields("field", firestoreTypeList, firestoreValueHelp, firestoreTypePattern, true, "", "")},
			// A subcollection of this document, started with its first
			// document, as on Google's console (#854).
			{ID: "startcollection", Label: "Start collection", Fields: firestoreStartCollectionFields()},
			{ID: "deletedocument", Label: "Delete document", Destructive: true, Leaves: true},
		}
	case 3:
		return []console.Action{{ID: "deletefield", Label: "Delete field", Destructive: true, Leaves: true}}
	}
	return nil
}

// ActAt implements console.PathActor.
func (p firestoreProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	switch {
	case action == "adddocument" && len(path) == 1:
		_, err := p.addDocument(ctx, project, path[0], values)
		return err
	case action == "startcollection" && len(path) == 2:
		sub := strings.TrimSpace(values["collection"])
		if sub == "" || strings.Contains(sub, "/") {
			return fmt.Errorf("%q is not a collection ID: an ID is required and has no slash", sub)
		}
		_, err := p.addDocument(ctx, project, path[0]+"/"+path[1]+"/"+sub, values)
		return err
	case action == "addfield" && len(path) == 2:
		return p.updateField(ctx, project, path[0], path[1], values["field"], false, func(c *firestore.Client) (any, error) {
			return parseFirestoreValue(c, project, values["type"], values["value"])
		})
	case action == "deletefield" && len(path) == 3:
		return p.updateField(ctx, project, path[0], path[1], path[2], true, func(*firestore.Client) (any, error) {
			return firestore.Delete, nil
		})
	case action == "deletedocument" && len(path) == 2:
		if project == "" {
			return errors.New("choose a project first")
		}
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		c, err := p.client(ctx, project)
		if err != nil {
			return err
		}
		defer c.Close()
		col := c.Collection(path[0])
		if col == nil || col.Doc(path[1]) == nil {
			return fmt.Errorf("%s/%s is not a document path", path[0], path[1])
		}
		// Exists, so a document already gone is the emulator's NOT_FOUND
		// rather than a delete that reports success for nothing. Its
		// subcollections are not deleted with it, in Firestore or here.
		_, err = col.Doc(path[1]).Delete(ctx, firestore.Exists)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// --- Datastore ---------------------------------------------------------------

const (
	datastoreTypeList    = "string, integer, float, boolean, timestamp, key, geopoint, array, entity or null"
	datastoreTypePattern = `^(string|integer|float|boolean|timestamp|key|geopoint|array|entity|null)$`
	datastoreValueHelp   = "As the type reads it: a timestamp is RFC 3339, such as 2026-09-27T15:04:05Z; a key is " +
		"Kind/name or Kind/id=123, with ancestors first, such as Customer/alice/Order/id=7; a geopoint is " +
		"latitude, longitude; an array is a JSON array and an entity (an embedded entity) a JSON object, whose " +
		"values are strings, numbers (with a decimal point for a float), booleans, null, objects and arrays. " +
		"Empty for null."
)

// parseKeyPath reads Kind/name or Kind/id=123 pairs, ancestors first.
func parseKeyPath(raw string) (*datastore.Key, error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(raw), "/"), "/")
	if len(parts) < 2 || len(parts)%2 != 0 {
		return nil, fmt.Errorf("%q is not a key: a key is Kind/name or Kind/id=123, ancestors first", raw)
	}
	var key *datastore.Key
	for i := 0; i < len(parts); i += 2 {
		k, err := datastoreKey(parts[i], parts[i+1])
		if err != nil {
			return nil, err
		}
		k.Parent = key
		key = k
	}
	return key, nil
}

// formatKeyPath writes a key as parseKeyPath reads it. ok is false for a key
// in a namespace, which the path cannot carry.
func formatKeyPath(k *datastore.Key) (string, bool) {
	var parts []string
	for ; k != nil; k = k.Parent {
		if k.Namespace != "" {
			return "", false
		}
		id := k.Name
		if id == "" {
			id = fmt.Sprintf("id=%d", k.ID)
		}
		parts = append([]string{k.Kind, id}, parts...)
	}
	return strings.Join(parts, "/"), true
}

// --- Datastore paths: namespaces and ancestors (#854) -------------------------

// datastoreNamespaceSegment begins the path of a page in a namespace other
// than the default: [__namespace__, ns, kind, key, property]. A kind name
// that begins and ends with two underscores is reserved by Datastore, so no
// kind can be named this and the first segment is never ambiguous.
const datastoreNamespaceSegment = "__namespace__"

// datastoreNamespacePattern is Datastore's rule for a namespace name.
const datastoreNamespacePattern = `^[0-9A-Za-z._\-]{0,100}$`

var datastoreNamespaceName = regexp.MustCompile(datastoreNamespacePattern)

// datastoreScope is the namespace a Datastore page is in.
type datastoreScope struct {
	ns string
	// namespaced is true when the path named the namespace, which it does
	// for every namespace but the default.
	namespaced bool
	// index is the page listing the namespaces themselves.
	index bool
}

// parseDatastorePath splits a page's path into its namespace and the rest:
// kind, entity key and property.
func parseDatastorePath(path []string) (datastoreScope, []string) {
	if len(path) == 0 || path[0] != datastoreNamespaceSegment {
		return datastoreScope{}, path
	}
	if len(path) == 1 {
		return datastoreScope{index: true, namespaced: true}, nil
	}
	return datastoreScope{ns: path[1], namespaced: true}, path[2:]
}

// at is the path of rest in this namespace.
func (s datastoreScope) at(rest ...string) []string {
	if !s.namespaced || s.index {
		return append([]string{}, rest...)
	}
	return append([]string{datastoreNamespaceSegment, s.ns}, rest...)
}

// trail is the breadcrumb of a page in a namespace, whose first two
// segments are an address and not a level: Namespaces, the namespace, then
// the kind, key and property. A default-namespace page needs none.
func (s datastoreScope) trail(rest ...string) []console.Crumb {
	if !s.namespaced {
		return nil
	}
	trail := []console.Crumb{{Label: "Namespaces", Path: []string{datastoreNamespaceSegment}}}
	if !s.index {
		trail = append(trail, console.Crumb{Label: s.ns, Path: s.at()})
		for i, r := range rest {
			trail = append(trail, console.Crumb{Label: r, Path: s.at(rest[:i+1]...)})
		}
	}
	trail[len(trail)-1].Path = nil
	return trail
}

// datastoreEntityKey reads back the key an entity row was named by: a root
// entity's name or id=N, or a child entity's whole key path,
// Customer/alice/Order/id=7, whose last kind is the page's. The namespace is
// the page's.
//
// A child's names are escaped in its path (datastoreKeyPath). A root
// entity's name is not, so one shaped like a child's path — four or more
// slash-separated parts, an even number, the last kind this one — cannot be
// told from a child and is read as the child; that is the one name this
// addressing cannot reach.
func datastoreEntityKey(ns, kind, id string) (*datastore.Key, error) {
	var key *datastore.Key
	if parts := strings.Split(id, "/"); len(parts) >= 4 && len(parts)%2 == 0 && parts[len(parts)-2] == kind {
		for i := 0; i < len(parts); i += 2 {
			name, err := url.PathUnescape(parts[i+1])
			if err != nil {
				return nil, fmt.Errorf("%q is not a key path: %v", id, err)
			}
			k, err := datastoreKey(parts[i], name)
			if err != nil {
				return nil, err
			}
			k.Parent = key
			key = k
		}
	} else {
		k, err := datastoreKey(kind, id)
		if err != nil {
			return nil, err
		}
		key = k
	}
	for k := key; k != nil; k = k.Parent {
		k.Namespace = ns
	}
	return key, nil
}

// datastoreKeySegment is how a listing names an entity: a root entity by its
// name or id=N, and a child by its key path, which is what tells it from a
// root entity with the same ID and what its page is addressed by.
func datastoreKeySegment(k *datastore.Key) string {
	if k.Parent == nil {
		return entityKeyName(k)
	}
	return datastoreKeyPath(k)
}

// datastoreKeyPath is a key as Kind/name pairs, ancestors first, whatever its
// namespace. A name holding a slash or a percent sign has it escaped (%2F,
// %25), so a name cannot split into two segments and the path reads back to
// the key it was written from.
func datastoreKeyPath(k *datastore.Key) string {
	var parts []string
	for ; k != nil; k = k.Parent {
		parts = append([]string{k.Kind, keyNameEscaper.Replace(entityKeyName(k))}, parts...)
	}
	return strings.Join(parts, "/")
}

var keyNameEscaper = strings.NewReplacer("%", "%25", "/", "%2F")

// entityKeyName is how the listing names an entity: its name, or id=N.
func entityKeyName(k *datastore.Key) string {
	if k.Name != "" {
		return k.Name
	}
	return fmt.Sprintf("id=%d", k.ID)
}

// parseDatastoreValue reads a form's value as the type the form names.
func parseDatastoreValue(typ, raw string) (any, error) {
	switch strings.TrimSpace(typ) {
	case "string":
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a 64-bit integer", raw)
		}
		return n, nil
	case "float":
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", raw)
		}
		return f, nil
	case "boolean":
		return parseBool(raw)
	case "null":
		if v := strings.TrimSpace(raw); v != "" && v != "null" {
			return nil, fmt.Errorf("a null has no value; leave Value empty")
		}
		return nil, nil
	case "timestamp":
		return parseTimestamp(raw)
	case "key":
		return parseKeyPath(raw)
	case "geopoint":
		lat, lng, err := parseLatLng(raw)
		if err != nil {
			return nil, err
		}
		return datastore.GeoPoint{Lat: lat, Lng: lng}, nil
	case "array", "entity":
		v, err := decodeJSON(raw, typ)
		if err != nil {
			return nil, err
		}
		if _, isArray := v.([]any); typ == "array" && !isArray {
			return nil, errors.New("an array is written as a JSON array")
		}
		if _, isMap := v.(map[string]any); typ == "entity" && !isMap {
			return nil, errors.New("an embedded entity is written as a JSON object")
		}
		return datastoreFromJSON(v)
	}
	return nil, fmt.Errorf("type %q is not one of %s", typ, datastoreTypeList)
}

func datastoreFromJSON(v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		return jsonNumber(t)
	case map[string]any:
		e := &datastore.Entity{}
		for _, k := range sortedAnyKeys(t) {
			c, err := datastoreFromJSON(t[k])
			if err != nil {
				return nil, err
			}
			e.Properties = append(e.Properties, datastore.Property{Name: k, Value: c})
		}
		return e, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			c, err := datastoreFromJSON(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

// datastoreEntityJSON is an embedded entity as a JSON object, when it is one:
// no key, and every property indexed as the property holding it is — an
// entity in an excluded property reads back with its own properties excluded
// (measured against the emulator) — because the object cannot say otherwise.
func datastoreEntityJSON(noIndex bool) func(any) (map[string]any, bool) {
	return func(v any) (map[string]any, bool) {
		e, ok := v.(*datastore.Entity)
		if !ok || e == nil || e.Key != nil {
			return nil, false
		}
		m := make(map[string]any, len(e.Properties))
		for _, p := range e.Properties {
			if p.NoIndex != noIndex {
				return nil, false
			}
			m[p.Name] = p.Value
		}
		return m, true
	}
}

// datastoreType names a property's type as the form does.
func datastoreType(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64:
		return "integer"
	case float64:
		return "float"
	case string:
		return "string"
	case time.Time:
		return "timestamp"
	case *datastore.Key:
		return "key"
	case datastore.GeoPoint:
		return "geopoint"
	case []any:
		return fmt.Sprintf("array (%d)", len(t))
	case *datastore.Entity:
		return "entity"
	case []byte:
		return "blob"
	}
	return fmt.Sprintf("%T", v)
}

// renderDatastoreValue is a property's value written out in full.
func renderDatastoreValue(v any, noIndex bool) string {
	switch t := v.(type) {
	case *datastore.Key:
		if s, ok := formatKeyPath(t); ok {
			return s
		}
		return t.String()
	case datastore.GeoPoint:
		return formatLatLng(t.Lat, t.Lng)
	case *datastore.Entity, []any:
		var b strings.Builder
		if encodeJSON(&b, t, datastoreEntityJSON(noIndex)) {
			return b.String()
		}
		return fmt.Sprintf("%v", t)
	}
	return renderValue(v)
}

// formatDatastoreValue is a stored value as its edit form holds it; ok is
// false for one the form cannot hold without changing it.
func formatDatastoreValue(v any, noIndex bool) (typ, raw string, ok bool) {
	switch t := v.(type) {
	case nil:
		return "null", "", true
	case bool:
		return "boolean", strconv.FormatBool(t), true
	case int64:
		return "integer", strconv.FormatInt(t, 10), true
	case float64:
		return "float", formatDouble(t), true
	case string:
		return "string", t, true
	case time.Time:
		return "timestamp", formatTimestamp(t), true
	case *datastore.Key:
		s, ok := formatKeyPath(t)
		return "key", s, ok
	case datastore.GeoPoint:
		return "geopoint", formatLatLng(t.Lat, t.Lng), true
	case []any, *datastore.Entity:
		var b strings.Builder
		if !encodeJSON(&b, t, datastoreEntityJSON(noIndex)) {
			return "", "", false
		}
		if _, isArray := t.([]any); isArray {
			return "array", b.String(), true
		}
		return "entity", b.String(), true
	}
	return "", "", false
}

// datastoreProperty reads a form's property: name, typed value and index flag.
func datastoreProperty(name string, values map[string]string) (datastore.Property, error) {
	v, err := parseDatastoreValue(values["type"], values["value"])
	if err != nil {
		return datastore.Property{}, err
	}
	return datastore.Property{Name: name, Value: v, NoIndex: values["excluded"] == "true"}, nil
}

// datastorePropertyFields are a property's inputs, with the index flag.
func datastorePropertyFields(requireField bool, typ, value string, excluded bool) []console.Field {
	fields := valueFields("property", datastoreTypeList, datastoreValueHelp, datastoreTypePattern, requireField, typ, value)
	return append(fields, console.Field{Name: "excluded", Label: "Exclude from indexes", Type: "checkbox",
		Default: strconv.FormatBool(excluded),
		Help: "An excluded property cannot be filtered or ordered on. A string over 1,500 bytes " +
			"must be excluded; Datastore refuses it otherwise."})
}

// datastoreEntityFields are Create entity's inputs on a kind's page.
func datastoreEntityFields() []console.Field {
	return append([]console.Field{
		{Name: "key", Label: "Key identifier", Type: "text",
			Help: "Optional. A name, or id=123 for a numeric ID; empty lets Datastore allocate a numeric ID."},
	}, datastorePropertyFields(false, "", "", false)...)
}

// datastoreKindField is the Kind input of a Create entity that names its
// kind: on the Datastore screen, on a namespace's page, and Create child
// entity.
func datastoreKindField(help string) console.Field {
	return console.Field{Name: "kind", Label: "Kind", Type: "text", Required: true, Help: help}
}

// datastoreChildFields are Create child entity's inputs on an entity's page.
func datastoreChildFields() []console.Field {
	return append([]console.Field{datastoreKindField(
		"The child's kind. Its parent is this entity, so its key path begins with this entity's.")},
		datastoreEntityFields()...)
}

// client is the Datastore client every screen uses.
func (p datastoreProvider) client(ctx context.Context, project string) (*datastore.Client, error) {
	c, err := datastore.NewClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Datastore: %w", err)
	}
	return c, nil
}

// datastoreError is the one error in a MultiError, so the console shows the
// emulator's status rather than "(and 0 other errors)".
func datastoreError(err error) error {
	var multi datastore.MultiError
	if errors.As(err, &multi) && len(multi) == 1 {
		return multi[0]
	}
	return err
}

// createEntity inserts an entity. An insert, not a put, so a key already
// taken is the emulator's ALREADY_EXISTS rather than an overwrite.
//
// The entity is in namespace ns, and a child of parent when parent is not
// nil (#854).
func (p datastoreProvider) createEntity(ctx context.Context, project, ns, kind string, parent *datastore.Key, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	if kind == "" {
		return "", errors.New("a kind is required")
	}
	key := datastore.IncompleteKey(kind, parent)
	if id := strings.TrimSpace(values["key"]); id != "" {
		k, err := datastoreKey(kind, id)
		if err != nil {
			return "", err
		}
		k.Parent = parent
		key = k
	}
	key.Namespace = ns
	var props datastore.PropertyList
	if name := values["field"]; strings.TrimSpace(name) != "" {
		prop, err := datastoreProperty(name, values)
		if err != nil {
			return "", err
		}
		props = append(props, prop)
	} else if strings.TrimSpace(values["value"]) != "" {
		return "", errors.New("a value needs a property name")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return "", err
	}
	defer c.Close()
	keys, err := c.Mutate(ctx, datastore.NewInsert(key, &props))
	if err != nil {
		return "", datastoreError(err)
	}
	return datastoreKeySegment(keys[0]), nil
}

// CreateForm implements console.Creator: Create entity, with its kind, as on
// Google's Datastore page — a kind exists because an entity has it.
func (datastoreProvider) CreateForm() (string, []console.Field) {
	return "Create entity", append([]console.Field{
		{Name: "namespace", Label: "Namespace", Type: "text", Pattern: datastoreNamespacePattern,
			Help: "Optional: empty for the default namespace. A new namespace exists once its first entity does. " +
				"Letters, digits, periods, underscores and dashes, at most 100."},
		datastoreKindField("The entity's kind. A new kind exists once its first entity does."),
	}, datastoreEntityFields()...)
}

// namespaceValue reads the Namespace input, refusing what Datastore would.
func namespaceValue(raw string) (string, error) {
	ns := strings.TrimSpace(raw)
	if !datastoreNamespaceName.MatchString(ns) {
		return "", fmt.Errorf("%q is not a namespace: letters, digits, periods, underscores and dashes, at most 100", ns)
	}
	if strings.HasPrefix(ns, "__") && strings.HasSuffix(ns, "__") {
		return "", fmt.Errorf("namespace %q is reserved: a name that begins and ends with two underscores is Datastore's", ns)
	}
	return ns, nil
}

// Create implements console.Creator; it returns the kind, whose page lists
// the new entity.
func (p datastoreProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	kind := strings.TrimSpace(values["kind"])
	if kind == "" {
		return "", errors.New("a kind is required")
	}
	ns, err := namespaceValue(values["namespace"])
	if err != nil {
		return "", err
	}
	if _, err := p.createEntity(ctx, project, ns, kind, nil, values); err != nil {
		return "", err
	}
	return kind, nil
}

// changeEntity reads an entity, changes its properties and writes it back
// with an update, in one transaction: an entity changed meanwhile aborts the
// transaction rather than being overwritten, and one deleted meanwhile is the
// emulator's NOT_FOUND rather than recreated.
func (p datastoreProvider) changeEntity(ctx context.Context, project string, scope datastoreScope, kind, id string, change func(datastore.PropertyList) (datastore.PropertyList, error)) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	key, err := datastoreEntityKey(scope.ns, kind, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		var props datastore.PropertyList
		if err := tx.Get(key, &props); err != nil {
			return err
		}
		changed, err := change(props)
		if err != nil {
			return err
		}
		_, err = tx.Mutate(datastore.NewUpdate(key, &changed))
		return err
	}, datastore.MaxAttempts(1))
	return datastoreError(err)
}

// propertyIndex finds a property by name.
func propertyIndex(props datastore.PropertyList, name string) int {
	for i, p := range props {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// propertyDetail is one property of an entity: its type, index flag, whole
// value, and the forms that change or remove it.
func (p datastoreProvider) propertyDetail(ctx context.Context, project string, scope datastoreScope, kind, id, name string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	key, err := datastoreEntityKey(scope.ns, kind, id)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer c.Close()
	var props datastore.PropertyList
	if err := c.Get(ctx, key, &props); err != nil {
		return console.Detail{Unavailable: "cannot read the entity: " + err.Error()}, nil
	}
	i := propertyIndex(props, name)
	if i < 0 {
		return console.Detail{Unavailable: fmt.Sprintf("entity %s has no property %q", id, name)}, nil
	}
	prop := props[i]
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Kind", Value: kind},
			{Label: "Key", Value: id},
			{Label: "Property", Value: name},
			{Label: "Type", Value: datastoreType(prop.Value)},
			{Label: "Indexed", Value: yesNo(!prop.NoIndex)},
		},
		Sections: []console.Section{{ID: "value", Label: "Value", Kind: console.KindText,
			Text: renderDatastoreValue(prop.Value, prop.NoIndex)}},
	}
	if typ, raw, ok := formatDatastoreValue(prop.Value, prop.NoIndex); ok {
		fields := datastorePropertyFields(true, typ, raw, prop.NoIndex)
		fields[0].Default, fields[0].Immutable = name, true
		fields[0].Help = "A property is renamed by adding it under the new name and deleting this one."
		d.Edit = &console.EditForm{
			Label: "Edit property", Fields: fields,
			Note: "Saved by writing the entity back with this property changed, in a transaction, so a change " +
				"made meanwhile is not overwritten.",
		}
	} else {
		d.Sections[0].Note = "This value cannot be edited here: the form cannot hold a blob, a key in a " +
			"namespace, or an embedded entity with a key or with properties indexed unlike the property holding it " +
			"without changing it. " +
			"It can be deleted."
	}
	return d, nil
}

// Edit implements console.Editor for one property of an entity.
func (p datastoreProvider) Edit(ctx context.Context, project string, full []string, values map[string]string) error {
	scope, path := parseDatastorePath(full)
	if scope.index || len(path) != 3 {
		return errors.New("a property is edited on its own page; an entity's properties are added with Add property")
	}
	name := path[2]
	prop, err := datastoreProperty(name, values)
	if err != nil {
		return err
	}
	return p.changeEntity(ctx, project, scope, path[0], path[1], func(props datastore.PropertyList) (datastore.PropertyList, error) {
		i := propertyIndex(props, name)
		if i < 0 {
			return nil, fmt.Errorf("entity %s has no property %q", path[1], name)
		}
		props[i] = prop
		return props, nil
	})
}

// DetailActions offers Create entity on a kind and on a namespace's page, Add
// property, Create child entity and Delete entity on an entity, and Delete
// property on a property.
func (p datastoreProvider) DetailActions(_ context.Context, project string, full []string) []console.Action {
	if project == "" {
		return nil
	}
	scope, path := parseDatastorePath(full)
	if scope.index {
		return nil
	}
	switch len(path) {
	case 0:
		if !scope.namespaced {
			return nil
		}
		return []console.Action{{ID: "createentity", Label: "Create entity",
			Fields: append([]console.Field{datastoreKindField("The entity's kind, in namespace " + scope.ns + ".")},
				datastoreEntityFields()...)}}
	case 1:
		return []console.Action{{ID: "createentity", Label: "Create entity", Fields: datastoreEntityFields()}}
	case 2:
		return []console.Action{
			{ID: "addproperty", Label: "Add property", Fields: datastorePropertyFields(true, "", "", false)},
			// A child entity, whose parent is this one (#854).
			{ID: "createchild", Label: "Create child entity", Fields: datastoreChildFields()},
			{ID: "deleteentity", Label: "Delete entity", Destructive: true, Leaves: true},
		}
	case 3:
		return []console.Action{{ID: "deleteproperty", Label: "Delete property", Destructive: true, Leaves: true}}
	}
	return nil
}

// ActAt implements console.PathActor.
func (p datastoreProvider) ActAt(ctx context.Context, project string, full []string, action string, values map[string]string) error {
	scope, path := parseDatastorePath(full)
	if scope.index {
		return fmt.Errorf("unknown action %q", action)
	}
	switch {
	case action == "createentity" && len(path) == 0 && scope.namespaced:
		_, err := p.createEntity(ctx, project, scope.ns, strings.TrimSpace(values["kind"]), nil, values)
		return err
	case action == "createentity" && len(path) == 1:
		_, err := p.createEntity(ctx, project, scope.ns, path[0], nil, values)
		return err
	case action == "createchild" && len(path) == 2:
		parent, err := datastoreEntityKey(scope.ns, path[0], path[1])
		if err != nil {
			return err
		}
		// The parent is read first: Datastore writes a child of a key that
		// holds nothing, which is a real state but never what a form on the
		// parent's page means.
		if err := p.mustExist(ctx, project, parent); err != nil {
			return err
		}
		_, err = p.createEntity(ctx, project, scope.ns, strings.TrimSpace(values["kind"]), parent, values)
		return err
	case action == "addproperty" && len(path) == 2:
		name := values["field"]
		if strings.TrimSpace(name) == "" {
			return errors.New("a property name is required")
		}
		prop, err := datastoreProperty(name, values)
		if err != nil {
			return err
		}
		return p.changeEntity(ctx, project, scope, path[0], path[1], func(props datastore.PropertyList) (datastore.PropertyList, error) {
			if propertyIndex(props, name) >= 0 {
				return nil, fmt.Errorf("entity %s already has a property %q; change it on its own page", path[1], name)
			}
			return append(props, prop), nil
		})
	case action == "deleteproperty" && len(path) == 3:
		return p.changeEntity(ctx, project, scope, path[0], path[1], func(props datastore.PropertyList) (datastore.PropertyList, error) {
			i := propertyIndex(props, path[2])
			if i < 0 {
				return nil, fmt.Errorf("entity %s has no property %q", path[1], path[2])
			}
			return append(props[:i], props[i+1:]...), nil
		})
	case action == "deleteentity" && len(path) == 2:
		return p.deleteEntity(ctx, project, scope, path[0], path[1])
	}
	return fmt.Errorf("unknown action %q", action)
}

// mustExist reads a key, so a write under it is refused when it holds
// nothing.
func (p datastoreProvider) mustExist(ctx context.Context, project string, key *datastore.Key) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	var props datastore.PropertyList
	return c.Get(ctx, key, &props)
}

// deleteEntity removes an entity. Datastore's delete succeeds for a key that
// holds nothing, so the entity is read first in the same transaction: one
// already gone is the client's "no such entity", not a success.
func (p datastoreProvider) deleteEntity(ctx context.Context, project string, scope datastoreScope, kind, id string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	key, err := datastoreEntityKey(scope.ns, kind, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		var props datastore.PropertyList
		if err := tx.Get(key, &props); err != nil {
			return err
		}
		return tx.Delete(key)
	}, datastore.MaxAttempts(1))
	return datastoreError(err)
}

var (
	_ console.Creator   = firestoreProvider{}
	_ console.Editor    = firestoreProvider{}
	_ console.PathActor = firestoreProvider{}
	_ console.Creator   = datastoreProvider{}
	_ console.Editor    = datastoreProvider{}
	_ console.PathActor = datastoreProvider{}
)
