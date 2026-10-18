package sqladmin

// Every settable field of the request bodies the Admin API serves (#1538)
// is applied, recorded, ignored as Cloud SQL ignores it (output only), or
// refused with 400 or 501 naming it: never accepted and silently dropped.
// TestSQLFieldsNeverSilentlyDropped checks the tables below against the
// discovery document's schemas, so a field added upstream cannot go
// unclassified, and docs/cloudsql.md has a row for each recorded one.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// How a field is treated.
const (
	// fieldKept is applied: it changes what the server does or answers.
	fieldKept = "kept"
	// fieldStored is recorded and answered as sent, with no behaviour
	// behind it: it describes the machine or the management Cloud SQL
	// would give the instance (docs/cloudsql.md has a row for each).
	fieldStored = "stored"
	// fieldOutput is output only, or set by the server: ignored in a
	// request, as Cloud SQL ignores it.
	fieldOutput = "output"
	// fieldRefused is refused, with the field's name, when it is set to
	// anything but its default.
	fieldRefused = "refused"
)

// fieldRule is how one field of a request body is treated. refuse, for a
// refused field (or a kept one some values of which are refused), says why
// a value is not taken: nil for one that is.
type fieldRule struct {
	how    string
	refuse func(field string, v any) error
}

var (
	kept   = fieldRule{how: fieldKept}
	stored = fieldRule{how: fieldStored}
	output = fieldRule{how: fieldOutput}
)

// notSet reports whether v says nothing: null, false, zero, "", or an
// object or list of nothing but such values (a client's default).
func notSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case float64:
		return x == 0
	case json.Number:
		return x == "0" || x == ""
	case []any:
		for _, e := range x {
			if !notSet(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for k, e := range x {
			if k != "kind" && !notSet(e) {
				return false
			}
		}
		return true
	}
	return false
}

// refused501 refuses any value but the default with 501, saying why.
func refused501(why string) fieldRule {
	return fieldRule{how: fieldRefused, refuse: func(f string, v any) error {
		if notSet(v) {
			return nil
		}
		return unimplemented(f, why)
	}}
}

// sqlServerOnly refuses a SQL Server setting with 400: CloudBurrow runs
// PostgreSQL and MySQL, and Cloud SQL refuses these on either.
func sqlServerOnly() fieldRule {
	return fieldRule{how: fieldRefused, refuse: func(f string, v any) error {
		if notSet(v) {
			return nil
		}
		return Errorf(http.StatusBadRequest, "Invalid request: %s is for Cloud SQL for SQL Server instances only; "+
			"CloudBurrow runs PostgreSQL and MySQL.", f)
	}}
}

// refusedValues takes the values in ok (and the default) and refuses any
// other with 501, saying why.
func refusedValues(why string, ok ...string) fieldRule {
	return fieldRule{how: fieldRefused, refuse: func(f string, v any) error {
		if notSet(v) {
			return nil
		}
		s, _ := v.(string)
		for _, o := range ok {
			if s == o {
				return nil
			}
		}
		return unimplemented(fmt.Sprintf("%s %v", f, v), why)
	}}
}

const (
	maintenanceWhy = "no Google-managed maintenance is ever done here: an instance's server is the image's version, and it has no maintenance versions"
	readPoolWhy    = "read pools (READ_POOL_INSTANCE, Enterprise Plus) are not served; create read replicas with masterInstanceName"
)

// instanceFields are DatabaseInstance's properties, as instances.insert,
// patch and update take them.
var instanceFields = map[string]fieldRule{
	"name": kept, "databaseVersion": kept, "rootPassword": kept, "masterInstanceName": kept,
	"instanceType": kept, "region": kept, "replicaConfiguration": kept, "settings": kept,
	"includeReplicasForMajorVersionUpgrade": kept, "onPremisesConfiguration": kept,
	// A legacy failover replica is 400 (#1462), in instanceBody.unsupported.
	"failoverReplica": kept,
	// A customer-managed encryption key (#1538): diskencryption.go.
	"diskEncryptionConfiguration": kept,

	"availableMaintenanceVersions": output, "backendType": output, "connectionName": output, "createTime": output,
	"currentDiskSize": output, "databaseInstalledVersion": output, "deploymentInfo": output,
	"diskEncryptionStatus": output, "dnsName": output, "dnsNames": output, "etag": output, "gceZone": output,
	"ipAddresses": output, "ipv6Address": output, "kind": output, "maxDiskSize": output, "nodes": output,
	"outOfDiskReport": output, "primaryDnsName": output, "project": output, "pscServiceAttachmentLink": output,
	"replicaNames": output, "satisfiesPzi": output, "satisfiesPzs": output, "scheduledMaintenance": output,
	"secondaryGceZone": output, "selfLink": output, "serverCaCert": output, "serviceAccountEmailAddress": output,
	"sqlNetworkArchitecture": output, "state": output, "suspensionReason": output,
	"upgradableDatabaseVersions": output, "writeEndpoint": output,

	"databaseCenterIntegrationEnabled": refused501("Database Center is not served: no instance metadata is sent anywhere"),
	"geminiConfig":                     refused501("Gemini in Databases is not served"),
	"maintenanceVersion":               refused501(maintenanceWhy),
	"nodeCount":                        refused501(readPoolWhy),
	// A MySQL primary's DR replica (#1614): switchover.go.
	"replicationCluster": kept,
	"switchTransactionLogsToCloudStorageEnabled": refused501("point-in-time recovery logs are kept with the instance's backups, " +
		"not on its data disk, so there is nothing to switch"),
	"tags": refused501("tag bindings are not made for Cloud SQL instances"),
}

// settingsFields are Settings' properties.
var settingsFields = map[string]fieldRule{
	"tier": kept, "userLabels": kept, "databaseFlags": kept, "deletionProtectionEnabled": kept,
	"availabilityType": kept, "activationPolicy": kept, "backupConfiguration": kept, "dataApiAccess": kept,
	"edition": kept, "dataDiskSizeGb": kept,
	// settingsVersion is checked by patch and update (#1538): a stale one is
	// 412, as Cloud SQL refuses a concurrent change.
	"settingsVersion": kept,
	// databaseReplicationEnabled is a read replica's replication state: see
	// checkReplicationSetting.
	"databaseReplicationEnabled": kept,
	// retainBackupsOnDelete and finalBackupConfig decide what instances.
	// delete keeps (#1538).
	"retainBackupsOnDelete": kept, "finalBackupConfig": kept,
	"kind": output,

	"authorizedGaeApplications": stored, "autoUpgradeEnabled": stored, "crashSafeReplicationEnabled": stored,
	"dataCacheConfig": stored, "dataDiskProvisionedIops": stored, "dataDiskProvisionedThroughput": stored,
	"dataDiskType": stored, "denyMaintenancePeriods": stored, "enableDataplexIntegration": stored,
	"insightsConfig": stored, "locationPreference": stored, "maintenanceWindow": stored,
	"performanceCaptureConfig": stored, "pricingPlan": stored, "replicationLagMaxSeconds": stored,
	"replicationType": stored, "storageAutoResize": stored, "storageAutoResizeLimit": stored,
	"acceleratedReplicaMode": stored,
	// ipConfiguration is recorded; a server CA mode other than the internal
	// CA's is refused (checkIPConfiguration).
	"ipConfiguration": stored,

	"activeDirectoryConfig": sqlServerOnly(), "advancedMachineFeatures": sqlServerOnly(), "collation": sqlServerOnly(),
	"entraidConfig": sqlServerOnly(), "sqlServerAuditConfig": sqlServerOnly(), "timeZone": sqlServerOnly(),

	"connectionPoolConfig": refused501("managed connection pooling is not served: connect to the server, or run a pooler of your own"),
	"connectorEnforcement": refusedValues("the server is reached directly through its host tunnel as well as through the connectors, "+
		"and direct connections are not refused", "NOT_REQUIRED"),
	"enableGoogleMlIntegration": refused501("the server has no Vertex AI integration (google_ml_integration)"),
	"passwordValidationPolicy":  refused501("a user's password is not checked against a policy"),
	"readPoolAutoScaleConfig":   refused501(readPoolWhy),
}

// userFields are User's properties, as users.insert and update take them.
var userFields = map[string]fieldRule{
	"name": kept, "host": kept, "password": kept, "type": kept,
	"etag": output, "kind": output, "instance": output, "project": output, "iamStatus": output,
	"iamEmail":             refused501(iamWhy),
	"databaseRoles":        refused501("users are created without role memberships; GRANT them with executeSql or a client"),
	"dualPasswordType":     refusedValues("MySQL's dual passwords (RETAIN CURRENT PASSWORD) are not set through the API", "NO_DUAL_PASSWORD"),
	"passwordPolicy":       refused501("a user's password is not checked against a policy"),
	"serverRoles":          sqlServerOnly(),
	"sqlserverUserDetails": sqlServerOnly(),
}

// databaseFields are Database's properties.
var databaseFields = map[string]fieldRule{
	"name": kept, "charset": kept, "collation": kept,
	"etag": output, "kind": output, "instance": output, "project": output, "selfLink": output,
	"sqlserverDatabaseDetails": sqlServerOnly(),
}

// backupRunFields are BackupRun's properties, as backupRuns.insert takes
// them.
var backupRunFields = map[string]fieldRule{
	"description": kept, "type": kept,
	"backupKind": refusedValues("a backup here is a logical dump of the server's databases, not a disk snapshot "+
		"or a physical backup", "SNAPSHOT"),
	"location": {how: fieldRefused, refuse: func(f string, v any) error {
		if notSet(v) || v == Region {
			return nil
		}
		return unimplemented(fmt.Sprintf("a backup in location %v", v), "CloudBurrow keeps every backup in its one region, "+Region)
	}},
	"diskEncryptionConfiguration": {how: fieldRefused, refuse: func(f string, v any) error {
		if notSet(v) {
			return nil
		}
		return Errorf(http.StatusBadRequest, "Invalid request: %s cannot be set on a backup run: a backup inherits its instance's "+
			"encryption, as Cloud SQL documents.", f)
	}},
	"timeZone":        sqlServerOnly(),
	"databaseVersion": output, "diskEncryptionStatus": output, "endTime": output, "enqueuedTime": output,
	"error": output, "id": output, "instance": output, "kind": output, "maxChargeableBytes": output,
	"selfLink": output, "startTime": output, "status": output, "windowStartTime": output,
}

// backupFields are Backup's properties, as CreateBackup and UpdateBackup
// take them.
var backupFields = map[string]fieldRule{
	"description": kept, "expiryTime": kept, "instance": kept, "location": kept, "ttlDays": kept,
	"backupInterval": output, "backupKind": output, "backupRun": output, "databaseVersion": output,
	"error": output, "instanceDeletionTime": output, "instanceSettings": output, "kind": output,
	"kmsKey": output, "kmsKeyVersion": output, "maxChargeableBytes": output, "name": output,
	"satisfiesPzi": output, "satisfiesPzs": output, "selfLink": output, "state": output,
	"timeZone": output, "type": output,
}

// sslCertFields are SslCertsInsertRequest's properties.
var sslCertFields = map[string]fieldRule{"commonName": kept}

// checkFields refuses an unknown field of schema, and a refused value.
func checkFields(schema string, rules map[string]fieldRule, body map[string]any) error {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rule, ok := rules[k]
		if !ok {
			return Errorf(http.StatusBadRequest, "Invalid JSON payload received. Unknown name %q at '%s': Cannot find field.", k, schema)
		}
		if rule.refuse != nil {
			if err := rule.refuse(prefixed(schema, k), body[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// prefixed names a field as the error says it: settings' fields with
// settings., the others by their own name.
func prefixed(schema, k string) string {
	if schema == "settings" {
		return "settings." + k
	}
	return k
}

// readChecked reads a request body of schema, refuses what checkFields
// refuses, and decodes it into v.
func readChecked(r *http.Request, schema string, rules map[string]fieldRule, v any) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return Errorf(http.StatusBadRequest, "Invalid request: %v", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	if err := checkFields(schema, rules, m); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	return nil
}

// checkInstanceFields checks a DatabaseInstance body and its settings.
func checkInstanceFields(body []byte) error {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	if err := checkFields("instance", instanceFields, m); err != nil {
		return err
	}
	st, _ := m["settings"].(map[string]any)
	if err := checkFields("settings", settingsFields, st); err != nil {
		return err
	}
	return checkIPConfiguration(st["ipConfiguration"])
}

// checkIPConfiguration refuses a server CA other than the internal one:
// instances here are GOOGLE_MANAGED_INTERNAL_CA, and per-instance CAs are
// issued by Certificate Authority Service, which is not served.
func checkIPConfiguration(v any) error {
	ic, _ := v.(map[string]any)
	switch m, _ := ic["serverCaMode"].(string); m {
	case "", "GOOGLE_MANAGED_INTERNAL_CA", "CA_MODE_UNSPECIFIED":
	default:
		return unimplemented("settings.ipConfiguration.serverCaMode "+m, "per-instance server certificates are issued by "+
			"Certificate Authority Service, which is not served; instances here are GOOGLE_MANAGED_INTERNAL_CA")
	}
	if p, _ := ic["serverCaPool"].(string); p != "" {
		return unimplemented("settings.ipConfiguration.serverCaPool", "a CUSTOMER_MANAGED_CAS_CA pool is Certificate Authority Service's, which is not served")
	}
	return nil
}

// fieldNames is a sorted list of the fields of rules treated as how, for
// the documentation and the tests.
func fieldNames(rules map[string]fieldRule, how string) []string {
	var out []string
	for k, r := range rules {
		if r.how == how {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// checkSettingsVersion refuses a patch or update whose settings.
// settingsVersion is not the instance's: Cloud SQL's guard against a
// concurrent change (#1538).
func checkSettingsVersion(raw map[string]any, cur Spec) error {
	v, ok := raw["settingsVersion"]
	if !ok || v == nil {
		return nil
	}
	if fmt.Sprint(v) != fmt.Sprint(cur.SettingsVersion) {
		return Errorf(http.StatusPreconditionFailed, "Precondition check failed: settings.settingsVersion is %v, and the instance's is %d: "+
			"it was changed since it was read.", v, cur.SettingsVersion)
	}
	return nil
}

// checkReplicationSetting refuses a settings.databaseReplicationEnabled
// that is not the instance's replication state: replication is stopped and
// started with instances.stopReplica and startReplica, and a primary has
// none.
func checkReplicationSetting(raw map[string]any, spec Spec) error {
	v, ok := raw["databaseReplicationEnabled"]
	if !ok || v == nil {
		return nil
	}
	on, _ := v.(bool)
	if spec.MasterInstanceName == "" {
		if on {
			return Errorf(http.StatusBadRequest, "Invalid request: settings.databaseReplicationEnabled is for read replicas; %s is not one.", spec.Name)
		}
		return nil
	}
	if on != !spec.ReplicationStopped {
		return Errorf(http.StatusBadRequest, "Invalid request: settings.databaseReplicationEnabled is not changed by a patch here; "+
			"stop or start the replica's replication with instances.stopReplica or instances.startReplica.")
	}
	return nil
}

// retainBackups is settings.retainBackupsOnDelete.
func retainBackups(spec Spec) bool {
	v, _ := spec.Settings["retainBackupsOnDelete"].(bool)
	return v
}

// finalBackupConfigOf is the final backup settings.finalBackupConfig asks
// instances.delete for: none unless it is enabled, kept retentionDays (30
// when not given) after the instance is gone.
func (s *Server) finalBackupConfigOf(spec Spec) (finalBackup, error) {
	fc, _ := spec.Settings["finalBackupConfig"].(map[string]any)
	if on, _ := fc["enabled"].(bool); !on {
		return finalBackup{}, nil
	}
	if s.backups == nil {
		return finalBackup{}, s.noBackups("settings.finalBackupConfig")
	}
	days := int64(30)
	switch d := fc["retentionDays"].(type) {
	case float64:
		days = int64(d)
	case string:
		if _, err := fmt.Sscan(d, &days); err != nil {
			return finalBackup{}, Errorf(http.StatusBadRequest, "Invalid request: settings.finalBackupConfig.retentionDays %q is not a number.", d)
		}
	}
	if days == 0 {
		days = 30
	}
	exp, err := expiry(s.Clock(), days, "")
	return finalBackup{enabled: true, ttlDays: days, expiry: exp}, err
}
