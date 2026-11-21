package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/siteverification"
)

// Quotas & System Limits and Domain verification (#2030): calls of the
// serviceusage Studio that read and change the Cloud Quotas and Site
// Verification APIs gcloud, Terraform and the client libraries call.
//
//   - quotas {service}: the project's quotas, of one API or of every one,
//     or, with the folder or organization chosen in the toolbar
//     (?folder=N, ?organizationId=N), that container's quotas (#2102);
//   - quota-set {service, quotaId, region, value, justification}: Edit
//     quotas' Submit request, an UpdateQuotaPreference with allow_missing;
//   - domains: the verified sites and domains;
//   - domain-token {type, identifier, method}: the token to place;
//   - domain-verify {type, identifier, method}: Verify;
//   - domain-delete {id}: Remove.

// consoleQuota is one row of Quotas & System Limits.
type consoleQuota struct {
	Service     string            `json:"service"`
	QuotaID     string            `json:"quotaId"`
	Name        string            `json:"name"`
	Metric      string            `json:"metric"`
	Type        string            `json:"type"`
	Dimensions  map[string]string `json:"dimensions"`
	Value       int64             `json:"value"`
	Default     int64             `json:"default"`
	Adjustable  bool              `json:"adjustable"`
	Enforced    bool              `json:"enforced"`
	Unit        string            `json:"unit"`
	Preference  string            `json:"preference,omitempty"`
	Granted     *int64            `json:"granted,omitempty"`
	Source      string            `json:"source"`
	Dimensional []string          `json:"dimensionKeys,omitempty"`
}

// quotaPreferenceID names a preference as Google's examples do
// (compute_googleapis_com-cpus-us-central1).
func quotaPreferenceID(service, quotaID, region string) string {
	id := strings.ReplaceAll(service, ".", "_") + "-" + strings.ToLower(quotaID)
	if region != "" {
		id += "-" + region
	}
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.TrimRight(id, "-_")
}

func (p serviceUsageProvider) quotasCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	var req struct {
		Service       string `json:"service"`
		QuotaID       string `json:"quotaId"`
		Region        string `json:"region"`
		Value         string `json:"value"`
		Justification string `json:"justification"`
		Type          string `json:"type"`
		Identifier    string `json:"identifier"`
		Method        string `json:"method"`
		ID            string `json:"id"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %v", err)
		}
	}
	if strings.HasPrefix(call, "domain") {
		return p.domainsCall(ctx, call, req.Type, req.Identifier, req.Method, req.ID)
	}
	if p.quotas == nil {
		return nil, errors.New("the Cloud Quotas API is not served by this instance")
	}
	scope := "projects/" + project
	if f := console.ScopeValue(ctx, scopeFolderParam); f != "" {
		scope = "folders/" + f
	} else if o := console.ScopeValue(ctx, scopeOrgParam); o != "" {
		scope = "organizations/" + o
	}
	parent := scope + "/locations/global"
	switch call {
	case "quotas":
		services := map[string]bool{}
		for _, q := range cloudquotas.Catalog {
			if req.Service == "" || q.Service == req.Service {
				services[q.Service] = true
			}
		}
		names := make([]string, 0, len(services))
		for s := range services {
			names = append(names, s)
		}
		sort.Strings(names)
		prefs, err := p.quotas.PreferencesOf(scope)
		if err != nil {
			return nil, err
		}
		rows := []consoleQuota{}
		for _, s := range names {
			resp, err := p.quotas.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: parent + "/services/" + s})
			if err != nil {
				return nil, err
			}
			for _, qi := range resp.GetQuotaInfos() {
				def, _ := catalogQuota(s, qi.GetQuotaId())
				for _, di := range qi.GetDimensionsInfos() {
					row := consoleQuota{Service: s, QuotaID: qi.GetQuotaId(), Name: qi.GetQuotaDisplayName(), Metric: qi.GetMetric(),
						Type: "Quota", Dimensions: di.GetDimensions(), Value: di.GetDetails().GetValue(), Default: def.Default,
						Adjustable: !qi.GetIsFixed(), Enforced: def.Enforced, Unit: qi.GetMetricUnit(), Source: def.Source,
						Dimensional: qi.GetDimensions()}
					if qi.GetIsFixed() {
						row.Type = "System limit"
					}
					for _, pr := range prefs {
						if pr.GetService() == s && pr.GetQuotaId() == qi.GetQuotaId() && sameDims(pr.GetDimensions(), di.GetDimensions()) {
							row.Preference = pr.GetName()
							if g := pr.GetQuotaConfig().GetGrantedValue(); g != nil {
								v := g.GetValue()
								row.Granted = &v
							}
						}
					}
					rows = append(rows, row)
				}
			}
		}
		return map[string]any{"project": project, "scope": scope, "quotas": rows}, nil
	case "quota-set":
		v, err := strconv.ParseInt(strings.TrimSpace(req.Value), 10, 64)
		if err != nil || v < 0 {
			return nil, errors.New("the new value must be a whole number, 0 or more")
		}
		dims := map[string]string{}
		if req.Region != "" {
			dims["region"] = req.Region
		}
		pr, err := p.quotas.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{
			AllowMissing: true,
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value", "justification"}},
			QuotaPreference: &pb.QuotaPreference{Name: parent + "/quotaPreferences/" + quotaPreferenceID(req.Service, req.QuotaID, req.Region),
				Service: req.Service, QuotaId: req.QuotaID, Dimensions: dims, Justification: req.Justification,
				QuotaConfig: &pb.QuotaConfig{PreferredValue: v, RequestOrigin: pb.QuotaConfig_CLOUD_CONSOLE}},
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": pr.GetName(), "granted": pr.GetQuotaConfig().GetGrantedValue().GetValue()}, nil
	}
	return nil, fmt.Errorf("unknown call %q", call)
}

func sameDims(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func catalogQuota(service, id string) (cloudquotas.Quota, bool) {
	for _, q := range cloudquotas.Catalog {
		if q.Service == service && q.QuotaID == id {
			return q, true
		}
	}
	return cloudquotas.Quota{}, false
}

func (p serviceUsageProvider) domainsCall(ctx context.Context, call, typ, identifier, method, id string) (any, error) {
	if p.sites == nil {
		return nil, errors.New("the Site Verification API is not served by this instance")
	}
	who := strings.ToLower(p.sites.Owner)
	site := siteverification.Site{Type: typ, Identifier: identifier}
	switch call {
	case "domains":
		all, err := p.sites.Resources()
		if err != nil {
			return nil, err
		}
		return map[string]any{"owner": who, "resources": all}, nil
	case "domain-token":
		return p.sites.GetToken(who, site, method)
	case "domain-verify":
		return p.sites.Insert(ctx, who, siteverification.Resource{Site: site}, method)
	case "domain-delete":
		return map[string]any{}, p.sites.Delete(who, id)
	}
	return nil, fmt.Errorf("unknown call %q", call)
}
