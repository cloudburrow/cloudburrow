package main

// A value action carries the property it was drawn from (#923).
//
// Edit value, Add value and Remove value address a value inside an array or
// an embedded entity by its steps from the property, such as [3], and
// Exclude from indexes (#924) the array or entity it changes. Each is one
// Lookup and one Commit in a transaction (changeEntity), so a write made
// during the save aborts it, but the form or row menu was drawn earlier: had
// another writer inserted a value before [3] since, [3] would name a
// different value, and Remove value would remove it.
//
// So each of these actions carries a digest of the property as the page
// read it, in a hidden field (console.Field's "hidden" type), and the change
// is refused, inside the transaction and before anything is written, when
// the property Lookup returns there has a different digest. The whole
// property is the unit, not the addressed value alone: a value's address
// depends on every value before it in its array, and two equal values would
// have the same digest. A change to another property of the entity does not
// refuse it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// datastoreExpectedField names the hidden field a value action carries its
// property's digest in.
const datastoreExpectedField = "expected"

// datastorePropertyDigest is a digest of a stored property value: its
// deterministic wire form, hashed, so two reads of the same value agree and
// any change inside it, an index flag or meaning included, does not.
func datastorePropertyDigest(prop *datastorepb.Value) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(prop)
	if err != nil {
		// A value Lookup returned always marshals; a digest nothing can
		// match refuses the action rather than letting it through.
		return "unmarshalable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// datastoreExpected is the hidden field that carries prop's digest.
func datastoreExpected(prop *datastorepb.Value) console.Field {
	return console.Field{Name: datastoreExpectedField, Label: "Drawn from", Type: "hidden",
		Default: datastorePropertyDigest(prop)}
}

// withDatastoreExpected is an action carrying prop's digest.
func withDatastoreExpected(a console.Action, prop *datastorepb.Value) console.Action {
	a.Fields = append(append([]console.Field{}, a.Fields...), datastoreExpected(prop))
	return a
}

// checkDatastoreExpected refuses a value action whose property is not the
// one its form or row was drawn from, or that does not say.
func checkDatastoreExpected(prop *datastorepb.Value, name string, values map[string]string) error {
	want := strings.TrimSpace(values[datastoreExpectedField])
	if want == "" {
		return errors.New("the action does not say which version of the property it was drawn from; reload the page and try again")
	}
	if datastorePropertyDigest(prop) != want {
		return fmt.Errorf("property %q changed since the page was loaded, so the value this action addresses may "+
			"not be the one shown; reload the page and try again", name)
	}
	return nil
}

// changeDrawnProperty is changeProperty for a value action: the property
// must be as its page was drawn from (checkDatastoreExpected), checked in the
// same transaction that writes the change.
func (p datastoreProvider) changeDrawnProperty(ctx context.Context, project string, scope datastoreScope, path []string, values map[string]string, change func(*datastorepb.Value) error) error {
	return p.changeProperty(ctx, project, scope, path[0], path[1], path[2], func(prop *datastorepb.Value) error {
		if err := checkDatastoreExpected(prop, path[2], values); err != nil {
			return err
		}
		return change(prop)
	})
}
