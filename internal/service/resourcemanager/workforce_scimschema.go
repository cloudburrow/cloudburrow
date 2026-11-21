package resourcemanager

// The SCIM tenant endpoint's /ServiceProviderConfig and /Schemas (#2068):
// RFC 7643 §5 and §7 with Google's documented limits (no bulk, no sort,
// no ETags, no password change, eq-only filters of at most 100 results,
// a bearer token), and the User, Group and Enterprise User schemas with
// the attributes Google provisions.

import (
	"net/http"
	"strings"
)

func scimServiceProviderConfig(tc *scimTenantCtx) map[string]any {
	return map[string]any{
		"schemas":          []string{scimSPCSchema},
		"documentationUri": "https://docs.cloud.google.com/iam/docs/workforce-identity-federation-scim",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": scimMaxResults},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []any{map[string]any{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "A SCIM token of the tenant, sent as a bearer token.",
			"primary":     true,
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": tc.base + "/ServiceProviderConfig"},
	}
}

// scimAttr is an attribute definition (RFC 7643 §7).
type scimAttr struct {
	name, typ                  string
	multi, required, caseExact bool
	mutability, uniqueness     string
	returned                   string
	sub                        []scimAttr
}

func (a scimAttr) render() map[string]any {
	m := map[string]any{
		"name": a.name, "type": a.typ, "multiValued": a.multi, "description": "", "required": a.required,
		"caseExact": a.caseExact, "mutability": scimOrDefault(a.mutability, "readWrite"),
		"returned": scimOrDefault(a.returned, "default"), "uniqueness": scimOrDefault(a.uniqueness, "none"),
	}
	if a.typ == "reference" {
		m["referenceTypes"] = []string{"external"}
	}
	if len(a.sub) > 0 {
		subs := make([]any, 0, len(a.sub))
		for _, s := range a.sub {
			subs = append(subs, s.render())
		}
		m["subAttributes"] = subs
	}
	return m
}

func scimOrDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func scimStr(name string) scimAttr { return scimAttr{name: name, typ: "string"} }

func scimMulti(name string, sub ...scimAttr) scimAttr {
	return scimAttr{name: name, typ: "complex", multi: true, sub: sub}
}

var scimTypedSubs = []scimAttr{scimStr("value"), scimStr("display"), scimStr("type"), {name: "primary", typ: "boolean"}}

var scimSchemas = []struct {
	id, name, desc string
	attrs          []scimAttr
}{
	{scimUserSchema, "User", "User Account", []scimAttr{
		{name: "userName", typ: "string", required: true, uniqueness: "server"},
		{name: "name", typ: "complex", sub: []scimAttr{scimStr("formatted"), scimStr("familyName"), scimStr("givenName"), scimStr("middleName"), scimStr("honorificPrefix"), scimStr("honorificSuffix")}},
		scimStr("displayName"), scimStr("nickName"), {name: "profileUrl", typ: "reference"}, scimStr("title"), scimStr("userType"),
		scimStr("preferredLanguage"), scimStr("locale"), scimStr("timezone"), {name: "active", typ: "boolean"},
		scimMulti("emails", scimTypedSubs...), scimMulti("phoneNumbers", scimTypedSubs...), scimMulti("ims", scimTypedSubs...),
		scimMulti("photos", scimTypedSubs...),
		scimMulti("addresses", scimStr("formatted"), scimStr("streetAddress"), scimStr("locality"), scimStr("region"), scimStr("postalCode"), scimStr("country"), scimStr("type"), scimAttr{name: "primary", typ: "boolean"}),
		{name: "groups", typ: "complex", multi: true, mutability: "readOnly", sub: []scimAttr{scimStr("value"), {name: "$ref", typ: "reference"}, scimStr("display"), scimStr("type")}},
		scimMulti("entitlements", scimTypedSubs...),
		scimMulti("roles", scimStr("value"), scimStr("type"), scimAttr{name: "primary", typ: "boolean"}),
		scimMulti("x509Certificates", scimTypedSubs...),
	}},
	{scimGroupSchema, "Group", "Group", []scimAttr{
		{name: "displayName", typ: "string", required: true},
		{name: "members", typ: "complex", multi: true, sub: []scimAttr{{name: "value", typ: "string", mutability: "immutable", caseExact: true}, {name: "$ref", typ: "reference", mutability: "immutable"}, {name: "type", typ: "string", mutability: "immutable"}}},
	}},
	{scimEntSchema, "EnterpriseUser", "Enterprise User", []scimAttr{
		scimStr("employeeNumber"), scimStr("costCenter"), scimStr("organization"), scimStr("division"), scimStr("department"),
		{name: "manager", typ: "complex", sub: []scimAttr{scimStr("value"), {name: "$ref", typ: "reference"}, {name: "displayName", typ: "string", mutability: "readOnly"}}},
	}},
}

func scimSchemaView(tc *scimTenantCtx, i int) map[string]any {
	s := scimSchemas[i]
	attrs := make([]any, 0, len(s.attrs))
	for _, a := range s.attrs {
		attrs = append(attrs, a.render())
	}
	return map[string]any{"schemas": []string{scimSchemaSchema}, "id": s.id, "name": s.name, "description": s.desc, "attributes": attrs,
		"meta": map[string]any{"resourceType": "Schema", "location": tc.base + "/Schemas/" + s.id}}
}

func serveScimSchemas(w http.ResponseWriter, tc *scimTenantCtx, id string) *scimError {
	if id == "" {
		out := make([]any, 0, len(scimSchemas))
		for i := range scimSchemas {
			out = append(out, scimSchemaView(tc, i))
		}
		writeScim(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(out), "startIndex": 1,
			"itemsPerPage": len(out), "Resources": out})
		return nil
	}
	for i, s := range scimSchemas {
		if strings.EqualFold(s.id, id) {
			writeScim(w, http.StatusOK, scimSchemaView(tc, i))
			return nil
		}
	}
	return scimErr(http.StatusNotFound, "", "Schema %s not found.", id)
}
