package main

// BigQuery Studio's YAML data preparations (#2105).
//
// Google kept data preparations as YAML before moving them to SQLX ("Data
// preparation files created before July 2025 are automatically migrated
// to the SQLX format", https://docs.cloud.google.com/bigquery/docs/manage-data-preparations,
// read 2026-11-22), and Dataform still runs a .dp.yaml file as a data
// preparation's contents_yaml. Google documents no schema for it; what is
// public is Dataform core's reading of it (core/actions/data_preparation.ts,
// @dataform/core 3.1.6, https://github.com/dataform-co/dataform, read
// 2026-11-22): configuration.errorTable, and nodes with an id, a source
// table or source nodeId, a destination table and generated schemas.
//
// Studio opens such a file (More > Open YAML): the editor shows its
// source, destination and error table and saves it back as YAML, every
// other key (generated schemas, anything undocumented) kept as it was, and
// the file byte for byte when none of those three changed. Migrate to SQLX
// turns it into an editable SQLX data preparation, as Google's migration
// does.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"gopkg.in/yaml.v3"

	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// yamlDataPrepParts finds a YAML data preparation's error table, first
// source table and first destination table mappings (nil when absent).
func yamlDataPrepParts(root *yaml.Node) (errTable, source, dest *yaml.Node) {
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if cfg := yamlChild(doc, "configuration"); cfg != nil {
		errTable = yamlChild(cfg, "errorTable")
	}
	if nodes := yamlChild(doc, "nodes"); nodes != nil && nodes.Kind == yaml.SequenceNode {
		for _, n := range nodes.Content {
			if source == nil {
				source = yamlChild(yamlChild(n, "source"), "table")
			}
			if dest == nil {
				dest = yamlChild(yamlChild(n, "destination"), "table")
			}
		}
	}
	return errTable, source, dest
}

// yamlChild is a mapping node's value for key.
func yamlChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlRef is a table mapping as project.dataset.table, leaving out the
// parts it does not set.
func yamlRef(m *yaml.Node) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, k := range []string{"project", "dataset", "table"} {
		if v := yamlChild(m, k); v != nil && v.Value != "" {
			parts = append(parts, v.Value)
		}
	}
	return strings.Join(parts, ".")
}

// setYAMLRef points a table mapping at ref (dataset.table or
// project.dataset.table), in place.
func setYAMLRef(project string, m *yaml.Node, ref string) error {
	parts := strings.Split(strings.Trim(strings.TrimSpace(ref), "`"), ".")
	if len(parts) < 2 {
		return fmt.Errorf("%q is not a table: write dataset.table or project.dataset.table", ref)
	}
	if _, _, _, err := studioTableRef(project, ref); err != nil {
		return err
	}
	want := map[string]string{"dataset": parts[len(parts)-2], "table": parts[len(parts)-1]}
	if len(parts) == 3 {
		want["project"] = parts[0]
	}
	var content []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i].Value
		if k == "project" && want["project"] == "" {
			continue
		}
		if v, ok := want[k]; ok {
			m.Content[i+1].Value = v
			delete(want, k)
		}
		content = append(content, m.Content[i], m.Content[i+1])
	}
	for _, k := range []string{"project", "dataset", "table"} {
		if v, ok := want[k]; ok {
			content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &yaml.Node{Kind: yaml.ScalarNode, Value: v})
		}
	}
	m.Content = content
	return nil
}

// sameRef is whether a model's table is the YAML's (the project left out
// of either meaning this one).
func sameRef(project, a, b string) bool {
	if a == b {
		return true
	}
	ap, ad, at, err1 := studioTableRef(project, a)
	bp, bd, bt, err2 := studioTableRef(project, b)
	return err1 == nil && err2 == nil && ap == bp && ad == bd && at == bt
}

// studioDataPrepFromYAML opens a YAML data preparation as the editor's
// model.
func studioDataPrepFromYAML(text string) (studioDataPrep, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return studioDataPrep{}, fmt.Errorf("not a data preparation's YAML: %v", err)
	}
	if root.Kind == 0 {
		return studioDataPrep{}, errors.New("the YAML is empty")
	}
	et, src, dst := yamlDataPrepParts(&root)
	if src == nil && dst == nil {
		return studioDataPrep{}, errors.New("not a data preparation's YAML: it has no nodes with a source or destination table")
	}
	return studioDataPrep{Source: yamlRef(src), Destination: yamlRef(dst), ErrorTable: yamlRef(et), LoadMode: "REPLACE", Steps: []studioPrepStep{}, YAML: text}, nil
}

// dataPrepYAML is a YAML data preparation's file with the model's source,
// destination and error table: the file itself when none changed.
func dataPrepYAML(project string, m studioDataPrep) (string, error) {
	if len(m.Steps) > 0 {
		return "", errors.New("a YAML data preparation's steps are not documented by Google: migrate it to SQLX to add steps")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(m.YAML), &root); err != nil {
		return "", fmt.Errorf("not a data preparation's YAML: %v", err)
	}
	et, src, dst := yamlDataPrepParts(&root)
	changed := false
	for _, p := range []struct {
		node *yaml.Node
		want string
		what string
	}{{src, m.Source, "source"}, {dst, m.Destination, "destination"}, {et, m.ErrorTable, "error table"}} {
		if sameRef(project, yamlRef(p.node), strings.TrimSpace(p.want)) {
			continue
		}
		if p.node == nil {
			return "", fmt.Errorf("the YAML has no %s table to change; migrate it to SQLX", p.what)
		}
		if err := setYAMLRef(project, p.node, p.want); err != nil {
			return "", fmt.Errorf("%s: %w", p.what, err)
		}
		changed = true
	}
	if !changed {
		return m.YAML, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return "", err
	}
	_ = enc.Close()
	return buf.String(), nil
}

// yamlDataPrepQuery is what a YAML data preparation writes to its first
// destination, its source table's rows, as a pipe-syntax query the
// editor's sample adds to.
func yamlDataPrepQuery(project string, m studioDataPrep) (string, error) {
	q, err := yamlDataPrepSelect(project, m)
	return strings.Replace(q, "SELECT * FROM ", "FROM ", 1), err
}

func yamlDataPrepSelect(project string, m studioDataPrep) (string, error) {
	text, err := dataPrepYAML(project, m)
	if err != nil {
		return "", err
	}
	y, err := dataform.ParseYAMLDataPreparation(text)
	if err != nil {
		return "", err
	}
	for _, n := range y.Nodes {
		if n.Destination != nil {
			return y.Query(n.ID, &dfpb.Target{Database: project, Schema: "dataform"})
		}
	}
	if len(y.Nodes) > 0 {
		return y.Query(y.Nodes[0].ID, &dfpb.Target{Database: project, Schema: "dataform"})
	}
	return "", errors.New("the data preparation has no nodes")
}

// yamlDataPrepFiles is the Dataform project of a YAML data preparation:
// the .dp.yaml file, declared in definitions/actions.yaml.
func yamlDataPrepFiles(project string, m studioDataPrep) (map[string]string, error) {
	text, err := dataPrepYAML(project, m)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"workflow_settings.yaml":               workflowSettingsYAML(project, studioPipelineSettings{}),
		"definitions/data_preparation.dp.yaml": text,
		"definitions/actions.yaml":             "actions:\n- dataPreparation:\n    filename: data_preparation.dp.yaml\n",
	}, nil
}
