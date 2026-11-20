package main

// The Spanner Instances list's info panel and the instance page's Upgrade
// link, as Google's console has them (#1859, from
// https://docs.cloud.google.com/spanner/docs/create-manage-instances, read
// 2026-11-20):
//
//   - "Select the checkbox for the instance. The Info panel appears", and
//     its Labels tab is where the instance's labels are added, deleted or
//     updated;
//   - "Click Upgrade next to the edition type", then the higher edition
//     under Update edition, and Save. A downgrade is Edit instance's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const (
	spannerLabelsActionID  = "editlabels"
	spannerUpgradeActionID = "upgradeedition"
)

// spannerEditionOrder is the editions from the lowest tier up.
var spannerEditionOrder = []string{"Standard", "Enterprise", "Enterprise Plus"}

// spannerLabelsAction is the info panel's Labels tab form, prefilled with
// the instance's labels and saved with Save. Linked: it is not a button on
// the instance's page, whose Edit instance is Google's.
func spannerLabelsAction(labels map[string]string) console.Action {
	def := ""
	if len(labels) > 0 {
		b, _ := json.Marshal(labels)
		def = string(b)
	}
	return console.Action{ID: spannerLabelsActionID, Label: "Labels", Submit: "Save", Linked: true, Fields: []console.Field{{
		Name: "labels", Label: "Labels", Type: "map", Default: def,
		KeyPattern: pubsubfront.LabelKeyPattern, ValuePattern: pubsubfront.LabelValuePattern,
		KeyHelp:    "A key is 1 to 63 lowercase letters, international characters, digits, _ and -, starting with a lowercase letter or an international character.",
		ValueHelp:  "A value is 0 to 63 lowercase letters, international characters, digits, _ and -.",
		MaxEntries: pubsubfront.MaxLabels,
	}}}
}

// spannerLabelsTab is an Instances row's Labels tab in the info panel.
func spannerLabelsTab(labels map[string]string) console.InfoTab {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	props := make([]console.Property, 0, len(keys))
	for _, k := range keys {
		props = append(props, console.Property{Label: k, Value: labels[k]})
	}
	form := spannerLabelsAction(labels)
	return console.InfoTab{ID: "labels", Label: "Labels", Properties: props, Empty: "This instance has no labels.", Form: &form}
}

// spannerHigherEditions are the editions above e, in order.
func spannerHigherEditions(e instancepb.Instance_Edition) []string {
	name := spannerEdition(e)
	if name == "—" {
		name = "Standard"
	}
	for i, n := range spannerEditionOrder {
		if n == name {
			return spannerEditionOrder[i+1:]
		}
	}
	return nil
}

// spannerUpgradeAction is Update edition, opened by Upgrade beside the
// instance's edition; none on Enterprise Plus, which has no higher edition.
func spannerUpgradeAction(e instancepb.Instance_Edition) (console.Action, bool) {
	higher := spannerHigherEditions(e)
	if len(higher) == 0 {
		return console.Action{}, false
	}
	return console.Action{ID: spannerUpgradeActionID, Label: "Update edition", Submit: "Save", Linked: true, Fields: []console.Field{{
		Name: "edition", Label: "Update edition", Type: "radio", Required: true, Options: higher, Default: higher[0],
		Help: "An upgrade takes effect at once and keeps every database. A downgrade is Edit instance's.",
	}}}, true
}

// spannerInstanceLinkedActions are the instance page's linked actions: the
// info panel's Labels and, below Enterprise Plus, Update edition.
func (p spannerProvider) spannerInstanceLinkedActions(ctx context.Context, project, instanceID string) []console.Action {
	inst, err := p.spannerGetInstance(ctx, project, instanceID)
	if err != nil {
		return nil
	}
	acts := []console.Action{spannerLabelsAction(inst.GetLabels())}
	if up, ok := spannerUpgradeAction(inst.GetEdition()); ok {
		acts = append(acts, up)
	}
	return acts
}

func (p spannerProvider) spannerGetInstance(ctx context.Context, project, instanceID string) (*instancepb.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	return instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: fmt.Sprintf("projects/%s/instances/%s", project, instanceID)})
}

// spannerInfoAct performs the info panel's Labels and Update edition.
func (p spannerProvider) spannerInfoAct(ctx context.Context, project string, path []string, action string,
	values map[string]string) (handled bool, err error) {
	if action != spannerLabelsActionID && action != spannerUpgradeActionID {
		return false, nil
	}
	if len(path) != 1 {
		return true, fmt.Errorf("%s is an instance's action", action)
	}
	in := &instancepb.Instance{Name: fmt.Sprintf("projects/%s/instances/%s", project, path[0])}
	var paths []string
	switch action {
	case spannerLabelsActionID:
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return true, fmt.Errorf("labels: %w", err)
		}
		in.Labels, paths = labels, []string{"labels"}
	case spannerUpgradeActionID:
		cur, err := p.spannerGetInstance(ctx, project, path[0])
		if err != nil {
			return true, err
		}
		want := strings.TrimSpace(values["edition"])
		ok := false
		for _, h := range spannerHigherEditions(cur.GetEdition()) {
			ok = ok || h == want
		}
		if !ok {
			return true, errors.New("Update edition upgrades to a higher edition: " + want + " is not above " +
				spannerEdition(cur.GetEdition()) + "; a downgrade is Edit instance's")
		}
		in.Edition, paths = spannerEditions[want], []string{"edition"}
	}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return true, fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	op, err := instAdmin.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: in, FieldMask: &fieldmaskpb.FieldMask{Paths: paths}})
	if err != nil {
		return true, err
	}
	_, err = op.Wait(ctx)
	return true, err
}

// spannerEditionProperty is the instance page's Edition, with Upgrade
// beside it below Enterprise Plus.
func spannerEditionProperty(e instancepb.Instance_Edition) console.Property {
	prop := console.Property{Label: "Edition", Value: spannerEdition(e)}
	if _, ok := spannerUpgradeAction(e); ok {
		prop.Action, prop.ActionText = spannerUpgradeActionID, "Upgrade"
	}
	return prop
}
