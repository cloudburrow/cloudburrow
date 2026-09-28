package main

// Exclude from indexes on a whole array or embedded entity (#924).
//
// Datastore stores an array's values that are excluded from indexes after
// its indexed ones (measured against the emulator in #911), so Add value and
// Edit value refuse an excluded value before an indexed one rather than let
// it move. That left no way to exclude a whole array one value at a time
// when Edit property's JSON cannot hold it (#894): the first value's flag
// could not change until every value after it was excluded, and those could
// not be excluded before it without moving. An array holding a key,
// timestamp or geopoint, or a string or blob over 1,500 bytes that must be
// excluded, could not be put first.
//
// Exclude from indexes, on an array or embedded entity property's page and
// on every array or embedded entity inside one, sets or clears the flag on
// every value inside it, at any depth, in one write through changeEntity:
// every value and its order are kept, and only the flags change. An
// embedded entity's own flag is set too; an array has none (Datastore
// refuses one on an array value; its values carry it) and an embedded
// entity's key has none. Every value then has the same flag, so no array
// inside holds an excluded value before an indexed one. An embedded entity
// that is itself a value of an array changes its own flag among its
// neighbours', so that array is checked as Add value checks it
// (datastoreArrayOrder). Clearing the flag is refused while a string or blob
// inside is over 1,500 bytes, which Datastore does not index.

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"cloud.google.com/go/datastore/apiv1/datastorepb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// datastoreIndexLimit is the most bytes of a string or blob Datastore
// indexes.
const datastoreIndexLimit = 1500

// datastoreFlagged is every value inside v that has an index flag of its
// own, v included when it is not an array, as steps from v: an array's
// values, an embedded entity and its properties' values, at any depth, in
// the order the Elements tab lists them. An embedded entity's key has none.
func datastoreFlagged(v *datastorepb.Value) []datastoreElement {
	var out []datastoreElement
	var walk func(v *datastorepb.Value, steps []any)
	walk = func(v *datastorepb.Value, steps []any) {
		at := func(s any) []any { return append(append([]any{}, steps...), s) }
		switch t := v.GetValueType().(type) {
		case *datastorepb.Value_ArrayValue:
			for i, e := range t.ArrayValue.GetValues() {
				walk(e, at(i))
			}
			return
		case *datastorepb.Value_EntityValue:
			out = append(out, datastoreElement{Steps: steps, Value: v})
			props := t.EntityValue.GetProperties()
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				walk(props[n], at(n))
			}
			return
		}
		out = append(out, datastoreElement{Steps: steps, Value: v})
	}
	walk(v, nil)
	return out
}

// datastoreAllExcluded is whether every value inside v with an index flag
// is excluded, and whether it has any.
func datastoreAllExcluded(v *datastorepb.Value) (excluded, has bool) {
	flagged := datastoreFlagged(v)
	for _, el := range flagged {
		if !el.Value.GetExcludeFromIndexes() {
			return false, true
		}
	}
	return true, len(flagged) > 0
}

// datastoreExcludeAction is Exclude from indexes on an array or embedded
// entity, prefilled checked when every value inside it is excluded; ok is
// false for one holding no value with an index flag, an empty array.
func datastoreExcludeAction(target *datastorepb.Value) (console.Action, bool) {
	excluded, has := datastoreAllExcluded(target)
	if !isDatastoreContainer(target) || !has {
		return console.Action{}, false
	}
	what := "array"
	if target.GetEntityValue() != nil {
		what = "embedded entity, itself included"
	}
	return console.Action{ID: "excludevalues", Label: "Exclude from indexes", Fields: []console.Field{
		{Name: "excluded", Label: "Exclude every value from indexes", Type: "checkbox",
			Default: strconv.FormatBool(excluded),
			Help: "Checked, every value inside this " + what + ", at any depth, is excluded from indexes; " +
				"unchecked, every one is indexed. Every value and its order are kept. An excluded value cannot " +
				"be filtered or ordered on, and a string or blob over 1,500 bytes cannot be indexed."},
	}}, true
}

// setDatastoreExcluded sets or clears the index flag on every value inside
// the array or embedded entity steps address inside property name, keeping
// every value and its order. Clearing it is refused while a string or blob
// inside is over 1,500 bytes; an embedded entity in an array that would then
// be excluded before an indexed neighbour is refused as Datastore would move
// it. A refused change leaves prop as it was.
func setDatastoreExcluded(prop *datastorepb.Value, name string, steps []any, excluded bool) error {
	label := datastoreElementLabel(name, steps)
	target, err := datastoreValueAt(prop, steps, label)
	if err != nil {
		return err
	}
	if !isDatastoreContainer(target) {
		return fmt.Errorf("%s is not an array or an embedded entity; a value's own flag is changed with Edit value", label)
	}
	flagged := datastoreFlagged(target)
	if !excluded {
		for _, el := range flagged {
			if n := len(el.Value.GetStringValue()) + len(el.Value.GetBlobValue()); n > datastoreIndexLimit {
				return fmt.Errorf("%s is %d bytes, and Datastore indexes a string or blob of at most 1,500 bytes; "+
					"it must stay excluded, so %s cannot be indexed as a whole",
					datastoreElementLabel(name, append(append([]any{}, steps...), el.Steps...)), n, label)
			}
		}
	}
	if i, inArray := lastIndex(steps); inArray && target.GetEntityValue() != nil {
		parent, err := datastoreValueAt(prop, steps[:len(steps)-1], label)
		if err != nil {
			return err
		}
		values := append([]*datastorepb.Value{}, parent.GetArrayValue().GetValues()...)
		moved := &datastorepb.Value{ExcludeFromIndexes: excluded}
		values[i] = moved
		if err := datastoreArrayOrder(values, label); err != nil {
			return err
		}
	}
	for _, el := range flagged {
		el.Value.ExcludeFromIndexes = excluded
	}
	return nil
}

// lastIndex is steps' last step when it is an array index.
func lastIndex(steps []any) (int, bool) {
	if len(steps) == 0 {
		return 0, false
	}
	i, ok := steps[len(steps)-1].(int)
	return i, ok
}

// excludeDatastoreValues is Exclude from indexes on a property's page (path
// of three) or an element's (four).
func (p datastoreProvider) excludeDatastoreValues(ctx context.Context, project string, scope datastoreScope, path []string, values map[string]string) error {
	var steps []any
	if len(path) == 4 {
		s, err := parseDatastoreElement(path[3])
		if err != nil {
			return err
		}
		steps = s
	}
	excluded := values["excluded"] == "true"
	return p.changeDrawnProperty(ctx, project, scope, path, values, func(prop *datastorepb.Value) error {
		return setDatastoreExcluded(prop, path[2], steps, excluded)
	})
}
