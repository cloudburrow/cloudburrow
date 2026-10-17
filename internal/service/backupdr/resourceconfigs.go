package backupdr

// resourceBackupConfigs.list (#1501): the backup configuration of each
// Cloud SQL instance of a project in a location, as Backup and DR answers
// it: a ResourceBackupConfig per instance, whose backupConfigsDetails are
// the instance's own automated backups (CLOUD_SQL_INSTANCE_BACKUP_CONFIG)
// when it has them on, and each backup plan association protecting it
// (BACKUPDR) with its vault and its rules' last successful backups.
// Cloud SQL instances are the one workload CloudBurrow's Backup and DR
// protects; Compute Engine instances and disks and Cloud Storage buckets
// are not served, so none is listed.
//
// resourceBackupConfigs.fetch (#1536) answers the same configs in the
// FetchResourceBackupConfigsResponse. Both take the filter and orderBy
// gcloud backup-dr resource-backup-config list sends (rbc_filter_rewrite):
// target_resource_display_name, target_resource_type, backup_configured,
// vaulted, backup_configs_details.backup_config_source_display_name and
// backup_configs_details.type, compared with =, != or : and joined by AND
// and OR; and orderBy target_resource_display_name, the one field gcloud
// lets you sort by.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// listResourceBackupConfigs answers resourceBackupConfigs.list, or
// resourceBackupConfigs.fetch when fetch is set: the same configs, filtered,
// ordered and paged.
func (s *Server) listResourceBackupConfigs(r *http.Request, project, location string, fetch bool) (any, error) {
	method := map[bool]string{false: "resourceBackupConfigs.list", true: "resourceBackupConfigs.fetch"}[fetch]
	lister, ok := s.sql.(InstanceLister)
	if !ok {
		return nil, unimplemented(method, inventoryWhy)
	}
	filter, err := parseRBCFilter(param(r, "filter"))
	if err != nil {
		return nil, err
	}
	desc, err := parseRBCOrder(param(r, "orderBy"))
	if err != nil {
		return nil, err
	}
	all := s.resourceBackupConfigs(lister, project, location)
	var kept []map[string]any
	for _, c := range all {
		if filter.match(c) {
			kept = append(kept, c)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := str(kept[i], "targetResourceDisplayName"), str(kept[j], "targetResourceDisplayName")
		if desc {
			return a > b
		}
		return a < b
	})
	size := 100
	if v := param(r, "pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalid("pageSize %q must be a non-negative integer.", v)
		}
		if n > 0 {
			size = min(n, 500)
		}
	}
	scope := "projects/" + project + "/locations/" + location + "/resourceBackupConfigs|" + param(r, "filter") + "|" + param(r, "orderBy")
	start := 0
	if tok := param(r, "pageToken"); tok != "" {
		after, err := paging.DecodeToken(scope, tok)
		if err == nil {
			start, err = strconv.Atoi(after)
		}
		if err != nil || start < 0 {
			return nil, invalid("pageToken %q is not a token this listing returned.", tok)
		}
	}
	out := []any{}
	for i := start; i < len(kept) && i < start+size; i++ {
		out = append(out, kept[i])
	}
	resp := map[string]any{"resourceBackupConfigs": out}
	if start+size < len(kept) {
		resp["nextPageToken"] = paging.EncodeToken(scope, strconv.Itoa(start+size))
	}
	return resp, nil
}

// resourceBackupConfigs is a config per Cloud SQL instance of the project
// in the location ("-" for every location).
func (s *Server) resourceBackupConfigs(lister InstanceLister, project, location string) []map[string]any {
	loc := "projects/" + project + "/locations/" + location
	out := []map[string]any{}
	for _, name := range lister.Instances(project) {
		info, ok := s.sql.Instance(project, name)
		if !ok || (location != "-" && info.Region != location) {
			continue
		}
		target := "//sqladmin.googleapis.com/projects/" + project + "/instances/" + name
		sum := sha256.Sum256([]byte(target))
		uid := hex.EncodeToString(sum[:8])
		details := []any{}
		vaulted := false
		if info.BackupEnabled {
			details = append(details, map[string]any{"type": "CLOUD_SQL_INSTANCE_BACKUP_CONFIG", "state": "ACTIVE",
				"backupConfigSource": target, "backupConfigSourceDisplayName": name, "applicableResource": target})
		}
		s.mu.Lock()
		var assocs []*association
		for _, a := range s.st.Associations {
			if a.Resource == "projects/"+project+"/instances/"+name || a.Resource == target {
				assocs = append(assocs, a)
			}
		}
		sort.Slice(assocs, func(i, j int) bool { return assocs[i].Name < assocs[j].Name })
		for _, a := range assocs {
			p := s.st.Plans[a.Plan]
			if p == nil {
				continue
			}
			vaulted = true
			rules := []any{}
			latest := ""
			for _, ru := range p.Rules {
				rule := map[string]any{"ruleId": ru.RuleID}
				if lb := a.LastBackup[ru.RuleID]; lb.Time != "" {
					rule["lastSuccessfulBackupTime"] = lb.Time
					if lb.Time > latest {
						latest = lb.Time
					}
				}
				rules = append(rules, rule)
			}
			d := map[string]any{"type": "BACKUPDR_BACKUP_PLAN", "state": "ACTIVE", "backupConfigSource": a.Name,
				"backupConfigSourceDisplayName": a.Name[strings.LastIndex(a.Name, "/")+1:], "applicableResource": target,
				"backupVault": p.Vault, "backupDrPlanConfig": map[string]any{"backupDrPlanRules": rules},
				"backupLocations": []any{map[string]any{"type": "REGIONAL", "locationId": location}}}
			if latest != "" {
				d["latestSuccessfulBackupTime"] = latest
			}
			details = append(details, d)
		}
		s.mu.Unlock()
		out = append(out, map[string]any{"name": loc + "/resourceBackupConfigs/" + uid, "uid": uid,
			"targetResource": target, "targetResourceDisplayName": name, "targetResourceType": "CLOUD_SQL_INSTANCE",
			"backupConfigsDetails": details, "backupConfigured": len(details) > 0, "vaulted": vaulted})
	}
	return out
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
