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

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
)

func (s *Server) listResourceBackupConfigs(r *http.Request, project, location string) (any, error) {
	if f := param(r, "filter"); f != "" {
		return nil, unimplemented("resourceBackupConfigs.list with a filter", "every resource backup config of the location is answered; "+
			"filter on the client")
	}
	lister, ok := s.sql.(InstanceLister)
	if !ok {
		return nil, unimplemented("resourceBackupConfigs.list", inventoryWhy)
	}
	loc := "projects/" + project + "/locations/" + location
	out := []any{}
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
			d := map[string]any{"type": "BACKUPDR", "state": "ACTIVE", "backupConfigSource": a.Name,
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
	return map[string]any{"resourceBackupConfigs": out}, nil
}
