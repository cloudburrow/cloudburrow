package iameval

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() {
	for name, svc := range map[string]*Service{
		"google.logging.v2.LoggingServiceV2":               Logging,
		"google.logging.v2.ConfigServiceV2":                Logging,
		"google.logging.v2.MetricsServiceV2":               Logging,
		"google.monitoring.v3.MetricService":               Monitoring,
		"google.monitoring.v3.AlertPolicyService":          Monitoring,
		"google.monitoring.v3.NotificationChannelService":  Monitoring,
		"google.monitoring.v3.UptimeCheckService":          Monitoring,
		"google.monitoring.v3.GroupService":                Monitoring,
		"google.monitoring.v3.ServiceMonitoringService":    Monitoring,
		"google.monitoring.v3.SnoozeService":               Monitoring,
		"google.monitoring.v3.QueryService":                Monitoring,
		"google.monitoring.dashboard.v1.DashboardsService": Monitoring,
		"google.cloud.run.v2.Services":                     Run,
		"google.cloud.run.v2.Jobs":                         Run,
		"google.cloud.run.v2.WorkerPools":                  Run,
		"google.cloud.functions.v2.FunctionService":        Functions,
		"google.cloud.functions.v1.CloudFunctionsService":  Functions,
		"google.cloud.eventarc.v1.Eventarc":                Eventarc,
		"google.cloud.redis.v1.CloudRedis":                 Redis,
		"google.cloud.redis.cluster.v1.CloudRedisCluster":  RedisCluster,
		"google.cloud.memorystore.v1.Memorystore":          Valkey,
	} {
		Services[name] = svc
	}
}

// rows merges sm's rows of several gRPC services.
func rows(byPrefix map[string]map[string][2]string) map[string]Method {
	m := map[string]Method{}
	for prefix, r := range byPrefix {
		for k, v := range sm(r, prefix) {
			m[k] = v
		}
	}
	return m
}

// Logging is Cloud Logging's table: the permission each method's REST
// reference names (https://docs.cloud.google.com/logging/docs/reference/v2/rest,
// read 2026-11-20): ListLogEntries "one or more of" logging.logEntries.list,
// logging.privateLogEntries.list and logging.views.access, of which
// logging.logEntries.list (Logs Viewer's) is checked, on each resource
// name; WriteLogEntries logging.logEntries.create on the log (the
// request's, else each entry's). GetCmekSettings' and UpdateCmekSettings'
// pages name logging.cmekSettings.get, which Google's permission index
// lacks, so they are not checked (UNVERIFIED, re-read 2026-11-20). The Operations mixin's
// logging.operations.* and the views' IAMPolicy mixin's
// logging.views.getIamPolicy and setIamPolicy are the permissions of their
// names in "Logging roles and permissions"
// (https://docs.cloud.google.com/iam/docs/roles-permissions/logging). A
// log view keeps its own policy (roles/logging.viewAccessor on one view).
// TailLogEntries is a stream, and saved queries and log scopes are served
// over JSON alone (no published proto), so they are not checked.
// A resource under an organization, folder or billing account rather than
// a project has no project ancestry here, so only the instance's own
// account passes there (a gap, #1879). A sink's custom writer identity's
// iam.serviceAccounts.actAs
// (https://docs.cloud.google.com/logging/docs/routing/user-managed-service-accounts)
// is not checked yet: a sink's update sends back the writer identity
// Logging chose. Google's wording of a
// denial is UNVERIFIED (re-read 2026-11-20:
// https://docs.cloud.google.com/logging/docs/access-control gives none):
// IAM Credentials' "Permission 'P' denied on resource (or it may not
// exist).", with ErrorInfo IAM_PERMISSION_DENIED.
var Logging = &Service{
	Name:              "logging.googleapis.com",
	PolicyCollections: []string{"views"},
	Types: map[string]string{"buckets": "logging.googleapis.com/LogBucket", "views": "logging.googleapis.com/LogView",
		"sinks": "logging.googleapis.com/LogSink", "metrics": "logging.googleapis.com/LogMetric",
		"exclusions": "logging.googleapis.com/LogExclusion", "links": "logging.googleapis.com/Link",
		"logs": "logging.googleapis.com/Log"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.logging.v2.LoggingServiceV2/": {
				"DeleteLog":       {"logging.logs.delete", "log_name"},
				"WriteLogEntries": {"logging.logEntries.create", "log_name|entries.log_name"},
				"ListLogEntries":  {"logging.logEntries.list", "resource_names"},
				"ListLogs":        {"logging.logs.list", "parent"},
			},
			"/google.logging.v2.ConfigServiceV2/": {
				"ListBuckets":       {"logging.buckets.list", "parent"},
				"GetBucket":         {"logging.buckets.get", "name"},
				"CreateBucket":      {"logging.buckets.create", "parent"},
				"CreateBucketAsync": {"logging.buckets.create", "parent"},
				"UpdateBucket":      {"logging.buckets.update", "name"},
				"UpdateBucketAsync": {"logging.buckets.update", "name"},
				"DeleteBucket":      {"logging.buckets.delete", "name"},
				"UndeleteBucket":    {"logging.buckets.undelete", "name"},
				"ListViews":         {"logging.views.list", "parent"},
				"GetView":           {"logging.views.get", "name"},
				"CreateView":        {"logging.views.create", "parent"},
				"UpdateView":        {"logging.views.update", "name"},
				"DeleteView":        {"logging.views.delete", "name"},
				"ListSinks":         {"logging.sinks.list", "parent"},
				"GetSink":           {"logging.sinks.get", "sink_name"},
				"CreateSink":        {"logging.sinks.create", "parent"},
				"UpdateSink":        {"logging.sinks.update", "sink_name"},
				"DeleteSink":        {"logging.sinks.delete", "sink_name"},
				"CreateLink":        {"logging.links.create", "parent"},
				"DeleteLink":        {"logging.links.delete", "name"},
				"ListLinks":         {"logging.links.list", "parent"},
				"GetLink":           {"logging.links.get", "name"},
				"ListExclusions":    {"logging.exclusions.list", "parent"},
				"GetExclusion":      {"logging.exclusions.get", "name"},
				"CreateExclusion":   {"logging.exclusions.create", "parent"},
				"UpdateExclusion":   {"logging.exclusions.update", "name"},
				"DeleteExclusion":   {"logging.exclusions.delete", "name"},
				"GetSettings":       {"logging.settings.get", "name"},
				"UpdateSettings":    {"logging.settings.update", "name"},
				"CopyLogEntries":    {"logging.buckets.copyLogEntries", "name"},
			},
			"/google.logging.v2.MetricsServiceV2/": {
				"ListLogMetrics":  {"logging.logMetrics.list", "parent"},
				"GetLogMetric":    {"logging.logMetrics.get", "metric_name"},
				"CreateLogMetric": {"logging.logMetrics.create", "parent"},
				"UpdateLogMetric": {"logging.logMetrics.update", "metric_name"},
				"DeleteLogMetric": {"logging.logMetrics.delete", "metric_name"},
			},
			"/google.longrunning.Operations/": {
				"GetOperation":    {"logging.operations.get", "name"},
				"ListOperations":  {"logging.operations.list", "name"},
				"CancelOperation": {"logging.operations.cancel", "name"},
			},
			"/google.iam.v1.IAMPolicy/": {
				"GetIamPolicy": {"logging.views.getIamPolicy", "resource"},
				"SetIamPolicy": {"logging.views.setIamPolicy", "resource"},
			},
		})
		return m
	}(),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource (or it may not exist).", permission),
			permission, resource, true)
	},
}

// Monitoring is Cloud Monitoring's table: the permission each method's
// REST reference names (https://docs.cloud.google.com/monitoring/api/ref_v3/rest
// and the v1 dashboards reference, read 2026-11-20), and, where a page
// names none (descriptors, time series, groups, services and SLOs,
// snoozes, and the patch methods), the permission of its name in
// "Monitoring roles and permissions"
// (https://docs.cloud.google.com/iam/docs/roles-permissions/monitoring).
// Three methods have no permission of their name, and are checked with
// the nearest: CreateServiceTimeSeries monitoring.timeSeries.create,
// QueryTimeSeries monitoring.timeSeries.list and ListGroupMembers
// monitoring.groups.get (UNVERIFIED, re-read 2026-11-20). ListUptimeCheckIps
// names no resource and is not checked. Monitoring keeps no
// resource-level policies. Google's wording of a denial is UNVERIFIED
// (re-read 2026-11-20: https://docs.cloud.google.com/monitoring/access-control
// gives none): its newer APIs' "Permission 'P' denied on resource 'R' (or
// it may not exist).", with ErrorInfo IAM_PERMISSION_DENIED.
var Monitoring = &Service{
	Name: "monitoring.googleapis.com",
	Types: map[string]string{"alertPolicies": "monitoring.googleapis.com/AlertPolicy",
		"notificationChannels": "monitoring.googleapis.com/NotificationChannel",
		"uptimeCheckConfigs":   "monitoring.googleapis.com/UptimeCheckConfig", "groups": "monitoring.googleapis.com/Group",
		"dashboards": "monitoring.googleapis.com/Dashboard", "snoozes": "monitoring.googleapis.com/Snooze"},
	Methods: rows(map[string]map[string][2]string{
		"/google.monitoring.v3.MetricService/": {
			"ListMonitoredResourceDescriptors": {"monitoring.monitoredResourceDescriptors.list", "name"},
			"GetMonitoredResourceDescriptor":   {"monitoring.monitoredResourceDescriptors.get", "name"},
			"ListMetricDescriptors":            {"monitoring.metricDescriptors.list", "name"},
			"GetMetricDescriptor":              {"monitoring.metricDescriptors.get", "name"},
			"CreateMetricDescriptor":           {"monitoring.metricDescriptors.create", "name"},
			"DeleteMetricDescriptor":           {"monitoring.metricDescriptors.delete", "name"},
			"ListTimeSeries":                   {"monitoring.timeSeries.list", "name"},
			"CreateTimeSeries":                 {"monitoring.timeSeries.create", "name"},
			"CreateServiceTimeSeries":          {"monitoring.timeSeries.create", "name"},
		},
		"/google.monitoring.v3.AlertPolicyService/": {
			"ListAlertPolicies": {"monitoring.alertPolicies.list", "name"},
			"GetAlertPolicy":    {"monitoring.alertPolicies.get", "name"},
			"CreateAlertPolicy": {"monitoring.alertPolicies.create", "name"},
			"DeleteAlertPolicy": {"monitoring.alertPolicies.delete", "name"},
			"UpdateAlertPolicy": {"monitoring.alertPolicies.update", "alert_policy.name"},
		},
		"/google.monitoring.v3.NotificationChannelService/": {
			"ListNotificationChannelDescriptors":      {"monitoring.notificationChannelDescriptors.list", "name"},
			"GetNotificationChannelDescriptor":        {"monitoring.notificationChannelDescriptors.get", "name"},
			"ListNotificationChannels":                {"monitoring.notificationChannels.list", "name"},
			"GetNotificationChannel":                  {"monitoring.notificationChannels.get", "name"},
			"CreateNotificationChannel":               {"monitoring.notificationChannels.create", "name"},
			"UpdateNotificationChannel":               {"monitoring.notificationChannels.update", "notification_channel.name"},
			"DeleteNotificationChannel":               {"monitoring.notificationChannels.delete", "name"},
			"SendNotificationChannelVerificationCode": {"monitoring.notificationChannels.sendVerificationCode", "name"},
			"GetNotificationChannelVerificationCode":  {"monitoring.notificationChannels.getVerificationCode", "name"},
			"VerifyNotificationChannel":               {"monitoring.notificationChannels.verify", "name"},
		},
		"/google.monitoring.v3.UptimeCheckService/": {
			"ListUptimeCheckConfigs":  {"monitoring.uptimeCheckConfigs.list", "parent"},
			"GetUptimeCheckConfig":    {"monitoring.uptimeCheckConfigs.get", "name"},
			"CreateUptimeCheckConfig": {"monitoring.uptimeCheckConfigs.create", "parent"},
			"UpdateUptimeCheckConfig": {"monitoring.uptimeCheckConfigs.update", "uptime_check_config.name"},
			"DeleteUptimeCheckConfig": {"monitoring.uptimeCheckConfigs.delete", "name"},
		},
		"/google.monitoring.v3.GroupService/": {
			"ListGroups":       {"monitoring.groups.list", "name"},
			"GetGroup":         {"monitoring.groups.get", "name"},
			"CreateGroup":      {"monitoring.groups.create", "name"},
			"UpdateGroup":      {"monitoring.groups.update", "group.name"},
			"DeleteGroup":      {"monitoring.groups.delete", "name"},
			"ListGroupMembers": {"monitoring.groups.get", "name"},
		},
		"/google.monitoring.v3.ServiceMonitoringService/": {
			"CreateService":               {"monitoring.services.create", "parent"},
			"GetService":                  {"monitoring.services.get", "name"},
			"ListServices":                {"monitoring.services.list", "parent"},
			"UpdateService":               {"monitoring.services.update", "service.name"},
			"DeleteService":               {"monitoring.services.delete", "name"},
			"CreateServiceLevelObjective": {"monitoring.slos.create", "parent"},
			"GetServiceLevelObjective":    {"monitoring.slos.get", "name"},
			"ListServiceLevelObjectives":  {"monitoring.slos.list", "parent"},
			"UpdateServiceLevelObjective": {"monitoring.slos.update", "service_level_objective.name"},
			"DeleteServiceLevelObjective": {"monitoring.slos.delete", "name"},
		},
		"/google.monitoring.v3.SnoozeService/": {
			"CreateSnooze": {"monitoring.snoozes.create", "parent"},
			"ListSnoozes":  {"monitoring.snoozes.list", "parent"},
			"GetSnooze":    {"monitoring.snoozes.get", "name"},
			"UpdateSnooze": {"monitoring.snoozes.update", "snooze.name"},
		},
		"/google.monitoring.v3.QueryService/": {
			"QueryTimeSeries": {"monitoring.timeSeries.list", "name"},
		},
		"/google.monitoring.dashboard.v1.DashboardsService/": {
			"CreateDashboard": {"monitoring.dashboards.create", "parent"},
			"ListDashboards":  {"monitoring.dashboards.list", "parent"},
			"GetDashboard":    {"monitoring.dashboards.get", "name"},
			"DeleteDashboard": {"monitoring.dashboards.delete", "name"},
			"UpdateDashboard": {"monitoring.dashboards.update", "dashboard.name"},
		},
	}),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
}

// Run is Cloud Run Admin API v2's table for the checks this round makes:
// a service's, job's or worker pool's template service account needs
// iam.serviceAccounts.actAs on it, "required to attach a service account
// on the service or revision" (and "on the job", "on the worker pool":
// https://docs.cloud.google.com/run/docs/configuring/services/service-identity,
// .../jobs/service-identity, .../workerpools/service-identity, read
// 2026-11-20). Cloud Run's own permissions (run.services.create, ...) are
// not checked yet: their rows have no permission. The denial is the
// troubleshooting page's "Permission 'iam.serviceaccounts.actAs' denied on
// service account E" (https://docs.cloud.google.com/run/docs/troubleshooting).
var Run = &Service{
	Name: "run.googleapis.com",
	Methods: func() map[string]Method {
		m := map[string]Method{}
		for svc, rows := range map[string]map[string][2]string{
			"Services":    {"CreateService": {"parent", "service.template.service_account"}, "UpdateService": {"service.name", "service.template.service_account"}},
			"Jobs":        {"CreateJob": {"parent", "job.template.template.service_account"}, "UpdateJob": {"job.name", "job.template.template.service_account"}},
			"WorkerPools": {"CreateWorkerPool": {"parent", "worker_pool.template.service_account"}, "UpdateWorkerPool": {"worker_pool.name", "worker_pool.template.service_account"}},
		} {
			for name, r := range rows {
				m["/google.cloud.run.v2."+svc+"/"+name] = Method{Field: r[0], ActAs: []string{r[1]}}
			}
		}
		return m
	}(),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
	DenyActAs: func(account, resource string) error {
		return grpcDenied("Permission 'iam.serviceaccounts.actAs' denied on service account "+account, ActAs, resource, true)
	},
}

// Functions is Cloud Functions v2's table for the checks this round
// makes: deployers need "Service Account User role
// (roles/iam.serviceAccountUser) on the runtime service account", and on
// the build's (https://docs.cloud.google.com/functions/docs/reference/iam/roles,
// read 2026-11-20), so CreateFunction and UpdateFunction check
// iam.serviceAccounts.actAs on serviceConfig.serviceAccountEmail and
// buildConfig.serviceAccount (which may be
// projects/{p}/serviceAccounts/{email}). The denial is the
// troubleshooting page's "Caller is missing permission
// 'iam.serviceaccounts.actAs' on service account
// projects/-/serviceAccounts/E." (https://docs.cloud.google.com/functions/docs/troubleshooting;
// its final period UNVERIFIED, re-read 2026-11-20).
var Functions = &Service{
	Name: "cloudfunctions.googleapis.com",
	Methods: map[string]Method{
		"/google.cloud.functions.v2.FunctionService/CreateFunction": {Field: "parent",
			ActAs: []string{"function.service_config.service_account_email", "function.build_config.service_account"}},
		"/google.cloud.functions.v2.FunctionService/UpdateFunction": {Field: "function.name",
			ActAs: []string{"function.service_config.service_account_email", "function.build_config.service_account"}},
		// 1st gen: the function's runtime account and its build's.
		"/google.cloud.functions.v1.CloudFunctionsService/CreateFunction": {Field: "location",
			ActAs: []string{"function.service_account_email", "function.build_service_account"}},
		"/google.cloud.functions.v1.CloudFunctionsService/UpdateFunction": {Field: "function.name",
			ActAs: []string{"function.service_account_email", "function.build_service_account"}},
	},
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on '%s'", permission, resource), permission, resource, false)
	},
	DenyActAs: func(account, resource string) error {
		return status.Error(codes.PermissionDenied, "Caller is missing permission 'iam.serviceaccounts.actAs' on service account projects/-/serviceAccounts/"+account+".")
	},
}

// Eventarc is Eventarc's table for the checks this round makes: a
// trigger's serviceAccount, "The iam.serviceAccounts.actAs permission must
// be granted on the service account to allow a principal to impersonate
// the service account"
// (https://docs.cloud.google.com/eventarc/docs/reference/rest/v1/projects.locations.triggers,
// read 2026-11-20), so CreateTrigger and UpdateTrigger check it.
// Eventarc's own permissions are not checked yet. Google's wording of the
// denial is UNVERIFIED (re-read 2026-11-20: the page gives none):
// newerDeny's, on the account.
var Eventarc = &Service{
	Name: "eventarc.googleapis.com",
	Methods: map[string]Method{
		"/google.cloud.eventarc.v1.Eventarc/CreateTrigger": {Field: "parent", ActAs: []string{"trigger.service_account"}},
		"/google.cloud.eventarc.v1.Eventarc/UpdateTrigger": {Field: "trigger.name", ActAs: []string{"trigger.service_account"}},
	},
	Deny: newerDeny,
}
