package main

// The Organization Policies page's custom constraints (#1891), as Google's
// console lays them out ("Creating and managing custom constraints",
// read 2026-11-19): Custom constraint opens Create custom constraint,
// whose fields are Display name, Constraint ID (custom.{id}), Description,
// Resource type, Enforcement method (Enforce on create, or on create and
// update), Condition (a CEL expression over the resource) and Action
// (Allow or Deny); the organization's custom constraints are listed, and
// each is edited or deleted from there. Every call is a method of the Org
// Policy v2 service orgpolicy.googleapis.com serves
// (internal/service/resourcemanager/orgpolicyv2.go), so what the page
// saves is what gcloud org-policies and the orgpolicy/apiv2 client read
// back, and what the services enforce.
//
//	custom-list    {}: the organization's custom constraints
//	custom-get     {name}
//	custom-set     {name, create, displayName, description, resourceType,
//	                methodTypes, condition, actionType}
//	custom-delete  {name}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type orgPolicyCustomView struct {
	Name         string   `json:"name"`
	ID           string   `json:"id"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	ResourceType string   `json:"resourceType"`
	MethodTypes  []string `json:"methodTypes"`
	Condition    string   `json:"condition"`
	ActionType   string   `json:"actionType"`
	UpdateTime   string   `json:"updateTime,omitempty"`
}

type orgPolicyCustomChange struct {
	orgPolicyCustomView
	Create bool `json:"create"`
}

// orgPolicyCustomTypes are the resource types the page offers, those
// CloudBurrow enforces custom constraints on.
var orgPolicyCustomTypes = []string{
	"storage.googleapis.com/Bucket",
	"iam.googleapis.com/ServiceAccount",
	"iam.googleapis.com/ServiceAccountKey",
	"pubsub.googleapis.com/Topic",
	"pubsub.googleapis.com/Subscription",
	"secretmanager.googleapis.com/Secret",
	"run.googleapis.com/Service",
	"firestore.googleapis.com/Database",
}

func customView(c *orgpolicypb.CustomConstraint) orgPolicyCustomView {
	v := orgPolicyCustomView{Name: c.GetName(), ID: c.GetName()[strings.LastIndex(c.GetName(), "/")+1:],
		DisplayName: c.GetDisplayName(), Description: c.GetDescription(), Condition: c.GetCondition(), ActionType: c.GetActionType().String()}
	if len(c.GetResourceTypes()) > 0 {
		v.ResourceType = c.GetResourceTypes()[0]
	}
	for _, m := range c.GetMethodTypes() {
		v.MethodTypes = append(v.MethodTypes, m.String())
	}
	if t := c.GetUpdateTime(); t != nil {
		v.UpdateTime = t.AsTime().UTC().Format("2006-01-02 15:04:05")
	}
	return v
}

func (p iamOrgPoliciesProvider) customCall(ctx context.Context, call string, body json.RawMessage) (any, error) {
	srv := resourcemanager.NewOrgPolicyServer(p.registry)
	var c orgPolicyCustomChange
	if len(body) > 0 {
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
	}
	name := c.Name
	if name != "" && !strings.Contains(name, "/") {
		if !strings.HasPrefix(name, "custom.") {
			name = "custom." + name
		}
		name = resourcemanager.OrganizationName + "/customConstraints/" + name
	}
	switch call {
	case "custom-list":
		all, err := p.registry.CustomConstraints(resourcemanager.OrganizationName)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"organization": resourcemanager.OrganizationName, "resourceTypes": orgPolicyCustomTypes,
			"constraints": []orgPolicyCustomView{}}
		views := []orgPolicyCustomView{}
		for _, cc := range all {
			views = append(views, customView(cc))
		}
		out["constraints"] = views
		return out, nil
	case "custom-get":
		cc, err := srv.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: name})
		if err != nil {
			return nil, err
		}
		return customView(cc), nil
	case "custom-set":
		if name == "" {
			return nil, errors.New("enter a constraint ID")
		}
		cc := &orgpolicypb.CustomConstraint{Name: name, DisplayName: c.DisplayName, Description: c.Description,
			Condition: c.Condition}
		if c.ResourceType != "" {
			cc.ResourceTypes = []string{c.ResourceType}
		}
		for _, m := range c.MethodTypes {
			v, ok := orgpolicypb.CustomConstraint_MethodType_value[m]
			if !ok {
				return nil, fmt.Errorf("no method type %q", m)
			}
			cc.MethodTypes = append(cc.MethodTypes, orgpolicypb.CustomConstraint_MethodType(v))
		}
		if v, ok := orgpolicypb.CustomConstraint_ActionType_value[c.ActionType]; ok {
			cc.ActionType = orgpolicypb.CustomConstraint_ActionType(v)
		}
		var out *orgpolicypb.CustomConstraint
		var err error
		if c.Create {
			out, err = srv.CreateCustomConstraint(ctx, &orgpolicypb.CreateCustomConstraintRequest{Parent: resourcemanager.OrganizationName, CustomConstraint: cc})
		} else {
			out, err = srv.UpdateCustomConstraint(ctx, &orgpolicypb.UpdateCustomConstraintRequest{CustomConstraint: cc})
		}
		if err != nil {
			return nil, err
		}
		return customView(out), nil
	case "custom-delete":
		if _, err := srv.DeleteCustomConstraint(ctx, &orgpolicypb.DeleteCustomConstraintRequest{Name: name}); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": name}, nil
	}
	return nil, fmt.Errorf("no such call %q", call)
}
