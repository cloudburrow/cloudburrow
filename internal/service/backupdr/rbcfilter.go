package backupdr

// The filter and orderBy of resourceBackupConfigs.list and .fetch (#1536),
// as gcloud backup-dr resource-backup-config list sends them
// (googlecloudsdk/api_lib/backupdr/rbc_filter_rewrite.py): a term is
// key=value, key!=value or key:value, string values quoted and booleans
// bare; terms are joined by AND and OR (OR binding tighter, as AIP-160
// has it), and a term may be negated with NOT or a leading "-". The
// backup_configs_details keys match when any of the config's details does.

import (
	"strconv"
	"strings"
)

// rbcKeys are the filter's keys, by their snake_case and camelCase names,
// to the field of the config (or of each of its details) they compare.
var rbcKeys = map[string]string{
	"target_resource_display_name": "targetResourceDisplayName",
	"targetResourceDisplayName":    "targetResourceDisplayName",
	"target_resource_type":         "targetResourceType",
	"targetResourceType":           "targetResourceType",
	"backup_configured":            "backupConfigured",
	"backupConfigured":             "backupConfigured",
	"vaulted":                      "vaulted",
	"backup_configs_details.backup_config_source_display_name": "backupConfigsDetails.backupConfigSourceDisplayName",
	"backupConfigsDetails.backupConfigSourceDisplayName":       "backupConfigsDetails.backupConfigSourceDisplayName",
	"backup_configs_details.type":                              "backupConfigsDetails.type",
	"backupConfigsDetails.type":                                "backupConfigsDetails.type",
}

var rbcBool = map[string]bool{"backupConfigured": true, "vaulted": true}

type rbcTerm struct {
	field, op, value string
	not              bool
}

// rbcFilter is a conjunction of disjunctions of terms.
type rbcFilter [][]rbcTerm

func rbcFilterError(f, why string) error {
	return invalid("Invalid filter %q: %s. The keys are target_resource_display_name, target_resource_type, backup_configured, "+
		"vaulted, backup_configs_details.backup_config_source_display_name and backup_configs_details.type, compared with =, != or :.", f, why)
}

func parseRBCFilter(f string) (rbcFilter, error) {
	toks, err := rbcTokens(f)
	if err != nil {
		return nil, rbcFilterError(f, err.Error())
	}
	var out rbcFilter
	var or []rbcTerm
	want := true // a term is expected next
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "AND" || t == "OR":
			if want {
				return nil, rbcFilterError(f, t+" without a term before it")
			}
			if t == "AND" {
				out = append(out, or)
				or = nil
			}
			want = true
			continue
		case !want:
			// Adjacent terms are an implicit AND.
			out = append(out, or)
			or = nil
		}
		not := false
		if t == "NOT" {
			not = true
			if i++; i >= len(toks) {
				return nil, rbcFilterError(f, "NOT without a term after it")
			}
			t = toks[i]
		} else if strings.HasPrefix(t, "-") && len(t) > 1 {
			not, t = true, t[1:]
		}
		term, err := rbcParseTerm(t)
		if err != nil {
			return nil, rbcFilterError(f, err.Error())
		}
		term.not = not
		or = append(or, term)
		want = false
	}
	if want && len(toks) > 0 {
		return nil, rbcFilterError(f, "it ends without a term")
	}
	if len(or) > 0 {
		out = append(out, or)
	}
	return out, nil
}

type rbcErr string

func (e rbcErr) Error() string { return string(e) }

// rbcTokens splits a filter at spaces outside quotes; a term keeps its
// quoted value whole.
func rbcTokens(f string) ([]string, error) {
	var out []string
	var b strings.Builder
	quote := byte(0)
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(f) {
				i++
				b.WriteByte(f[i])
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			b.WriteByte(c)
		case c == ' ' || c == '\t' || c == '\n':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		case c == '(' || c == ')':
			return nil, rbcErr("parentheses are not supported")
		default:
			b.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, rbcErr("an unterminated quote")
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	// "key = value" with spaces around the operator is one term.
	var joined []string
	for i := 0; i < len(out); i++ {
		if (out[i] == "=" || out[i] == "!=" || out[i] == ":") && len(joined) > 0 && i+1 < len(out) {
			joined[len(joined)-1] += out[i] + out[i+1]
			i++
			continue
		}
		joined = append(joined, out[i])
	}
	return joined, nil
}

func rbcParseTerm(t string) (rbcTerm, error) {
	i := strings.IndexAny(t, "=!:")
	if i <= 0 {
		return rbcTerm{}, rbcErr("term " + strconv.Quote(t) + " has no key=value")
	}
	key, op, val := t[:i], t[i:i+1], t[i+1:]
	if op == "!" {
		if !strings.HasPrefix(val, "=") {
			return rbcTerm{}, rbcErr("term " + strconv.Quote(t) + " has an unknown operator")
		}
		op, val = "!=", val[1:]
	}
	field, ok := rbcKeys[key]
	if !ok {
		return rbcTerm{}, rbcErr("unknown key " + strconv.Quote(key))
	}
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		u, err := strconv.Unquote(`"` + strings.ReplaceAll(val[1:len(val)-1], `"`, `\"`) + `"`)
		if err != nil {
			return rbcTerm{}, rbcErr("value " + val + " is not a valid string")
		}
		val = u
	}
	if rbcBool[field] {
		if _, err := strconv.ParseBool(val); err != nil {
			return rbcTerm{}, rbcErr(key + " is true or false, not " + strconv.Quote(val))
		}
		val = strings.ToLower(val)
	}
	return rbcTerm{field: field, op: op, value: val}, nil
}

func (f rbcFilter) match(c map[string]any) bool {
	for _, or := range f {
		ok := false
		for _, t := range or {
			if t.match(c) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (t rbcTerm) match(c map[string]any) bool {
	var vals []string
	if sub, ok := strings.CutPrefix(t.field, "backupConfigsDetails."); ok {
		ds, _ := c["backupConfigsDetails"].([]any)
		for _, d := range ds {
			if m, ok := d.(map[string]any); ok {
				vals = append(vals, str(m, sub))
			}
		}
	} else if b, ok := c[t.field].(bool); ok {
		vals = []string{strconv.FormatBool(b)}
	} else {
		vals = []string{str(c, t.field)}
	}
	hit := false
	for _, v := range vals {
		if v == t.value {
			hit = true
			break
		}
	}
	if t.op == "!=" {
		hit = !hit
	}
	return hit != t.not
}

// parseRBCOrder is whether orderBy sorts descending; target_resource_display_name
// is the one field it sorts by.
func parseRBCOrder(o string) (bool, error) {
	f := strings.Fields(o)
	if len(f) == 0 {
		return false, nil
	}
	if (f[0] == "target_resource_display_name" || f[0] == "targetResourceDisplayName") && len(f) <= 2 {
		if len(f) == 1 || strings.EqualFold(f[1], "asc") {
			return false, nil
		}
		if strings.EqualFold(f[1], "desc") {
			return true, nil
		}
	}
	return false, invalid("Invalid orderBy %q: resource backup configs are ordered by target_resource_display_name, optionally desc.", o)
}
