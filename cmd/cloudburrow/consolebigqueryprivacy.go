package main

// A table's and a view's analysis rules on its page (#1580): the
// privacy_policy that CREATE or ALTER TABLE|VIEW … OPTIONS (privacy_policy =
// …) or a view's tables.insert set, read from INFORMATION_SCHEMA.TABLE_OPTIONS
// (the Go client has no field for it), shown as an Analysis rules group of
// the Details tab: its aggregation threshold, differential privacy and join
// restriction rules, each with its settings.
//
// Google's console offers no form for analysis rules: the analysis rules
// page sets and changes them only with the DDL's privacy_policy option. So
// they are read-only here, and the group says where to change them: ALTER
// VIEW (or ALTER TABLE) … SET OPTIONS (privacy_policy = …) in the query
// editor's Read-write mode, which the front then enforces on every query.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// analysisRuleLabels name each rule of a privacy_policy as the analysis
// rules page does, by its key in the DDL's JSON and the REST resource's.
var analysisRuleLabels = []struct{ snake, camel, label string }{
	{"aggregation_threshold_policy", "aggregationThresholdPolicy", "Aggregation threshold"},
	{"differential_privacy_policy", "differentialPrivacyPolicy", "Differential privacy"},
	{"join_restriction_policy", "joinRestrictionPolicy", "Join restriction"},
}

// analysisRulesGroup is the Analysis rules group of dataset.table's
// Details, or false when it has no privacy_policy.
func (p bigqueryProvider) analysisRulesGroup(ctx context.Context, c *bigquery.Client, datasetID, tableID string,
	view bool) (console.PropertyGroup, bool) {
	policy, err := readPrivacyPolicy(ctx, c, datasetID, tableID)
	if err != nil {
		return console.PropertyGroup{Heading: "Analysis rules", Properties: []console.Property{
			{Label: "Analysis rules", Value: "cannot read INFORMATION_SCHEMA.TABLE_OPTIONS: " + bigqueryRefusal(err).Error()}}}, true
	}
	if policy == "" {
		return console.PropertyGroup{}, false
	}
	props := analysisRuleProps(policy)
	kind := "TABLE"
	if view {
		kind = "VIEW"
	}
	props = append(props, console.Property{Label: "Edit analysis rules", Value: fmt.Sprintf(
		"Google's console has no form for them: run ALTER %s `%s.%s` SET OPTIONS (privacy_policy = '…') in the query "+
			"editor's Read-write mode (NULL removes them).", kind, datasetID, tableID)})
	return console.PropertyGroup{Heading: "Analysis rules", Properties: props}, true
}

// readPrivacyPolicy is the privacy_policy TABLE_OPTIONS lists of
// dataset.table, its JSON text, or "".
func readPrivacyPolicy(ctx context.Context, c *bigquery.Client, datasetID, tableID string) (string, error) {
	q := c.Query(fmt.Sprintf("SELECT option_value FROM `%s`.INFORMATION_SCHEMA.TABLE_OPTIONS "+
		"WHERE table_name = %s AND option_name = 'privacy_policy'",
		strings.ReplaceAll(datasetID, "`", ""), strconv.Quote(tableID)))
	it, err := q.Read(ctx)
	if err != nil {
		return "", err
	}
	var row []bigquery.Value
	switch err := it.Next(&row); {
	case err == iterator.Done:
		return "", nil
	case err != nil:
		return "", err
	}
	v, _ := row[0].(string)
	return unquoteOption(v), nil
}

// unquoteOption is the text of v, a TABLE_OPTIONS option_value that is a
// string literal, or v when it is none.
func unquoteOption(v string) string {
	if s, err := strconv.Unquote(v); err == nil {
		return s
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}

// analysisRuleProps are the rules of policy, a privacy_policy's JSON, one
// property each: its settings, "privacy_unit_column: last_name, threshold:
// 3". A policy that is not a JSON object is shown as it is.
func analysisRuleProps(policy string) []console.Property {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(policy), &raw) != nil {
		return []console.Property{{Label: "Privacy policy", Value: policy}}
	}
	var props []console.Property
	seen := map[string]bool{}
	for _, l := range analysisRuleLabels {
		for _, k := range []string{l.snake, l.camel} {
			if v, ok := raw[k]; ok && string(v) != "null" {
				props = append(props, console.Property{Label: l.label, Value: ruleSettings(v)})
				seen[k] = true
			}
		}
	}
	var rest []string
	for k := range raw {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		props = append(props, console.Property{Label: k, Value: string(raw[k])})
	}
	return props
}

// ruleSettings are a rule's settings, name: value, by name.
func ruleSettings(v json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(v, &m) != nil {
		return string(v)
	}
	if len(m) == 0 {
		return "on, with BigQuery's defaults"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		var s string
		switch x := m[k].(type) {
		case string:
			s = x
		case []any:
			items := make([]string, len(x))
			for j, e := range x {
				items[j] = fmt.Sprint(e)
			}
			s = strings.Join(items, ", ")
		default:
			b, _ := json.Marshal(x)
			s = string(b)
		}
		parts[i] = k + ": " + s
	}
	return strings.Join(parts, "; ")
}
