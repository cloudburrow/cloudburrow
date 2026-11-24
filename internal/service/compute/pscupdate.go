package compute

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
)

// forwardingRules.patch and setTarget, and globalForwardingRules.patch and
// setTarget, on Private Service Connect endpoints (#2140), as the REST
// references and vpc/docs/configure-private-service-connect-services
// (read 2026-11-22) describe them: patch takes a JSON merge patch with the
// rule's fingerprint, and of an endpoint "you can only update the global
// access field of endpoints for published services" (allowPscGlobalAccess;
// networkTier, the one field the reference names, is PREMIUM for an
// internal endpoint); "if you want to update other fields, delete the
// endpoint, and then create a new one". setTarget changes no endpoint: "the
// target is not mutable once set as a service attachment", and an endpoint
// for Google APIs keeps its bundle the same way.

// fwdOutputOnly are a forwarding rule's fields a patch may carry unchanged
// (gcloud and Terraform send the resource back as read).
var fwdOutputOnly = map[string]bool{"kind": true, "id": true, "creationTimestamp": true, "selfLink": true,
	"selfLinkWithId": true, "fingerprint": true, "labelFingerprint": true, "pscConnectionStatus": true,
	"pscConnectionId": true, "region": true, "labels": true}

func (s *Server) patchForwardingRule(project, scope, rel string, b obj) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	if fp := str(b, "fingerprint"); fp != "" && fp != str(o, "fingerprint") {
		return nil, errf(http.StatusPreconditionFailed, "conditionNotMet",
			"Supplied fingerprint does not match current fingerprint.")
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	changed := map[string]any{}
	for _, k := range keys {
		v := b[k]
		if fwdOutputOnly[k] || v == nil {
			continue
		}
		switch k {
		case "allowPscGlobalAccess":
			on, _ := v.(bool)
			if on == (o["allowPscGlobalAccess"] == true) {
				continue
			}
			if scope == "global" {
				return nil, invalid("Invalid value for field 'resource.allowPscGlobalAccess': %v. Global access applies to endpoints for published services only.", on)
			}
			changed[k] = on
		case "networkTier":
			if v != "PREMIUM" {
				return nil, invalid("Invalid value for field 'resource.networkTier': '%v'. A Private Service Connect endpoint's network tier is PREMIUM.", v)
			}
		default:
			cur := o[k]
			if sv, ok := v.(string); ok && linkKeys[k] {
				v = Rel(sv, project)
			}
			if cur != nil && reflect.DeepEqual(fmt.Sprint(v), fmt.Sprint(cur)) {
				continue
			}
			return nil, invalid("Invalid value for field 'resource.%s': only the global access field of an endpoint for a published service can be updated; to update other fields, delete the endpoint and create a new one.", k)
		}
	}
	for k, v := range changed {
		if v == false {
			delete(o, k)
		} else {
			o[k] = v
		}
	}
	o["fingerprint"] = newFingerprint()
	return s.done(project, scope, "patch", rel, o["id"])
}

func (s *Server) setForwardingRuleTarget(project, scope, rel string, b obj) (obj, error) {
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	t := str(b, "target")
	if t == "" {
		return nil, required("resource.target")
	}
	cur := str(o, "target")
	if scope != "global" {
		t = Rel(t, project)
	}
	if t != cur {
		what := "The target is not mutable once set as a service attachment"
		if scope == "global" {
			what = "The target of an endpoint for Google APIs (its bundle) is not mutable"
		}
		return nil, invalid("Invalid value for field 'resource.target': '%s'. %s; delete the endpoint and create a new one.", str(b, "target"), what)
	}
	return s.done(project, scope, "setTarget", rel, o["id"])
}
