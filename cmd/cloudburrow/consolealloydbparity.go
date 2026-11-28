package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// AlloyDB's 2026-11-25 console parity round (#2251), as Google's console
// has each:
//
//   - the Clusters list is one table of clusters with their instances
//     under them, in Google's columns (Resource name, Resource type,
//     Location, Status, Private IP address, High availability, Read pool
//     nodes, Automated backups, Most recent backup, Labels, Created,
//     Encryption: https://docs.cloud.google.com/alloydb/docs/cluster-list);
//   - a cluster's Data protection: Continuous backups and recovery policy's
//     Edit (Recovery window (days), Advanced encryption options) and
//     Automated backup policy's Edit (Automate backups), each ending in
//     Update, over the cluster's backups
//     (https://docs.cloud.google.com/alloydb/docs/backup/configure);
//   - Upgrade on the Overview: Choose database version, Continue, the
//     cluster ID typed back, Start upgrade, and the Overview's Upgrade
//     status with Cancel upgrade greyed out where the upgrade cannot be
//     cancelled
//     (https://docs.cloud.google.com/alloydb/docs/upgrade-db-inplace-major-version);
//   - Users rows' actions: Change password (Password or No password, OK)
//     and Delete
//     (https://docs.cloud.google.com/alloydb/docs/database-users/manage-roles).

// alloyDBDefaultRecoveryWindow is Google's default recovery window, in
// days, and the bounds Google takes.
const (
	alloyDBDefaultRecoveryWindow = 14
	alloyDBMaxRecoveryWindow     = 35
)

// alloyDBClusterColumns are Google's Clusters list's columns.
var alloyDBClusterColumns = []string{"Resource name", "Resource type", "Location", "Status", "Private IP address", "High availability",
	"Read pool nodes", "Automated backups", "Most recent backup", "Maintenance", "Labels", "Created", "Encryption"}

func alloyDBLabels(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+": "+m[k])
	}
	return strings.Join(out, ", ")
}

func alloyDBEncryption(cfg *alloydbpb.EncryptionConfig) string {
	if cfg.GetKmsKeyName() != "" {
		return "Customer-managed"
	}
	return "Google-managed"
}

func alloyDBInstanceResourceType(t alloydbpb.Instance_InstanceType) string {
	return alloyDBInstanceType(t) + " instance"
}

// alloyDBClusterListing is Google's Clusters list: a row a cluster, its
// instances under it.
func alloyDBClusterListing(ctx context.Context, s *alloydb.Server, project string) (console.Listing, error) {
	listing := console.Listing{Columns: alloyDBClusterColumns, NameColumn: "Resource name", Noun: "clusters",
		HiddenColumns: []string{"Labels", "Created", "Encryption"}, ChildNoun: "instances"}
	if project == "" {
		listing.Prompt = "AlloyDB lists a project's clusters. Choose one in the toolbar."
		return listing, nil
	}
	resp, err := s.ListClusters(ctx, &alloydbpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	var backups []*alloydbpb.Backup
	if b, err := s.ListBackups(ctx, &alloydbpb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000}); err == nil {
		backups = b.GetBackups()
	}
	for _, c := range resp.GetClusters() {
		ins, _ := s.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: c.GetName(), PageSize: 1000})
		instances := ins.GetInstances()
		sort.SliceStable(instances, func(i, j int) bool { return instances[i].GetInstanceType() < instances[j].GetInstanceType() })
		auto := "Disabled"
		if c.GetAutomatedBackupPolicy().GetEnabled() {
			auto = "Enabled"
		}
		recent := "-"
		var newest time.Time
		for _, b := range backups {
			if b.GetClusterName() == c.GetName() && b.GetState() == alloydbpb.Backup_READY && b.GetCreateTime().AsTime().After(newest) {
				newest, recent = b.GetCreateTime().AsTime(), kmsTime(b.GetCreateTime())
			}
		}
		clusterType := "Primary cluster"
		if c.GetClusterType() == alloydbpb.Cluster_SECONDARY {
			clusterType = "Secondary cluster"
		}
		privateIP, ha, pools := "-", "-", 0
		var children []console.Resource
		for _, in := range instances {
			row := map[string]string{"Resource name": lastSegment(in.GetName()), "Resource type": alloyDBInstanceResourceType(in.GetInstanceType()),
				"Location": kmsOrDash(in.GetGceZone()), "Status": alloyDBState(in.GetState()), "Private IP address": kmsOrDash(in.GetIpAddress()),
				"High availability": "-", "Read pool nodes": "-", "Automated backups": "-", "Most recent backup": "-", "Maintenance": "-",
				"Labels": alloyDBLabels(in.GetLabels()), "Created": kmsTime(in.GetCreateTime()), "Encryption": "-"}
			if row["Location"] == "-" {
				row["Location"] = kmsLocation(c.GetName())
			}
			switch in.GetInstanceType() {
			case alloydbpb.Instance_READ_POOL:
				n := int(in.GetReadPoolConfig().GetNodeCount())
				pools += n
				row["Read pool nodes"] = strconv.Itoa(n)
			default:
				row["High availability"] = "Disabled"
				if in.GetAvailabilityType() == alloydbpb.Instance_REGIONAL {
					row["High availability"] = "Enabled"
				}
				ha, privateIP = row["High availability"], row["Private IP address"]
			}
			children = append(children, console.Resource{Name: lastSegment(in.GetName()), Status: in.GetState().String(), Fields: row})
		}
		poolNodes := "-"
		if pools > 0 {
			poolNodes = strconv.Itoa(pools)
		}
		listing.Items = append(listing.Items, console.Resource{Name: c.GetName(), Status: c.GetState().String(), Children: children,
			Fields: map[string]string{"Resource name": lastSegment(c.GetName()), "Resource type": clusterType,
				"Location": kmsLocation(c.GetName()), "Status": alloyDBState(c.GetState()), "Private IP address": privateIP,
				"High availability": ha, "Read pool nodes": poolNodes, "Automated backups": auto, "Most recent backup": recent,
				"Maintenance": alloyDBUpcomingMaintenance(c),
				"Labels":      alloyDBLabels(c.GetLabels()), "Created": kmsTime(c.GetCreateTime()), "Encryption": alloyDBEncryption(c.GetEncryptionConfig())}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

// alloyDBDataProtection is a cluster's Data protection: its two policies,
// each with Google's Edit, over its backups.
func alloyDBDataProtection(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster) console.Section {
	cb := c.GetContinuousBackupConfig()
	cbState, window := "Disabled", "-"
	if cb.GetEnabled() {
		cbState, window = "Enabled", strconv.Itoa(int(cb.GetRecoveryWindowDays()))
	}
	earliest := "-"
	if t := c.GetContinuousBackupInfo().GetEarliestRestorableTime(); t != nil {
		earliest = kmsTime(t)
	}
	auto := c.GetAutomatedBackupPolicy()
	autoState, schedule, retention := "Disabled", "-", "-"
	if auto.GetEnabled() {
		autoState = "Enabled"
	}
	if w := auto.GetWeeklySchedule(); w != nil {
		days := len(w.GetDaysOfWeek())
		times := make([]string, 0, len(w.GetStartTimes()))
		for _, t := range w.GetStartTimes() {
			times = append(times, fmt.Sprintf("%02d:%02d", t.GetHours(), t.GetMinutes()))
		}
		when := "every day"
		if days != 0 && days != 7 {
			names := make([]string, 0, days)
			for _, d := range w.GetDaysOfWeek() {
				n := strings.ToLower(d.String())
				names = append(names, strings.ToUpper(n[:1])+n[1:])
			}
			when = strings.Join(names, ", ")
		}
		schedule = when + " at " + strings.Join(times, ", ") + " UTC"
	}
	switch {
	case auto.GetTimeBasedRetention() != nil:
		retention = fmt.Sprintf("%d days", int(auto.GetTimeBasedRetention().GetRetentionPeriod().AsDuration().Hours()/24))
	case auto.GetQuantityBasedRetention() != nil:
		retention = fmt.Sprintf("%d backups", auto.GetQuantityBasedRetention().GetCount())
	}
	return console.Section{ID: "backups", Label: "Data protection", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Continuous backups and recovery policy", Properties: []console.Property{
				{Label: "Continuous backup and recovery", Value: cbState, Action: "edit-continuous-backup", ActionText: "Edit"},
				{Label: "Recovery window (days)", Value: window},
				{Label: "Earliest restore point", Value: earliest},
				{Label: "Encryption", Value: alloyDBEncryption(cb.GetEncryptionConfig())}}},
			{Heading: "Automated backup policy", Properties: []console.Property{
				{Label: "Automated backups", Value: autoState, Action: "edit-automated-backup", ActionText: "Edit"},
				{Label: "Schedule", Value: schedule},
				{Label: "Retention", Value: retention}}},
		},
		Listing: alloyDBBackupListing(ctx, s, c.GetName(), kmsLocation(c.GetName()))}
}

// alloyDBDataProtectionActions are the Data protection's two Edits.
func alloyDBDataProtectionActions(c *alloydbpb.Cluster) []console.Action {
	window := int(c.GetContinuousBackupConfig().GetRecoveryWindowDays())
	if window == 0 {
		window = alloyDBDefaultRecoveryWindow
	}
	enc, key := "GOOGLE", c.GetContinuousBackupConfig().GetEncryptionConfig().GetKmsKeyName()
	if key != "" {
		enc = "CMEK"
	}
	automate := "false"
	if c.GetAutomatedBackupPolicy().GetEnabled() {
		automate = "true"
	}
	const advanced = "Advanced encryption options"
	return []console.Action{
		{ID: "edit-continuous-backup", Label: "Edit continuous backups and recovery policy", Submit: "Update", Linked: true, Fields: []console.Field{
			{Name: "window", Label: "Recovery window (days)", Type: "number", Required: true, Default: strconv.Itoa(window),
				Pattern: `^([1-9]|[12][0-9]|3[0-5])$`,
				Help:    "1 to 35 days. A shorter window takes effect at once; a longer one grows to its limit as time passes."},
			{Name: "encryption", Label: "Encryption", Type: "radio", Section: advanced, Default: enc, Options: []string{"GOOGLE", "CMEK"},
				OptionLabels: map[string]string{"GOOGLE": "Google-managed encryption key", "CMEK": "Customer-managed encryption key (CMEK)"}},
			{Name: "kmsKey", Label: "Select a customer-managed key", Type: "text", Section: advanced, Default: key, ShowWhen: "encryption=CMEK",
				Required: true, Help: "projects/PROJECT/locations/REGION/keyRings/RING/cryptoKeys/KEY, in the cluster's region."}}},
		{ID: "edit-automated-backup", Label: "Edit automated backup policy", Submit: "Update", Linked: true, Fields: []console.Field{
			{Name: "automate", Label: "Automate backups", Type: "checkbox", Default: automate,
				Help: "Turned on without a schedule of its own, the cluster is backed up once a day and each backup kept 14 days."}}},
	}
}

// alloyDBUpgradeTargets are the newer major versions a cluster upgrades to.
func alloyDBUpgradeTargets(from alloydbpb.DatabaseVersion) []string {
	var out []string
	for i := len(alloyDBVersions) - 1; i >= 0; i-- {
		if v := alloydbpb.DatabaseVersion(alloydbpb.DatabaseVersion_value[alloyDBVersions[i]]); v > from {
			out = append(out, alloyDBVersions[i])
		}
	}
	return out
}

// alloyDBUpgradeAction is Google's Upgrade: Choose database version, then
// Upgrade cluster, the cluster's ID typed back, ending in Start upgrade.
func alloyDBUpgradeAction(c *alloydbpb.Cluster, running bool) console.Action {
	id := lastSegment(c.GetName())
	targets := alloyDBUpgradeTargets(c.GetDatabaseVersion())
	labels := map[string]string{}
	for _, v := range targets {
		labels[v] = strings.TrimSuffix(alloyDBVersionLabels[v], " (default)")
	}
	a := console.Action{ID: "upgrade", Label: "Upgrade", Title: "Upgrade cluster " + id, Steps: "continue", Submit: "Start upgrade",
		Fields: []console.Field{
			{Name: "version", Label: "Database version", Type: "radio", Required: true, Section: "Choose database version",
				Options: targets, OptionLabels: labels, Help: "Currently " + alloyDBVersionLabel(c.GetDatabaseVersion()) +
					". The primary instance is unavailable while it is upgraded, and every read pool instance is rebuilt."},
			{Name: "confirm", Label: "Cluster ID", Type: "text", Required: true, Section: "Upgrade cluster",
				Pattern: "^" + regexp.QuoteMeta(id) + "$", Help: "Enter the cluster's ID, " + id + ", to start the upgrade."}}}
	if len(targets) > 0 {
		a.Fields[0].Default = targets[0]
	}
	switch {
	case running:
		a.Unavailable = "The cluster is being upgraded: see Upgrade status."
	case c.GetClusterType() == alloydbpb.Cluster_SECONDARY:
		a.Unavailable = "A secondary cluster is upgraded with its primary cluster."
	case len(targets) == 0:
		a.Unavailable = alloyDBVersionLabel(c.GetDatabaseVersion()) + " is the newest major version."
	}
	return a
}

// alloyDBLastUpgrade is the cluster's newest upgrade operation, nil when
// it has never been upgraded.
func alloyDBLastUpgrade(ctx context.Context, s *alloydb.Server, cluster string) (*longrunningpb.Operation, *alloydbpb.OperationMetadata) {
	loc := strings.Join(strings.Split(cluster, "/")[:4], "/")
	resp, err := s.Operations().ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: loc, PageSize: 1000})
	if err != nil {
		return nil, nil
	}
	var last *longrunningpb.Operation
	var lastMD *alloydbpb.OperationMetadata
	for _, op := range resp.GetOperations() {
		md := &alloydbpb.OperationMetadata{}
		if op.GetMetadata().UnmarshalTo(md) != nil || md.GetVerb() != "upgrade" || md.GetTarget() != cluster {
			continue
		}
		if lastMD == nil || md.GetCreateTime().AsTime().After(lastMD.GetCreateTime().AsTime()) {
			last, lastMD = op, md
		}
	}
	return last, lastMD
}

// alloyDBUpgradeStatus is the Overview's Upgrade status group, and whether
// the upgrade is still running.
func alloyDBUpgradeStatus(ctx context.Context, s *alloydb.Server, cluster string) (*console.PropertyGroup, bool) {
	op, md := alloyDBLastUpgrade(ctx, s, cluster)
	if op == nil {
		return nil, false
	}
	status, detail := "In progress", "-"
	switch {
	case !op.GetDone():
	case op.GetError() != nil:
		status, detail = "Failed", op.GetError().GetMessage()
	default:
		status = "Succeeded"
		resp := &alloydbpb.UpgradeClusterResponse{}
		if op.GetResponse().UnmarshalTo(resp) == nil && resp.GetMessage() != "" {
			detail = resp.GetMessage()
		}
	}
	ended := "-"
	if md.GetEndTime() != nil {
		ended = kmsTime(md.GetEndTime())
	}
	return &console.PropertyGroup{Heading: "Upgrade status", Properties: []console.Property{
		{Label: "Upgrade status", Value: status}, {Label: "Details", Value: detail},
		{Label: "Started", Value: kmsTime(md.GetCreateTime())}, {Label: "Ended", Value: ended}}}, !op.GetDone()
}

// alloyDBCancelUpgrade is Google's Cancel upgrade, greyed out as Google
// greys it where the upgrade cannot be cancelled: this server's upgrade is
// never stopped half-way.
func alloyDBCancelUpgrade(running bool) console.Action {
	a := console.Action{ID: "cancel-upgrade", Label: "Cancel upgrade",
		Unavailable: "This upgrade cannot be cancelled: the server is not stopped half-way through its upgrade."}
	if !running {
		a.Unavailable = "No upgrade is running."
	}
	return a
}

func (p alloyDBProvider) upgrade(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, values map[string]string) error {
	_, running := alloyDBUpgradeStatus(ctx, s, c.GetName())
	if why := alloyDBUpgradeAction(c, running).Unavailable; why != "" {
		return alloyDBErrorOf(why)
	}
	if strings.TrimSpace(values["confirm"]) != lastSegment(c.GetName()) {
		return fmt.Errorf("enter the cluster's ID, %s, to start the upgrade", lastSegment(c.GetName()))
	}
	v, ok := alloydbpb.DatabaseVersion_value[values["version"]]
	if !ok {
		return fmt.Errorf("choose a database version")
	}
	// The upgrade runs on: the Overview's Upgrade status follows it.
	_, err := s.UpgradeCluster(ctx, &alloydbpb.UpgradeClusterRequest{Name: c.GetName(), Version: alloydbpb.DatabaseVersion(v)})
	return err
}

func (p alloyDBProvider) editContinuousBackup(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, values map[string]string) error {
	days, err := strconv.Atoi(strings.TrimSpace(values["window"]))
	if err != nil || days < 1 || days > alloyDBMaxRecoveryWindow {
		return fmt.Errorf("the recovery window is 1 to %d days", alloyDBMaxRecoveryWindow)
	}
	cb := &alloydbpb.ContinuousBackupConfig{Enabled: c.GetContinuousBackupConfig().Enabled, RecoveryWindowDays: int32(days)}
	if cb.Enabled == nil {
		on := true
		cb.Enabled = &on
	}
	if values["encryption"] == "CMEK" {
		key := strings.TrimSpace(values["kmsKey"])
		if key == "" {
			return fmt.Errorf("select a customer-managed key")
		}
		cb.EncryptionConfig = &alloydbpb.EncryptionConfig{KmsKeyName: key}
	}
	op, err := s.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: c.GetName(), ContinuousBackupConfig: cb},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"continuous_backup_config"}}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 30*time.Second)
}

func (p alloyDBProvider) editAutomatedBackup(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, values map[string]string) error {
	on := values["automate"] == "true" || values["automate"] == "on"
	policy := c.GetAutomatedBackupPolicy()
	if policy == nil || (policy.GetWeeklySchedule() == nil && policy.GetTimeBasedRetention() == nil && policy.GetQuantityBasedRetention() == nil) {
		// Google's default where the cluster has no policy of its own: a
		// backup a day, each kept 14 days.
		policy = &alloydbpb.AutomatedBackupPolicy{
			Schedule: &alloydbpb.AutomatedBackupPolicy_WeeklySchedule_{WeeklySchedule: &alloydbpb.AutomatedBackupPolicy_WeeklySchedule{
				StartTimes: []*timeofday.TimeOfDay{{Hours: 23}},
				DaysOfWeek: []dayofweek.DayOfWeek{dayofweek.DayOfWeek_MONDAY, dayofweek.DayOfWeek_TUESDAY, dayofweek.DayOfWeek_WEDNESDAY,
					dayofweek.DayOfWeek_THURSDAY, dayofweek.DayOfWeek_FRIDAY, dayofweek.DayOfWeek_SATURDAY, dayofweek.DayOfWeek_SUNDAY}}},
			Retention: &alloydbpb.AutomatedBackupPolicy_TimeBasedRetention_{TimeBasedRetention: &alloydbpb.AutomatedBackupPolicy_TimeBasedRetention{
				RetentionPeriod: durationpb.New(alloyDBDefaultRecoveryWindow * 24 * time.Hour)}},
			BackupWindow: durationpb.New(time.Hour)}
	} else {
		policy = proto.Clone(policy).(*alloydbpb.AutomatedBackupPolicy)
	}
	policy.Enabled = &on
	op, err := s.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: c.GetName(), AutomatedBackupPolicy: policy},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"automated_backup_policy"}}})
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 30*time.Second)
}

// alloyDBUserRowActions are a Users row's actions menu: Change password
// (Google's, for a built-in user) and Delete.
func alloyDBUserRowActions(u *alloydbpb.User) []console.Action {
	change := console.Action{ID: "change-password", Label: "Change password", Submit: "OK", Fields: []console.Field{
		{Name: "password", Label: "Password", Type: "password", Help: "The user's new password; left empty with No password checked."},
		{Name: "noPassword", Label: "No password", Type: "checkbox", Default: "false",
			Help: "The user then cannot sign in with a password."}}}
	if u.GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER {
		change.Unavailable = "A Cloud IAM user signs in with an IAM access token, not a password."
	}
	return []console.Action{change,
		{ID: "delete-user", Label: "Delete", Destructive: true, ConfirmTitle: "Delete user account?",
			Confirm: "The user can no longer connect; the objects it owns stay."}}
}

// userActions performs a Users row's action at [cluster, user].
func (p alloyDBProvider) userActions(ctx context.Context, s *alloydb.Server, c *alloydbpb.Cluster, user, action string, values map[string]string) error {
	name := c.GetName() + "/users/" + user
	switch action {
	case "change-password":
		pw := values["password"]
		if values["noPassword"] == "true" || values["noPassword"] == "on" {
			pw = ""
		} else if pw == "" {
			return fmt.Errorf("enter a password, or check No password")
		}
		_, err := s.UpdateUser(ctx, &alloydbpb.UpdateUserRequest{User: &alloydbpb.User{Name: name, Password: pw},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"password"}}})
		return err
	case "delete-user":
		_, err := s.DeleteUser(ctx, &alloydbpb.DeleteUserRequest{Name: name})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func alloyDBErrorOf(why string) error {
	if why == "" {
		return nil
	}
	return fmt.Errorf("%s", why)
}
