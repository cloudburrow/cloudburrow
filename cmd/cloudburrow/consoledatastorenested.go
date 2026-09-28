package main

// Editing a value inside an array or an embedded entity (#905).
//
// Edit property's form holds an array or an embedded entity as JSON, and
// JSON has no key, timestamp or geopoint, so since #894 such a property was
// shown by type and offered no edit: a key in an array could only be deleted
// with the whole property and added again through the API. Google's
// Datastore console edits the values inside one in place. A property whose
// value is an array or an embedded entity now lists every value inside it, at
// any depth, on an Elements tab, each with its own page and an Edit value
// form, typed as Edit property's is. Saving it writes back that one value,
// through the v1 API in a transaction as Edit property does (changeEntity):
// the rest of the property and of the entity go back exactly as Lookup
// returned them, their index flags and meanings included, and the value
// keeps its own index flag.
//
// An element is addressed by its steps from the property, written as a JSON
// array — [2] is the third value of an array, ["line", "when"] the when
// property of the embedded entity line, [1, "__key__"] the key of the
// embedded entity that is an array's second value. Datastore reserves names
// that begin and end with two underscores, so no property collides with an
// embedded entity's __key__. A key in another project or database (#893) is
// listed and offered no edit.
//
// Adding and removing values (#911). Edit value changes a value but not the
// shape of what holds it, so an array holding a key, a timestamp or a
// geopoint could not grow or shrink at all: Edit property's JSON cannot hold
// it (#894). Every array and embedded entity inside a property is now listed
// as well, before the values inside it, and Add value adds a value to one —
// at an index of an array, or appended; as a new property of an embedded
// entity, or as its key when it has none — and Remove value, confirmed as a
// delete is, removes the value its row or page addresses. Both are written
// through changeEntity, like Edit value, so every other value of the entity
// goes back exactly as Lookup returned it, index flags and meanings included.
//
// A blob is shown and edited as base64 (#912), standard encoding with
// padding, as the v1 REST API's blobValue is; Edit value has an Exclude from
// indexes checkbox, prefilled with the value's own flag, because a blob or a
// string over 1,500 bytes must be excluded, which Datastore enforces.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// datastoreKeyStep is an embedded entity's own key, as an element's step.
const datastoreKeyStep = "__key__"

// datastoreElementScan bounds the values an Elements tab lists.
const datastoreElementScan = 1000

// The Edit value form's types: one value, not an array or an entity, whose
// values are each edited on their own.
const (
	datastoreElementTypeList    = "string, integer, float, boolean, timestamp, key, geopoint, blob or null"
	datastoreElementTypePattern = `^(string|integer|float|boolean|timestamp|key|geopoint|blob|null)$`
)

// datastoreElement is one value inside an array or embedded entity property.
type datastoreElement struct {
	Steps []any // int for an array's index, string for a property or __key__
	Value *datastorepb.Value
}

// datastoreElementSegment writes an element's steps as its page's path
// segment: a JSON array.
func datastoreElementSegment(steps []any) string {
	b, _ := json.Marshal(steps)
	return string(b)
}

// parseDatastoreElement reads an element's path segment back to its steps.
func parseDatastoreElement(seg string) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(seg))
	dec.UseNumber()
	var raw []any
	if err := dec.Decode(&raw); err != nil || dec.More() || len(raw) == 0 {
		return nil, fmt.Errorf("%q is not a value's address inside a property: a JSON array of indexes and names, such as [2] or [\"line\", \"when\"]", seg)
	}
	steps := make([]any, len(raw))
	for i, r := range raw {
		switch t := r.(type) {
		case json.Number:
			n, err := strconv.Atoi(t.String())
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q is not an array index", t)
			}
			steps[i] = n
		case string:
			steps[i] = t
		default:
			return nil, fmt.Errorf("%v is neither an array index nor a property name", r)
		}
	}
	return steps, nil
}

var (
	datastorePlainName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	datastoreElementType = regexp.MustCompile(datastoreElementTypePattern)
)

// datastoreElementLabel names an element as its row and page show it: the
// property, then [i] for an array index and .name for a property of an
// embedded entity, a name that is not a plain identifier written ["…"].
func datastoreElementLabel(property string, steps []any) string {
	var b strings.Builder
	b.WriteString(property)
	for _, s := range steps {
		switch t := s.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", t)
		case string:
			if datastorePlainName.MatchString(t) {
				b.WriteString("." + t)
			} else {
				q, _ := json.Marshal(t)
				b.WriteString("[" + string(q) + "]")
			}
		}
	}
	return b.String()
}

// datastoreElements lists the values inside an array or embedded entity, at
// any depth, depth first, an entity's key before its properties and its
// properties by name. An array or entity inside is listed itself, before the
// values inside it, so values can be added to it and it can be removed
// (#911).
func datastoreElements(v *datastorepb.Value) []datastoreElement {
	var out []datastoreElement
	var walk func(v *datastorepb.Value, steps []any)
	walk = func(v *datastorepb.Value, steps []any) {
		at := func(s any) []any { return append(append([]any{}, steps...), s) }
		if len(steps) > 0 && isDatastoreContainer(v) {
			out = append(out, datastoreElement{Steps: steps, Value: v})
		}
		switch t := v.GetValueType().(type) {
		case *datastorepb.Value_ArrayValue:
			for i, e := range t.ArrayValue.GetValues() {
				walk(e, at(i))
			}
		case *datastorepb.Value_EntityValue:
			if k := t.EntityValue.GetKey(); len(k.GetPath()) > 0 {
				out = append(out, datastoreElement{Steps: at(datastoreKeyStep),
					Value: &datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: k}}})
			}
			props := t.EntityValue.GetProperties()
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				walk(props[n], at(n))
			}
		default:
			if len(steps) > 0 {
				out = append(out, datastoreElement{Steps: steps, Value: v})
			}
		}
	}
	walk(v, nil)
	return out
}

// isDatastoreKeyStep is whether steps address an embedded entity's own key.
func isDatastoreKeyStep(steps []any) bool {
	s, ok := steps[len(steps)-1].(string)
	return ok && s == datastoreKeyStep
}

// isDatastoreContainer is whether a value is an array or an embedded entity,
// which values are added to (#911).
func isDatastoreContainer(v *datastorepb.Value) bool {
	switch v.GetValueType().(type) {
	case *datastorepb.Value_ArrayValue, *datastorepb.Value_EntityValue:
		return true
	}
	return false
}

// datastoreExcludedHelp is the Exclude from indexes checkbox's help on a
// value inside a property.
const datastoreExcludedHelp = "An excluded value cannot be filtered or ordered on. A string or blob over 1,500 " +
	"bytes must be excluded; Datastore refuses it otherwise. In an array, excluded values go after the indexed " +
	"ones, which is where Datastore stores them."

// datastoreElementForm is an element's Edit value form, prefilled; ok is
// false, with why, for a value the form cannot hold without changing it.
// noIndex is the value's own index flag, which the form's Exclude from
// indexes is prefilled with; an embedded entity's key has none.
func datastoreElementForm(v any, keyStep, noIndex bool) (fields []console.Field, why string, ok bool) {
	switch v.(type) {
	case datastoreForeignKey:
		return nil, datastoreNoEditNote(v), false
	case []any, *datastore.Entity, datastoreForeignEntity:
		return nil, "An array or embedded entity inside a property is changed value by value: each value " +
			"inside it is listed after it, with Edit value, and Add value and Remove value change what it holds.", false
	}
	typ, raw, holdable := formatDatastoreValue(v, false)
	if !holdable {
		return nil, "This value cannot be edited here.", false
	}
	types, pattern := datastoreElementTypeList, datastoreElementTypePattern
	if keyStep {
		// An embedded entity's key is a key or nothing.
		types, pattern = "key", `^key$`
	}
	fields = []console.Field{
		{Name: "type", Label: "Type", Type: "text", Required: true, Default: typ, Pattern: pattern,
			Help: "One of " + types + "."},
		{Name: "value", Label: "Value", Type: "textarea", Default: raw, Help: datastoreValueHelp},
	}
	if !keyStep {
		fields = append(fields, console.Field{Name: "excluded", Label: "Exclude from indexes", Type: "checkbox",
			Default: strconv.FormatBool(noIndex), Help: datastoreExcludedHelp})
	}
	return fields, "", true
}

// datastoreRowValue is a value as a listing's row shows it: in full, as its
// page does, except a blob's base64, which is shortened (#912).
func datastoreRowValue(v any, noIndex bool) string {
	s := renderDatastoreValue(v, noIndex)
	if _, isBlob := v.([]byte); isBlob {
		return summarise(s)
	}
	return s
}

// datastoreElementAction is an element's Edit value.
func datastoreElementAction(fields []console.Field) console.Action {
	return console.Action{ID: "editvalue", Label: "Edit value", Fields: fields}
}

// lookupDatastoreElement reads the entity and finds the element its page
// addresses, and the property holding it, whose digest the element's actions
// carry (#923).
func (p datastoreProvider) lookupDatastoreElement(ctx context.Context, project string, scope datastoreScope, kind, id, name string, steps []any) (*datastore.Key, datastoreElement, *datastorepb.Value, error) {
	key, err := datastoreEntityKey(project, scope.ns, kind, id)
	if err != nil {
		return nil, datastoreElement{}, nil, err
	}
	c, done, err := p.rawDatastore()
	if err != nil {
		return nil, datastoreElement{}, nil, err
	}
	defer done()
	e, err := lookupEntity(ctx, c, project, key, nil)
	if err != nil {
		return nil, datastoreElement{}, nil, fmt.Errorf("cannot read the entity: %w", err)
	}
	prop, has := e.GetProperties()[name]
	if !has {
		return nil, datastoreElement{}, nil, fmt.Errorf("entity %s has no property %q", datastoreEntityHeading(key), name)
	}
	want := datastoreElementSegment(steps)
	for _, el := range datastoreElements(prop) {
		if datastoreElementSegment(el.Steps) == want {
			return key, el, prop, nil
		}
	}
	return nil, datastoreElement{}, nil, fmt.Errorf("property %q holds no value at %s", name, datastoreElementLabel(name, steps))
}

// datastoreElementsSection is a property's Elements tab: every value inside an array
// or embedded entity, each opening its own page, with Edit value where the
// form can hold it, Add value on an array or embedded entity, and Remove
// value on each (#911).
//
// base is where v is inside the property: none for the property itself, and
// an element's steps on that element's page. drawn is the whole property as
// read, whose digest every row's actions carry (#923).
func datastoreElementsSection(project string, scope datastoreScope, key *datastore.Key, kind, name string, base []any, prop, drawn *datastorepb.Value) (console.Section, bool) {
	switch prop.GetValueType().(type) {
	case *datastorepb.Value_ArrayValue, *datastorepb.Value_EntityValue:
	default:
		return console.Section{}, false
	}
	list := console.Listing{Columns: []string{"Type", "Indexed", "Value"}, NameColumn: "Element", Noun: "values",
		RowsOpenable: true}
	els := datastoreElements(prop)
	for _, el := range els {
		if len(list.Items) >= datastoreElementScan {
			break
		}
		el.Steps = append(append([]any{}, base...), el.Steps...)
		v := datastoreValueGo(el.Value, project)
		at := scope.at(kind, datastoreEntityAddress(key), name, datastoreElementSegment(el.Steps))
		row := console.Resource{
			Name: datastoreElementLabel(name, el.Steps),
			Fields: map[string]string{
				"Type":    datastoreType(v),
				"Indexed": yesNo(!el.Value.GetExcludeFromIndexes()),
				"Value":   datastoreRowValue(v, el.Value.GetExcludeFromIndexes()),
			},
			Opens:   at,
			Actions: datastoreElementActions(project, el, drawn),
			ActsOn:  at,
		}
		if isDatastoreKeyStep(el.Steps) || el.Value.GetArrayValue() != nil {
			// A key has no index flag, and an array's is its values'.
			row.Fields["Indexed"] = "—"
		}
		list.Items = append(list.Items, row)
	}
	list.Total = len(list.Items)
	switch {
	case len(els) > datastoreElementScan:
		list.Note = fmt.Sprintf("The first %d of %d values.", datastoreElementScan, len(els))
	case list.Total == 0:
		list.Note = "This " + map[bool]string{true: "array", false: "embedded entity"}[prop.GetArrayValue() != nil] +
			" holds no value. Add value, on this page, adds one."
	default:
		list.Note = "Edit value writes back that one value; Add value, on an array or embedded entity and on this " +
			"page, adds one to it, and Remove value removes one. Every other value is written back as it was " +
			"read. A key in another project or database is offered no edit."
	}
	return console.Section{ID: "elements", Label: "Elements", Listing: list}, true
}

// elementDetail is one value inside an array or embedded entity property.
func (p datastoreProvider) elementDetail(ctx context.Context, project string, scope datastoreScope, kind, id, name, seg string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	steps, err := parseDatastoreElement(seg)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	key, el, drawn, err := p.lookupDatastoreElement(ctx, project, scope, kind, id, name, steps)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	v := datastoreValueGo(el.Value, project)
	summary := []console.Property{
		{Label: "Kind", Value: kind},
		{Label: "Key", Value: datastoreEntityHeading(key)},
		{Label: "Property", Value: name},
		{Label: "Element", Value: datastoreElementLabel(name, steps)},
		{Label: "Type", Value: datastoreType(v)},
	}
	if !isDatastoreKeyStep(steps) && el.Value.GetArrayValue() == nil {
		summary = append(summary, console.Property{Label: "Indexed", Value: yesNo(!el.Value.GetExcludeFromIndexes())})
	}
	d := console.Detail{
		Summary: summary,
		Sections: []console.Section{{ID: "value", Label: "Value", Kind: console.KindText,
			Text: renderDatastoreValue(v, el.Value.GetExcludeFromIndexes())}},
	}
	if _, why, ok := datastoreElementForm(v, isDatastoreKeyStep(steps), el.Value.GetExcludeFromIndexes()); !ok {
		d.Sections[0].Note = why
	} else {
		d.Sections[0].Note = "Edit value writes back this value only, in a transaction; the rest of the " +
			"property and the entity are written back as they were read."
	}
	// What is inside an array or embedded entity, each opening its own page
	// (#911).
	if sec, ok := datastoreElementsSection(project, scope, key, kind, name, steps, el.Value, drawn); ok {
		d.Sections = append(d.Sections, sec)
	}
	return d, nil
}

// datastoreElementActions are what an element's row and page offer: Edit
// value, prefilled, when the form can hold the value; Add value and Exclude
// from indexes (#924) on an array or embedded entity; and Remove value on
// every value (#911). The action route checks the same things in the
// transaction, so a value offered no edit is refused there too. Each carries
// the digest of drawn, the property as read (#923).
func datastoreElementActions(project string, el datastoreElement, drawn *datastorepb.Value) []console.Action {
	var out []console.Action
	if fields, _, ok := datastoreElementForm(datastoreValueGo(el.Value, project), isDatastoreKeyStep(el.Steps),
		el.Value.GetExcludeFromIndexes()); ok {
		out = append(out, datastoreElementAction(fields))
	}
	if isDatastoreContainer(el.Value) {
		out = append(out, datastoreAddValueAction(el.Value))
		if a, ok := datastoreExcludeAction(el.Value); ok {
			out = append(out, a)
		}
	}
	out = append(out, datastoreRemoveValueAction())
	for i := range out {
		out[i] = withDatastoreExpected(out[i], drawn)
	}
	return out
}

// elementActions is an element page's actions (datastoreElementActions).
func (p datastoreProvider) elementActions(ctx context.Context, project string, scope datastoreScope, path []string) []console.Action {
	steps, err := parseDatastoreElement(path[3])
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, el, drawn, err := p.lookupDatastoreElement(ctx, project, scope, path[0], path[1], path[2], steps)
	if err != nil {
		return nil
	}
	return datastoreElementActions(project, el, drawn)
}

// parseDatastoreElementValue reads Edit value's form: one value, as Edit
// property reads its type and value.
func parseDatastoreElementValue(values map[string]string, keyStep bool) (any, error) {
	typ := strings.TrimSpace(values["type"])
	if typ == "array" || typ == "entity" {
		return nil, errors.New("Edit value writes one value; an array or embedded entity is written as a whole with Edit property")
	}
	if keyStep && typ != "key" {
		return nil, errors.New("an embedded entity's key is a key")
	}
	if !datastoreElementType.MatchString(typ) {
		return nil, fmt.Errorf("type %q is not one of %s", typ, datastoreElementTypeList)
	}
	return parseDatastoreValue(typ, values["value"])
}

// datastoreExcludedValue reads a form's Exclude from indexes: nil when the
// form did not send it, which keeps the value's own flag.
func datastoreExcludedValue(values map[string]string) *bool {
	raw, sent := values["excluded"]
	if !sent {
		return nil
	}
	v := raw == "true"
	return &v
}

// editDatastoreElement is Edit value: the entity is read and written back in
// one transaction with only the addressed value replaced.
func (p datastoreProvider) editDatastoreElement(ctx context.Context, project string, scope datastoreScope, path []string, values map[string]string) error {
	name := path[2]
	steps, err := parseDatastoreElement(path[3])
	if err != nil {
		return err
	}
	parsed, err := parseDatastoreElementValue(values, isDatastoreKeyStep(steps))
	if err != nil {
		return err
	}
	return p.changeDrawnProperty(ctx, project, scope, path, values, func(prop *datastorepb.Value) error {
		return setDatastoreElement(prop, project, name, steps, parsed, datastoreExcludedValue(values))
	})
}

// changeProperty is changeEntity on the one property name, which must be
// there.
func (p datastoreProvider) changeProperty(ctx context.Context, project string, scope datastoreScope, kind, id, name string, change func(*datastorepb.Value) error) error {
	return p.changeEntity(ctx, project, scope, kind, id, func(props map[string]*datastorepb.Value) error {
		prop, has := props[name]
		if !has {
			return fmt.Errorf("entity %s has no property %q", datastoreEntityLabel(project, scope.ns, kind, id), name)
		}
		return change(prop)
	})
}

// setDatastoreElement replaces the value steps address inside property name
// with the parsed value, as the Go client writes it, with the index flag
// noIndex names or, when it is nil, the old value's, and the old value's
// meaning when the type is the same; nothing else inside the property
// changes.
func setDatastoreElement(prop *datastorepb.Value, project, name string, steps []any, parsed any, noIndex *bool) error {
	label := datastoreElementLabel(name, steps)
	return replaceDatastoreElement(prop, steps, label, func(old *datastorepb.Value) (*datastorepb.Value, error) {
		// Checked again in the transaction: the value may have changed
		// since the form was drawn.
		if _, why, ok := datastoreElementForm(datastoreValueGo(old, project), isDatastoreKeyStep(steps), old.GetExcludeFromIndexes()); !ok {
			return nil, fmt.Errorf("%s: %s", label, why)
		}
		flag := old.GetExcludeFromIndexes()
		if noIndex != nil && !isDatastoreKeyStep(steps) {
			flag = *noIndex
		}
		nv, err := datastoreValuePB(parsed, flag)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", label, err)
		}
		// A value of an array keeps its place (datastoreArrayOrder).
		if i, inArray := steps[len(steps)-1].(int); inArray {
			parent, err := datastoreValueAt(prop, steps[:len(steps)-1], label)
			if err != nil {
				return nil, err
			}
			values := append([]*datastorepb.Value{}, parent.GetArrayValue().GetValues()...)
			values[i] = nv
			if err := datastoreArrayOrder(values, label); err != nil {
				return nil, err
			}
		}
		if reflect.TypeOf(nv.GetValueType()) == reflect.TypeOf(old.GetValueType()) {
			nv.Meaning = old.GetMeaning()
		}
		// A key in this project, written with the project named or not, is
		// written back the same way.
		if ok, nk := old.GetKeyValue(), nv.GetKeyValue(); ok != nil && nk != nil {
			if pid := ok.GetPartitionId().GetProjectId(); pid != "" {
				if nk.PartitionId == nil {
					nk.PartitionId = &datastorepb.PartitionId{}
				}
				nk.PartitionId.ProjectId = pid
			}
		}
		return nv, nil
	})
}

// replaceDatastoreElement replaces the value steps address inside v with
// what with makes of it, leaving every other value as it is.
func replaceDatastoreElement(v *datastorepb.Value, steps []any, label string, with func(*datastorepb.Value) (*datastorepb.Value, error)) error {
	last := len(steps) == 1
	switch s := steps[0].(type) {
	case int:
		arr := v.GetArrayValue()
		if arr == nil || s >= len(arr.GetValues()) {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		if !last {
			return replaceDatastoreElement(arr.Values[s], steps[1:], label, with)
		}
		nv, err := with(arr.Values[s])
		if err != nil {
			return err
		}
		arr.Values[s] = nv
		return nil
	case string:
		e := v.GetEntityValue()
		if e == nil {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		if s == datastoreKeyStep && last {
			if len(e.GetKey().GetPath()) == 0 {
				return fmt.Errorf("the embedded entity at %s has no key", label)
			}
			nv, err := with(&datastorepb.Value{ValueType: &datastorepb.Value_KeyValue{KeyValue: e.Key}})
			if err != nil {
				return err
			}
			e.Key = nv.GetKeyValue()
			return nil
		}
		child, has := e.GetProperties()[s]
		if !has {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		if !last {
			return replaceDatastoreElement(child, steps[1:], label, with)
		}
		nv, err := with(child)
		if err != nil {
			return err
		}
		e.Properties[s] = nv
		return nil
	}
	return fmt.Errorf("the property holds no value at %s", label)
}
