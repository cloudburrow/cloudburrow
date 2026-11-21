package main

// The Memorystore create pages' shared fields (#1862), as Google's Create
// cluster (Redis Cluster) and Create instance (Valkey) pages have them,
// in Google's order: Zonal availability, Set up connection, Security (one
// choice, IAM Auth or Token Auth checking and locking Enable In-Transit
// Encryption (TLS)), Certificate authority management, Encryption, ACL
// policy, Data persistence strategy, Automated daily backups and
// Maintenance. Sources:
// https://docs.cloud.google.com/memorystore/docs/cluster/create-instances,
// https://docs.cloud.google.com/memorystore/docs/valkey/create-instances.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// The create pages' section headings, Google's.
const (
	msSectionConnection  = "Set up connection"
	msSectionSecurity    = "Security"
	msSectionCA          = "Certificate authority management"
	msSectionEncryption  = "Encryption"
	msSectionACL         = "ACL policy"
	msSectionPersistence = "Data persistence strategy"
	msSectionBackups     = "Automated backups"
	msSectionMaintenance = "Maintenance"
	msSectionAdvanced    = "Advanced options"
	msSectionNetwork     = "Advanced network options"
)

// memorystoreHours are a day's hours, the Time window and maintenance
// start hour choices.
var memorystoreHours = func() []string {
	out := make([]string, 24)
	for h := range out {
		out[h] = strconv.Itoa(h)
	}
	return out
}()

var memorystoreHourLabels = func() map[string]string {
	out := map[string]string{}
	for h := 0; h < 24; h++ {
		out[strconv.Itoa(h)] = fmt.Sprintf("%02d:00 - %02d:00 UTC", h, (h+1)%24)
	}
	return out
}()

// memorystoreDays are the maintenance window's days, Monday first.
var memorystoreDays = []string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

var memorystoreDayLabels = map[string]string{"MONDAY": "Monday", "TUESDAY": "Tuesday", "WEDNESDAY": "Wednesday",
	"THURSDAY": "Thursday", "FRIDAY": "Friday", "SATURDAY": "Saturday", "SUNDAY": "Sunday"}

// memorystoreZonalFields are Zonal availability and its Zone.
func memorystoreZonalFields() []console.Field {
	return []console.Field{
		{Name: "zoneDistribution", Label: "Zonal availability", Type: "radio", Default: "MULTI_ZONE",
			Options:      []string{"MULTI_ZONE", "SINGLE_ZONE"},
			OptionLabels: map[string]string{"MULTI_ZONE": "Multi-zone", "SINGLE_ZONE": "Single zone"},
			Help:         "Multi-zone spreads the nodes over the region's zones; Single zone keeps them in one. Every node runs on this machine either way."},
		{Name: "zone", Label: "Zone", Type: "text", Default: redisadmin.Region + "-a", ShowWhen: "zoneDistribution=SINGLE_ZONE",
			Pattern: `^[a-z]+-[a-z]+[0-9]+-[a-z]$`, Help: "A zone of the region, such as " + redisadmin.Region + "-a."},
	}
}

// memorystoreSecurityFields are Security, the TLS box it holds, and
// Certificate authority management with its CA pool: server_ca_mode and
// server_ca_pool, each mode served (Redis clusters #1907, Valkey
// instances #1918; a customer-managed CA is this instance's Certificate
// Authority Service).
func memorystoreSecurityFields(what string) []console.Field {
	return []console.Field{
		{Name: "security", Label: "Security", Type: "radio", Default: "DISABLED", Section: msSectionSecurity,
			Options:      []string{"IAM_AUTH", "TOKEN_AUTH", "DISABLED"},
			OptionLabels: map[string]string{"IAM_AUTH": "IAM Auth", "TOKEN_AUTH": "Token Auth", "DISABLED": "Disabled"},
			Help: "IAM Auth: clients send an access token, minted by this instance for a principal granted the connect role on the project, as the AUTH string. " +
				"Token Auth: clients log in as a token auth user with a token the service generates, which the " + what + "'s page adds and shows."},
		{Name: "transitEncryption", Label: "Enable In-Transit Encryption (TLS)", Type: "checkbox", Section: msSectionSecurity,
			CheckedWhen: "security=IAM_AUTH|TOKEN_AUTH",
			Help:        "Every node serves TLS only, with a certificate of the " + what + "'s own CA, which its page shows. It cannot be changed later."},
		{Name: "caMode", Label: "Certificate authority management", Type: "radio", Default: "PER_INSTANCE", Section: msSectionCA,
			ShowWhen: "transitEncryption", Options: []string{"PER_INSTANCE", "CUSTOMER_MANAGED", "SHARED"},
			OptionLabels: map[string]string{"PER_INSTANCE": "Per-instance CA", "CUSTOMER_MANAGED": "Customer-managed CA", "SHARED": "Shared CA"},
			Help: "Per-instance CA: the " + what + "'s own CA, which its page shows. Shared CA: the region's CA, valid for every " + what +
				" of the region that uses it. Customer-managed CA: a Certificate Authority Service CA pool of the " + what + "'s region issues each node's certificate. It cannot be changed later."},
		{Name: "caPool", Label: "CA pool", Type: "text", Section: msSectionCA, ShowWhen: "caMode=CUSTOMER_MANAGED",
			Pattern: `^(projects/[^\/]+/locations/[^\/]+/caPools/[^\/]+)?$`,
			Help:    "projects/{project}/locations/{region}/caPools/{pool}, with an enabled CA, in the " + what + "'s region. Clients trust the pool's CA certificates."},
	}
}

// memorystoreCAMode is the form's server_ca_mode, as the number both
// APIs' enums share (1 per-instance, 2 shared, 3 customer-managed), and
// server_ca_pool; 0 without in-transit encryption or a choice.
func memorystoreCAMode(values map[string]string) (int32, string) {
	if !memorystoreTLS(values) {
		return 0, ""
	}
	switch values["caMode"] {
	case "SHARED":
		return 2, ""
	case "CUSTOMER_MANAGED":
		return 3, strings.TrimSpace(values["caPool"])
	}
	return 0, ""
}

// memorystoreAuthMode is the Security choice, with the legacy iamAuth and
// tokenAuth boxes a client of the console API may still send.
func memorystoreAuthMode(values map[string]string) string {
	switch {
	case values["security"] != "":
		return values["security"]
	case values["tokenAuth"] == "true":
		return "TOKEN_AUTH"
	case values["iamAuth"] == "true":
		return "IAM_AUTH"
	}
	return "DISABLED"
}

// memorystoreTLS is whether the form asks for in-transit encryption: the
// box, which Security holds on with an auth mode, as Google's page does.
func memorystoreTLS(values map[string]string) bool {
	if values["security"] == "IAM_AUTH" || values["security"] == "TOKEN_AUTH" {
		return true
	}
	return values["transitEncryption"] == "true"
}

// memorystoreEncryptionFields are Encryption and its Cloud KMS key.
func memorystoreEncryptionFields(what string) []console.Field {
	return []console.Field{
		{Name: "encryption", Label: "Encryption", Type: "radio", Default: "GOOGLE", Section: msSectionEncryption,
			Options:      []string{"GOOGLE", "CMEK"},
			OptionLabels: map[string]string{"GOOGLE": "Google-managed encryption key", "CMEK": "Cloud KMS key"}},
		{Name: "kmsKey", Label: "Select a Cloud KMS key", Type: "text", Section: msSectionEncryption, ShowWhen: "encryption=CMEK",
			Pattern: `^(projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+)?$`,
			Help:    "projects/{project}/locations/{region}/keyRings/{ring}/cryptoKeys/{key}, a key of this instance's Cloud KMS in the " + what + "'s region (#1557)."},
	}
}

// memorystoreKMSKey is the key the form names, when Encryption is Cloud
// KMS key (or a console API client sends only the key).
func memorystoreKMSKey(values map[string]string) string {
	if values["encryption"] == "GOOGLE" {
		return ""
	}
	return strings.TrimSpace(values["kmsKey"])
}

// memorystorePersistenceField is Data persistence strategy; snapshot is
// RDB's name, "Redis Database (RDB)" or Valkey's "Memory Snapshot".
func memorystorePersistenceField(snapshot string) console.Field {
	return console.Field{Name: "persistence", Label: "Data persistence strategy", Type: "radio", Default: "DISABLED",
		Section: msSectionPersistence, Options: []string{"DISABLED", "AOF", "RDB"},
		OptionLabels: map[string]string{"DISABLED": "No persistence", "AOF": "Append Only File (AOF) persistence", "RDB": snapshot + " persistence"},
		Help:         "With persistence the data is kept across a restart of CloudBurrow; without, it is lost."}
}

// memorystoreBackupFields are Automated daily backups, Days to retain
// backups and Time window.
func memorystoreBackupFields() []console.Field {
	return []console.Field{
		{Name: "autoBackup", Label: "Automated daily backups", Type: "checkbox", Section: msSectionBackups,
			Help: "A backup is taken every day in the time window and kept for the days chosen."},
		{Name: "backupRetention", Label: "Days to retain backups", Type: "number", Default: "35", Section: msSectionBackups,
			ShowWhen: "autoBackup", Pattern: `^([1-9]|[1-9][0-9]|[12][0-9][0-9]|3[0-5][0-9]|36[0-5])$`, Help: "1 to 365."},
		{Name: "backupHour", Label: "Time window", Type: "select", Default: "0", Section: msSectionBackups, ShowWhen: "autoBackup",
			Options: memorystoreHours, OptionLabels: memorystoreHourLabels},
	}
}

// memorystoreAutoBackup reads the backup fields: whether they are on, the
// hour and the retention.
func memorystoreAutoBackup(values map[string]string) (on bool, hour int32, retention time.Duration, err error) {
	if values["autoBackup"] != "true" {
		return false, 0, 0, nil
	}
	days := 35
	if v := strings.TrimSpace(values["backupRetention"]); v != "" {
		if days, err = strconv.Atoi(v); err != nil || days < 1 || days > 365 {
			return false, 0, 0, fmt.Errorf("days to retain backups %q is not 1 to 365", v)
		}
	}
	h, err := strconv.Atoi(strings.TrimSpace(values["backupHour"]))
	if values["backupHour"] == "" {
		h, err = 0, nil
	}
	if err != nil || h < 0 || h > 23 {
		return false, 0, 0, fmt.Errorf("time window %q is not an hour 0 to 23", values["backupHour"])
	}
	return true, int32(h), time.Duration(days) * 24 * time.Hour, nil
}

// memorystoreMaintenanceFields are Maintenance, Default or Custom with its
// day and start hour.
func memorystoreMaintenanceFields() []console.Field {
	return []console.Field{
		{Name: "maintenance", Label: "Maintenance", Type: "radio", Default: "DEFAULT", Section: msSectionMaintenance,
			Options: []string{"DEFAULT", "CUSTOM"}, OptionLabels: map[string]string{"DEFAULT": "Default", "CUSTOM": "Custom"},
			Help: "Custom sets a weekly window (maintenance_policy) in which maintenance is scheduled."},
		{Name: "maintenanceDay", Label: "Day", Type: "select", Default: "SUNDAY", Section: msSectionMaintenance,
			ShowWhen: "maintenance=CUSTOM", Options: memorystoreDays, OptionLabels: memorystoreDayLabels},
		{Name: "maintenanceHour", Label: "Start hour", Type: "select", Default: "0", Section: msSectionMaintenance,
			ShowWhen: "maintenance=CUSTOM", Options: memorystoreHours, OptionLabels: memorystoreHourLabels},
	}
}

// memorystoreCreateWindow reads the maintenance fields: the day and hour
// of a Custom window, or has false for Default.
func memorystoreCreateWindow(values map[string]string) (day dayofweek.DayOfWeek, start *timeofday.TimeOfDay, has bool, err error) {
	if values["maintenance"] != "CUSTOM" {
		return 0, nil, false, nil
	}
	d, h, has, err := clusterMaintenanceWindow(values)
	if err != nil || !has {
		return 0, nil, false, err
	}
	return d, &timeofday.TimeOfDay{Hours: h}, true, nil
}

// memorystoreZones is Zonal availability as the page shows it.
func memorystoreZones(mode, zone string) string {
	if mode == "SINGLE_ZONE" {
		return "Single zone " + orDash(zone)
	}
	return "Multi-zone"
}

// memorystoreAutoBackupText is automated_backup_config as the page shows it.
func memorystoreAutoBackupText(on bool, hour int32, retention time.Duration) string {
	if !on {
		return "Disabled"
	}
	if retention == 0 {
		retention = 35 * 24 * time.Hour
	}
	return fmt.Sprintf("Daily at %s, kept %d days", memorystoreHourLabels[strconv.Itoa(int(hour))], int(retention.Hours()/24))
}

// memorystoreCAModeText is a server_ca_mode as the pages show it, empty
// when unset.
func memorystoreCAModeText(mode string, set bool) string {
	if !set {
		return ""
	}
	return mode
}
