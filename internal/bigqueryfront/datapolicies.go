package bigqueryfront

import (
	"regexp"
	"strings"
)

// A column's data policies (#1456): ALTER TABLE … ALTER COLUMN … SET
// OPTIONS (data_policies = [...]) and data_policies += [...].
//
// The DDL reference: data_policies, ARRAY<STRING>, "Applies a data policy
// to a column", each "{'name':'myproject.region-us.data_policy_name1'}",
// and "The ALTER TABLE ALTER COLUMN statement supports the = and +=
// operators for this option"; NULL clears it ("Setting the VALUE to NULL
// clears the column's value for that option"). The table resource keeps
// them as the field's dataPolicies, each {"name": "projects/P/locations/
// L/dataPolicies/ID"} (TableFieldSchema, DataPolicyOption), which the
// emulator's schema keeps (its Table is the API's), so the front writes
// them there through the tables.patch path and tables.get reads them back.
//
// A data policy is a resource of the BigQuery Data Policy API, which
// CloudBurrow does not serve: a policy is not looked up, and no masking
// or access rule of one is applied to the column's reads.

// dataPolicyName reads one entry, {'name':'project.region-r.id'}.
var dataPolicyName = regexp.MustCompile(`^\{\s*['"]name['"]\s*:\s*['"]([^'"]+)['"]\s*\}$`)

// dataPolicyNames reads val, an ARRAY<STRING> literal or NULL, as the REST
// names of its policies; msg is why it cannot be read (400).
func dataPolicyNames(val []token) (names []string, msg string) {
	if len(val) == 1 && val[0].is("NULL") {
		return nil, ""
	}
	bad := "The option data_policies must be an array of string literals such as " +
		`"{'name':'myproject.region-us.data_policy_name1'}"`
	if len(val) > 0 && val[0].is("ARRAY") {
		val = val[1:]
	}
	if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
		return nil, bad
	}
	for i := 1; i < len(val)-1; i++ {
		if i%2 == 0 {
			if !val[i].punct(",") {
				return nil, bad
			}
			continue
		}
		s, ok := stringLiteral(val[i])
		if !ok {
			return nil, bad
		}
		m := dataPolicyName.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			return nil, bad
		}
		parts := strings.Split(m[1], ".")
		if len(parts) != 3 || !strings.HasPrefix(strings.ToLower(parts[1]), "region-") || parts[0] == "" || parts[2] == "" ||
			len(parts[1]) == len("region-") {
			return nil, "The data policy name " + m[1] + " must be project.region-REGION.data_policy_id"
		}
		names = append(names, "projects/"+parts[0]+"/locations/"+strings.ToLower(parts[1][len("region-"):])+
			"/dataPolicies/"+parts[2])
	}
	return names, ""
}

// setDataPolicies sets o, a data_policies option parseAlter checked, on f,
// a schema field: = replaces the field's policies, += adds those it lacks.
func setDataPolicies(f map[string]any, o alterOption) {
	names, _ := dataPolicyNames(o.val)
	var out []any
	have := map[string]bool{}
	if o.add {
		cur, _ := f["dataPolicies"].([]any)
		for _, p := range cur {
			if m, ok := p.(map[string]any); ok {
				have[str(m, "name")] = true
				out = append(out, p)
			}
		}
	}
	for _, n := range names {
		if !have[n] {
			have[n] = true
			out = append(out, map[string]any{"name": n})
		}
	}
	if len(out) == 0 {
		delete(f, "dataPolicies")
		return
	}
	f["dataPolicies"] = out
}
