package sqladmin

// tiers.list and flags.list (#1380) answer the lists Google documents, for
// the database versions CloudBurrow runs (flagsdata.go).

import (
	"net/http"
	"strconv"
	"strings"
)

func listFlags(version string) (any, error) {
	if version != "" && !SupportedVersions[version] {
		if strings.HasPrefix(version, "POSTGRES_") || strings.HasPrefix(version, "MYSQL_") || strings.HasPrefix(version, "SQLSERVER_") {
			return map[string]any{"kind": "sql#flagsList", "items": []any{}}, nil
		}
		return nil, Errorf(http.StatusBadRequest, "Invalid request: databaseVersion %q is not a Cloud SQL database version.", version)
	}
	items := []any{}
	for _, f := range documentedFlags {
		var applies []string
		for _, a := range f.AppliesTo {
			if version == "" || a == version {
				applies = append(applies, a)
			}
		}
		if len(applies) == 0 {
			continue
		}
		m := map[string]any{"kind": "sql#flag", "name": f.Name, "flagScope": "SQL_FLAG_SCOPE_DATABASE", "type": f.Type, "appliesTo": applies,
			"requiresRestart": f.RequiresRestart}
		if len(f.AllowedStringValues) > 0 {
			m["allowedStringValues"] = f.AllowedStringValues
		}
		if f.MinValue != nil {
			m["minValue"] = strconv.FormatInt(*f.MinValue, 10)
		}
		if f.MaxValue != nil {
			m["maxValue"] = strconv.FormatInt(*f.MaxValue, 10)
		}
		if f.InBeta {
			m["inBeta"] = true
		}
		items = append(items, m)
	}
	return map[string]any{"kind": "sql#flagsList", "items": items}, nil
}

func listTiers() any {
	items := []any{}
	for _, t := range documentedTiers {
		region := t.Region
		if len(region) == 0 {
			region = []string{Region}
		}
		items = append(items, map[string]any{"kind": "sql#tier", "tier": t.Tier, "RAM": strconv.FormatInt(t.RAM, 10),
			"DiskQuota": strconv.FormatInt(t.DiskQuota, 10), "region": region})
	}
	return map[string]any{"kind": "sql#tiersList", "items": items}
}
