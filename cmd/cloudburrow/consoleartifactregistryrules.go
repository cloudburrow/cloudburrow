package main

// Edit rule and Edit cleanup policies on an Artifact Registry repository's
// page (#1491): the console parity audit found UpdateRule and GetRule, and
// UpdateRepository's cleanup_policies and cleanup_policy_dry_run, reachable
// only from gcloud.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// editRuleAction is Edit rule, offered when the repository has rules: the
// rule is chosen from them, and each field left empty keeps the rule's.
func editRuleAction(ctx context.Context, repoName string) (console.Action, bool) {
	rs, err := runadapter.ConfigServer().ListRules(ctx, &arpb.ListRulesRequest{Parent: repoName})
	if err != nil || len(rs.GetRules()) == 0 {
		return console.Action{}, false
	}
	var ids []string
	for _, r := range rs.GetRules() {
		ids = append(ids, r.GetName()[strings.LastIndex(r.GetName(), "/")+1:])
	}
	sort.Strings(ids)
	return console.Action{ID: "edit-rule", Label: "Edit rule", Fields: []console.Field{
		{Name: "id", Label: "Rule", Type: "select", Required: true, Options: ids},
		{Name: "action", Label: "Action", Type: "select", Options: []string{"", "DENY", "ALLOW"}, Help: "Empty keeps the rule's"},
		{Name: "package", Label: "Package", Type: "text", Help: "Empty keeps the rule's; - applies it to every image"},
		{Name: "condition", Label: "Condition", Type: "text", Help: "A CEL expression; empty keeps the rule's, - clears it"},
	}}, true
}

// editRule reads the rule (GetRule) and sends UpdateRule with the fields
// the form changed in its mask.
func editRule(ctx context.Context, repoName string, v map[string]string) error {
	cs := runadapter.ConfigServer()
	name := repoName + "/rules/" + strings.TrimSpace(v["id"])
	cur, err := cs.GetRule(ctx, &arpb.GetRuleRequest{Name: name})
	if err != nil {
		return err
	}
	in := &arpb.Rule{Name: cur.GetName()}
	var mask []string
	if a := strings.TrimSpace(v["action"]); a != "" {
		n, ok := arpb.Rule_Action_value[a]
		if !ok {
			return fmt.Errorf("unknown action %q", a)
		}
		in.Action, mask = arpb.Rule_Action(n), append(mask, "action")
	}
	if pkg := strings.TrimSpace(v["package"]); pkg != "" {
		if pkg == "-" {
			pkg = ""
		}
		in.PackageId, mask = pkg, append(mask, "package_id")
	}
	if c := strings.TrimSpace(v["condition"]); c != "" {
		if c != "-" {
			in.Condition = &expr.Expr{Expression: c}
		}
		mask = append(mask, "condition")
	}
	if len(mask) == 0 {
		return errors.New("nothing to change: set an action, a package or a condition")
	}
	_, err = cs.UpdateRule(ctx, &arpb.UpdateRuleRequest{Rule: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
	return err
}

// cleanupPoliciesJSON is a repository's cleanup policies as the form's
// JSON list, each policy as the API's JSON spells it, sorted by ID.
func cleanupPoliciesJSON(r *arpb.Repository) string {
	ids := make([]string, 0, len(r.GetCleanupPolicies()))
	for id := range r.GetCleanupPolicies() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []json.RawMessage
	for _, id := range ids {
		b, err := protojson.Marshal(r.GetCleanupPolicies()[id])
		if err == nil {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return "[]"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}

// editCleanupAction is Edit cleanup policies, prefilled.
func editCleanupAction(r *arpb.Repository) console.Action {
	dry := "false"
	if r.GetCleanupPolicyDryRun() {
		dry = "true"
	}
	return console.Action{ID: "edit-cleanup", Label: "Edit cleanup policies", Fields: []console.Field{
		{Name: "policies", Label: "Cleanup policies", Type: "textarea", Default: cleanupPoliciesJSON(r),
			Help: `A JSON list of policies, each {"id": ..., "action": "DELETE" or "KEEP", "condition": {"tagState": ..., "olderThan": "86400s", ...}} ` +
				`or {"id": ..., "action": "KEEP", "mostRecentVersions": {"keepCount": 5}}; [] removes them all. Policies are stored, not run.`},
		{Name: "dryRun", Label: "Dry run", Type: "checkbox", Default: dry, Help: "cleanup_policy_dry_run: the policies are evaluated but delete nothing"},
	}}
}

// parseCleanupPolicies reads the form's JSON list into the map
// UpdateRepository takes.
func parseCleanupPolicies(s string) (map[string]*arpb.CleanupPolicy, error) {
	out := map[string]*arpb.CleanupPolicy{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("cleanup policies: expected a JSON list: %w", err)
	}
	for i, r := range raw {
		p := &arpb.CleanupPolicy{}
		if err := protojson.Unmarshal(r, p); err != nil {
			return nil, fmt.Errorf("cleanup policy %d: %w", i+1, err)
		}
		if p.GetId() == "" {
			return nil, fmt.Errorf("cleanup policy %d has no id", i+1)
		}
		if _, dup := out[p.GetId()]; dup {
			return nil, fmt.Errorf("cleanup policy %q is named twice", p.GetId())
		}
		out[p.GetId()] = p
	}
	return out, nil
}

// editCleanup is UpdateRepository of cleanup_policies and
// cleanup_policy_dry_run.
func (p artifactRegistryProvider) editCleanup(ctx context.Context, name string, v map[string]string) error {
	pols, err := parseCleanupPolicies(v["policies"])
	if err != nil {
		return err
	}
	return p.withClient(ctx, func(c *artifactregistry.Client) error {
		_, err := c.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{
			Repository: &arpb.Repository{Name: name, CleanupPolicies: pols, CleanupPolicyDryRun: v["dryRun"] == "true" || v["dryRun"] == "on"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cleanup_policies", "cleanup_policy_dry_run"}}})
		return err
	})
}
