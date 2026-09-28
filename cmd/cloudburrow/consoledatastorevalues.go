package main

// Adding and removing values inside an array or an embedded entity (#911).
//
// Edit value (#905) replaces one value but cannot change the shape of what
// holds it, and Edit property rewrites the whole property only when its JSON
// can hold every value inside it (#894), so an array holding a key, a
// timestamp or a geopoint could not grow or shrink from the console at all.
// Google's Datastore console adds and removes an array's and an embedded
// entity's values in its entity editor.
//
// Add value is offered on an array or embedded entity property's page and on
// every array or embedded entity inside one (its row on the Elements tab, and
// its own page). Into an array it inserts at an index, or appends; into an
// embedded entity it adds a property the entity does not have, or its key,
// named __key__, when it has none. Remove value, on every value inside a
// property, removes that value: from its array, which closes up; from its
// embedded entity; or an embedded entity's key. Both are one Lookup and one
// Commit in a transaction (changeEntity), so every other value goes back
// exactly as Lookup returned it, index flags and meanings included, and the
// emulator's own refusals, such as a reserved or empty property name, are
// what the form shows. An array inside an array and an indexed value over
// 1,500 bytes are refused before the write, as Datastore would refuse them;
// so is an excluded value before an indexed one in the same array, which
// Datastore would move (datastoreArrayOrder).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// datastoreAddValueAction is Add value on an array or an embedded entity,
// target: an index or a property name, then a value typed as Edit property's
// is, and its index flag, prefilled as its neighbours'.
func datastoreAddValueAction(target *datastorepb.Value) console.Action {
	var where console.Field
	noIndex := target.GetExcludeFromIndexes()
	if arr := target.GetArrayValue(); arr != nil {
		n := len(arr.GetValues())
		where = console.Field{Name: "index", Label: "Index", Type: "text", Pattern: `^\s*[0-9]*\s*$`,
			Help: fmt.Sprintf("Where the value goes: 0 puts it first and %d, or empty, after the last. "+
				"The values from that index on move up one.", n)}
		// An array has no index flag of its own: its values carry it. The
		// last value's is what an appended value can have without moving
		// (datastoreArrayOrder).
		noIndex = n > 0 && arr.GetValues()[n-1].GetExcludeFromIndexes()
	} else {
		where = console.Field{Name: "field", Label: "Property", Type: "text", Required: true,
			Help: "A property this embedded entity does not have yet. " + datastoreKeyStep +
				" gives it a key, when it has none, of type key."}
	}
	fields := []console.Field{where,
		{Name: "type", Label: "Type", Type: "text", Required: true, Default: "string", Pattern: datastoreTypePattern,
			Help: "One of " + datastoreTypeList + ". An array cannot hold an array."},
		{Name: "value", Label: "Value", Type: "textarea", Help: datastoreValueHelp},
		{Name: "excluded", Label: "Exclude from indexes", Type: "checkbox", Default: strconv.FormatBool(noIndex),
			Help: datastoreExcludedHelp + " An embedded entity's key has no index flag."},
	}
	return console.Action{ID: "addvalue", Label: "Add value", Fields: fields}
}

// datastoreRemoveValueAction is Remove value, confirmed as a delete is. It
// leaves an element's page, whose value is gone.
func datastoreRemoveValueAction() console.Action {
	return console.Action{ID: "removevalue", Label: "Remove value", Destructive: true, Leaves: true}
}

// propertyActions is a property page's actions: Add value on an array or
// embedded entity property (#911), and Delete property.
func (p datastoreProvider) propertyActions(ctx context.Context, project string, scope datastoreScope, path []string) []console.Action {
	del := console.Action{ID: "deleteproperty", Label: "Delete property", Destructive: true, Leaves: true}
	key, err := datastoreEntityKey(project, scope.ns, path[0], path[1])
	if err != nil {
		return []console.Action{del}
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, done, err := p.rawDatastore()
	if err != nil {
		return []console.Action{del}
	}
	defer done()
	e, err := lookupEntity(ctx, c, project, key, nil)
	if prop := e.GetProperties()[path[2]]; err == nil && isDatastoreContainer(prop) {
		return []console.Action{datastoreAddValueAction(prop), del}
	}
	return []console.Action{del}
}

// datastoreValueAt is the value steps address inside v, which is v itself
// for no steps. An embedded entity's key holds nothing, so it is never a step
// on the way.
func datastoreValueAt(v *datastorepb.Value, steps []any, label string) (*datastorepb.Value, error) {
	for _, step := range steps {
		switch s := step.(type) {
		case int:
			arr := v.GetArrayValue()
			if arr == nil || s >= len(arr.GetValues()) {
				return nil, fmt.Errorf("the property holds no value at %s", label)
			}
			v = arr.GetValues()[s]
		case string:
			child, has := v.GetEntityValue().GetProperties()[s]
			if !has {
				return nil, fmt.Errorf("the property holds no value at %s", label)
			}
			v = child
		default:
			return nil, fmt.Errorf("the property holds no value at %s", label)
		}
	}
	return v, nil
}

// addDatastoreElement adds a value to the array or embedded entity steps
// address inside property name, as Add value's form names it. Nothing else
// inside the property changes.
func addDatastoreElement(prop *datastorepb.Value, name string, steps []any, values map[string]string) error {
	label := datastoreElementLabel(name, steps)
	target, err := datastoreValueAt(prop, steps, label)
	if err != nil {
		return err
	}
	if !isDatastoreContainer(target) {
		return fmt.Errorf("%s is not an array or an embedded entity; a value is added to one", label)
	}
	typ := strings.TrimSpace(values["type"])
	parsed, err := parseDatastoreValue(typ, values["value"])
	if err != nil {
		return err
	}
	noIndex := values["excluded"] == "true"
	if arr := target.GetArrayValue(); arr != nil {
		at := len(arr.GetValues())
		if raw := strings.TrimSpace(values["index"]); raw != "" {
			i, err := strconv.Atoi(raw)
			if err != nil || i < 0 || i > at {
				return fmt.Errorf("index %q is not one of 0 to %d: %s holds %d values", raw, at, label, at)
			}
			at = i
		}
		if _, isArray := parsed.([]any); isArray {
			// Datastore's rule, refused here so the form says it plainly.
			return errors.New("an array cannot hold an array; Datastore refuses one")
		}
		nv, err := datastoreValuePB(parsed, noIndex)
		if err != nil {
			return fmt.Errorf("%s[%d]: %v", label, at, err)
		}
		values := append(append(append([]*datastorepb.Value{}, arr.GetValues()[:at]...), nv), arr.GetValues()[at:]...)
		if err := datastoreArrayOrder(values, datastoreElementLabel(name, append(append([]any{}, steps...), at))); err != nil {
			return err
		}
		arr.Values = values
		return nil
	}
	e := target.GetEntityValue()
	field := values["field"]
	if strings.TrimSpace(field) == "" {
		return errors.New("a property name is required")
	}
	if field == datastoreKeyStep {
		k, isKey := parsed.(*datastore.Key)
		if !isKey || k == nil {
			return errors.New("an embedded entity's key is a key")
		}
		if len(e.GetKey().GetPath()) > 0 {
			return fmt.Errorf("the embedded entity %s already has a key; change it with Edit value", label)
		}
		e.Key = datastoreKeyPB(k)
		return nil
	}
	if _, has := e.GetProperties()[field]; has {
		return fmt.Errorf("the embedded entity %s already has a property %q; change it with Edit value", label, field)
	}
	nv, err := datastoreValuePB(parsed, noIndex)
	if err != nil {
		return fmt.Errorf("%s: %v", datastoreElementLabel(name, append(append([]any{}, steps...), field)), err)
	}
	if e.Properties == nil {
		e.Properties = map[string]*datastorepb.Value{}
	}
	e.Properties[field] = nv
	return nil
}

// datastoreArrayOrder refuses an array whose values would not read back in
// the order written: Datastore stores an array's values that are excluded
// from indexes after its indexed ones, keeping each group's order (measured
// against the emulator, top level and inside an embedded entity), so an
// excluded value before an indexed one would move to the end. label names
// the value the change writes.
func datastoreArrayOrder(values []*datastorepb.Value, label string) error {
	excluded := false
	for _, v := range values {
		if v.GetExcludeFromIndexes() {
			excluded = true
		} else if excluded {
			return fmt.Errorf("%s: Datastore stores an array's values excluded from indexes after its indexed ones, "+
				"so this change would reorder the array; an excluded value goes after every indexed value of its array", label)
		}
	}
	return nil
}

// removeDatastoreElement removes the value steps address inside property
// name: from its array, whose later values move down one; from its embedded
// entity; or an embedded entity's key. Nothing else inside the property
// changes.
func removeDatastoreElement(prop *datastorepb.Value, name string, steps []any) error {
	label := datastoreElementLabel(name, steps)
	if len(steps) == 0 {
		return errors.New("a property is removed with Delete property")
	}
	parent, err := datastoreValueAt(prop, steps[:len(steps)-1], label)
	if err != nil {
		return err
	}
	switch s := steps[len(steps)-1].(type) {
	case int:
		arr := parent.GetArrayValue()
		if arr == nil || s >= len(arr.GetValues()) {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		arr.Values = append(arr.Values[:s], arr.Values[s+1:]...)
		return nil
	case string:
		e := parent.GetEntityValue()
		if e == nil {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		if s == datastoreKeyStep {
			if len(e.GetKey().GetPath()) == 0 {
				return fmt.Errorf("the embedded entity at %s has no key", label)
			}
			e.Key = nil
			return nil
		}
		if _, has := e.GetProperties()[s]; !has {
			return fmt.Errorf("the property holds no value at %s", label)
		}
		delete(e.Properties, s)
		return nil
	}
	return fmt.Errorf("the property holds no value at %s", label)
}

// addDatastoreValue is Add value on a property's page (path of three) or an
// element's (four).
func (p datastoreProvider) addDatastoreValue(ctx context.Context, project string, scope datastoreScope, path []string, values map[string]string) error {
	var steps []any
	if len(path) == 4 {
		s, err := parseDatastoreElement(path[3])
		if err != nil {
			return err
		}
		steps = s
	}
	return p.changeProperty(ctx, project, scope, path[0], path[1], path[2], func(prop *datastorepb.Value) error {
		return addDatastoreElement(prop, path[2], steps, values)
	})
}

// removeDatastoreValue is Remove value on an element.
func (p datastoreProvider) removeDatastoreValue(ctx context.Context, project string, scope datastoreScope, path []string) error {
	steps, err := parseDatastoreElement(path[3])
	if err != nil {
		return err
	}
	return p.changeProperty(ctx, project, scope, path[0], path[1], path[2], func(prop *datastorepb.Value) error {
		return removeDatastoreElement(prop, path[2], steps)
	})
}
