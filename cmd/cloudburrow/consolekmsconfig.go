package main

// Key Management's KMS Controls page (#2053), at /kms/config: the default
// Key Access Justifications policy of the project and of the folders and
// organization above it, as Google's "KMS Controls, then Key Access
// Justifications (KAJ)" shows and edits it
// (https://docs.cloud.google.com/assured-workloads/key-access-justifications/docs/set-default-policy,
// read 2026-11-20): Edit, Set Key Access Justifications policy, Allow
// specific reason codes, the Justification reasons, then Create. The page
// reads and writes through Cloud KMS's own JSON methods
// ({parent}/kajPolicyConfig and showEffectiveKeyAccessJustificationsPolicyConfig,
// #1577), the calls a REST client makes, so a refusal is the API's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
)

type kmsConfigProvider struct{ svc *kmsService }

func (kmsConfigProvider) ID() string    { return "kms-config" }
func (kmsConfigProvider) Title() string { return "KMS Controls" }

// kajReasonWords are the access reasons in Google's words
// (https://docs.cloud.google.com/assured-workloads/key-access-justifications/docs/justification-codes).
var kajReasonWords = map[string]string{
	"CUSTOMER_INITIATED_SUPPORT":                 "Customer-initiated support",
	"GOOGLE_INITIATED_SERVICE":                   "Google-initiated service",
	"THIRD_PARTY_DATA_REQUEST":                   "Third-party data request",
	"GOOGLE_INITIATED_REVIEW":                    "Google-initiated review",
	"CUSTOMER_INITIATED_ACCESS":                  "Customer-initiated access",
	"GOOGLE_INITIATED_SYSTEM_OPERATION":          "Google-initiated system operation",
	"REASON_NOT_EXPECTED":                        "Reason not expected",
	"MODIFIED_CUSTOMER_INITIATED_ACCESS":         "Modified customer-initiated access",
	"MODIFIED_GOOGLE_INITIATED_SYSTEM_OPERATION": "Modified Google-initiated system operation",
	"GOOGLE_RESPONSE_TO_PRODUCTION_ALERT":        "Google response to production alert",
	"CUSTOMER_AUTHORIZED_WORKFLOW_SERVICING":     "Customer-authorized workflow servicing",
}

// kajReasons are the reasons in enum order.
func kajReasons() []string {
	var out []string
	for v := int32(1); ; v++ {
		n, ok := kmspb.AccessReason_name[v]
		if !ok {
			break
		}
		out = append(out, n)
	}
	return out
}

const kmsConfigNote = "A default Key Access Justifications policy is given to keys created afterwards without a policy of their own; " +
	"existing keys keep theirs. A project inherits the nearest folder's or the organization's default. " +
	"Customer-initiated access and Google-initiated system operation are essential for normal functioning."

// kajConfigJSON is a KeyAccessJustificationsPolicyConfig as the API answers.
type kajConfigBody struct {
	Name   string `json:"name"`
	Policy *struct {
		Reasons []string `json:"allowedAccessReasons"`
	} `json:"defaultKeyAccessJustificationPolicy"`
}

// call sends one JSON request to Cloud KMS's own handler.
func (p kmsConfigProvider) call(ctx context.Context, method, path string, body any, out any) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(kmsNotStarted)
	}
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	kms.NewRESTHandler(api).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error.Message == "" {
			e.Error.Message = rec.Body.String()
		}
		return errors.New(e.Error.Message)
	}
	return json.Unmarshal(rec.Body.Bytes(), out)
}

// scopes are the project and its ancestors, nearest first.
func (p kmsConfigProvider) scopes(project string) []string {
	out := []string{"projects/" + project}
	if reg := p.svc.hierarchy; reg != nil {
		if anc, err := (kmsHierarchy{reg: reg}).Ancestors("projects/" + project); err == nil {
			out = append(out, anc...)
		}
	}
	return out
}

func kajPolicyWords(c kajConfigBody) string {
	if c.Policy == nil || len(c.Policy.Reasons) == 0 {
		return "No default policy"
	}
	words := make([]string, 0, len(c.Policy.Reasons))
	for _, r := range c.Policy.Reasons {
		w := kajReasonWords[r]
		if w == "" {
			w = r
		}
		words = append(words, w)
	}
	return "Allow specific reason codes: " + strings.Join(words, ", ")
}

func (p kmsConfigProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Resource", "Key Access Justifications policy"}, NameColumn: "Configuration",
		Noun: "configurations", Note: kmsConfigNote, RowsOpenable: true}
	if p.svc.API() == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	if project == "" {
		out.Prompt = "KMS Controls are a project's, a folder's and an organization's. Choose a project in the toolbar."
		return out, nil
	}
	for _, s := range p.scopes(project) {
		var c kajConfigBody
		if err := p.call(ctx, http.MethodGet, "/v1/"+s+"/kajPolicyConfig", nil, &c); err != nil {
			return console.Listing{}, err
		}
		out.Items = append(out.Items, console.Resource{Name: c.Name, Fields: map[string]string{
			"Resource": s, "Key Access Justifications policy": kajPolicyWords(c)}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// owns reports whether name is the config of project or of an ancestor.
func (p kmsConfigProvider) owns(project, name string) (string, bool) {
	for _, s := range p.scopes(project) {
		if name == s+"/kajPolicyConfig" {
			return s, true
		}
	}
	return "", false
}

func (p kmsConfigProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	scope, ok := p.owns(project, path[0])
	if !ok {
		return console.Detail{Unavailable: path[0] + " is not a configuration of project " + project + " or of its folders or organization"}, nil
	}
	var c kajConfigBody
	if err := p.call(ctx, http.MethodGet, "/v1/"+scope+"/kajPolicyConfig", nil, &c); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{
		{Label: "Resource", Value: scope},
		{Label: "Key Access Justifications policy", Value: kajPolicyWords(c)},
	}
	if strings.HasPrefix(scope, "projects/") {
		var eff struct {
			Effective kajConfigBody `json:"effectiveKajPolicy"`
		}
		if err := p.call(ctx, http.MethodGet, "/v1/"+scope+":showEffectiveKeyAccessJustificationsPolicyConfig", nil, &eff); err == nil {
			from := strings.TrimSuffix(eff.Effective.Name, "/kajPolicyConfig")
			props = append(props, console.Property{Label: "Effective policy for new keys", Value: kajPolicyWords(eff.Effective) + " (from " + from + ")"})
		}
	}
	reasons := kajReasons()
	labels := map[string]string{}
	for _, r := range reasons {
		labels[r] = kajReasonWords[r]
	}
	set := "none"
	var selected []string
	if c.Policy != nil && len(c.Policy.Reasons) > 0 {
		set = "allow"
		selected = c.Policy.Reasons
	}
	sort.Strings(selected)
	fields := []console.Field{
		{Name: "policy", Label: "Set Key Access Justifications policy", Type: "radio", Default: set, Options: []string{"none", "allow"},
			OptionLabels: map[string]string{"none": "No default policy", "allow": "Allow specific reason codes"}},
	}
	for _, r := range reasons {
		def := "false"
		for _, s := range selected {
			if s == r {
				def = "true"
			}
		}
		fields = append(fields, console.Field{Name: "reason_" + r, Label: labels[r], Type: "checkbox", Default: def,
			Section: "Justification reasons", ShowWhen: "policy=allow"})
	}
	return console.Detail{
		Title: "Key Access Justifications (KAJ)",
		Sections: []console.Section{{ID: "kaj", Label: "Key Access Justifications (KAJ)", Kind: console.KindProperties, Note: kmsConfigNote,
			Groups: []console.PropertyGroup{{Heading: "Default policy", Properties: props}}}},
		Edit: &console.EditForm{Label: "Edit", Title: "Key Access Justifications policy", Submit: "Create", Fields: fields,
			Note: "Saved with the configuration's PATCH; keys that exist keep their policy."},
	}, nil
}

// Edit sets the default policy, or clears it.
func (p kmsConfigProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	if len(path) != 1 {
		return errors.New("choose a configuration")
	}
	scope, ok := p.owns(project, path[0])
	if !ok {
		return fmt.Errorf("%s is not a configuration of project %s", path[0], project)
	}
	body := map[string]any{"name": path[0]}
	if strings.TrimSpace(values["policy"]) == "allow" {
		reasons := []string{}
		for _, r := range kajReasons() {
			if checked(values, "reason_"+r) {
				reasons = append(reasons, r)
			}
		}
		if len(reasons) == 0 {
			return errors.New("select at least one justification reason, or choose No default policy")
		}
		body["defaultKeyAccessJustificationPolicy"] = map[string]any{"allowedAccessReasons": reasons}
	}
	var out kajConfigBody
	return p.call(ctx, http.MethodPatch, "/v1/"+scope+"/kajPolicyConfig?updateMask="+url.QueryEscape("defaultKeyAccessJustificationPolicy"), body, &out)
}

var (
	_ console.Driller = kmsConfigProvider{}
	_ console.Editor  = kmsConfigProvider{}
)
