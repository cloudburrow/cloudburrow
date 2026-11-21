package cloudquotas

// Quota is one quota of a served API, as Cloud Quotas describes it.
type Quota struct {
	Service           string
	QuotaID           string
	Metric            string
	MetricDisplayName string
	QuotaDisplayName  string
	MetricUnit        string
	// Default is the value Google's quotas page documents for a project.
	Default int64
	// Dimensions are the quota's dimension keys ("region"), empty for a
	// project-wide quota.
	Dimensions []string
	// RefreshInterval is "minute" or "day" for a rate quota, empty for an
	// allocation quota.
	RefreshInterval string
	// Fixed is a limit Google documents as not adjustable: a preference
	// for it is refused.
	Fixed bool
	// Enforced says CloudBurrow refuses calls past the value, through
	// internal/quotalimit, so a granted preference changes what the API
	// allows. A quota that is not enforced is described, not applied.
	Enforced bool
	// Source is the Google page the default and the names were read from.
	Source string
	// Container is the level of the resource hierarchy the quota applies
	// to: ContainerProject (the zero value), ContainerFolder or
	// ContainerOrganization. A folder's or organization's quota counts the
	// usage of everything under it (Google's Cloud Quotas overview), and is
	// read and adjusted under folders/{n} or organizations/{n} only.
	Container Container
}

// Container is a level of the resource hierarchy a quota applies to.
type Container int

// The levels Google documents (QuotaInfo.ContainerType).
const (
	ContainerProject Container = iota
	ContainerFolder
	ContainerOrganization
)

// Collection is the level's collection in a resource name.
func (c Container) Collection() string {
	switch c {
	case ContainerFolder:
		return "folders"
	case ContainerOrganization:
		return "organizations"
	}
	return "projects"
}

// Catalog is every quota CloudBurrow describes (#2030). The defaults and
// display names are Google's quotas pages', read 2026-11-20; Google's docs
// give no quota IDs or metric names for them (only Compute Engine's
// CPUS-per-project-region is documented as an ID), so the IDs follow that
// documented form and are CloudBurrow's (UNVERIFIED, re-read 2026-11-20: https://docs.cloud.google.com/docs/quotas/reference/rest).
var Catalog = []Quota{
	{
		Service: "iam.googleapis.com", QuotaID: "SERVICE-ACCOUNT-COUNT-per-project",
		Metric: "iam.googleapis.com/service_account_count", MetricDisplayName: "Service Account Count",
		QuotaDisplayName: "Service Account Count", MetricUnit: "1/{project}", Default: 100, Enforced: true,
		Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "READ-REQUESTS-per-minute-per-project",
		Metric: "iam.googleapis.com/read_requests", MetricDisplayName: "Read requests",
		QuotaDisplayName: "Read requests per minute", MetricUnit: "1/min/{project}", Default: 6000, RefreshInterval: "minute",
		Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "WRITE-REQUESTS-per-minute-per-project",
		Metric: "iam.googleapis.com/write_requests", MetricDisplayName: "Write requests",
		QuotaDisplayName: "Write requests per minute", MetricUnit: "1/min/{project}", Default: 600, RefreshInterval: "minute",
		Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iamcredentials.googleapis.com", QuotaID: "GENERATE-CREDENTIALS-per-minute-per-project",
		Metric: "iamcredentials.googleapis.com/generate_credentials", MetricDisplayName: "Generate credentials requests",
		QuotaDisplayName: "Generate credentials requests per minute", MetricUnit: "1/min/{project}", Default: 60000, RefreshInterval: "minute",
		Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iamcredentials.googleapis.com", QuotaID: "SIGN-REQUESTS-per-minute-per-project",
		Metric: "iamcredentials.googleapis.com/sign_requests", MetricDisplayName: "Sign JWT or blob requests",
		QuotaDisplayName: "Sign JWT or blob requests per minute", MetricUnit: "1/min/{project}", Default: 60000, RefreshInterval: "minute",
		Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "privateca.googleapis.com", QuotaID: "CAS-per-project-region",
		Metric: "privateca.googleapis.com/certificate_authorities", MetricDisplayName: "Certificate authorities",
		QuotaDisplayName: "CAs per location per project", MetricUnit: "1/{project}/{region}", Default: 1000,
		Dimensions: []string{"region"}, Enforced: true,
		Source: "https://docs.cloud.google.com/certificate-authority-service/quotas",
	},
	{
		Service: "workflows.googleapis.com", QuotaID: "WORKFLOWS-per-project",
		Metric: "workflows.googleapis.com/workflows", MetricDisplayName: "Workflows",
		QuotaDisplayName: "Workflows per project", MetricUnit: "1/{project}", Default: 10000, Fixed: true, Enforced: true,
		Source: "https://docs.cloud.google.com/workflows/quotas",
	},
	// Organization-level quotas (#2102): IAM's quotas page lists these per
	// organization, read 2026-11-21.
	{
		Service: "iam.googleapis.com", QuotaID: WorkforcePoolQuotaID,
		Metric: "iam.googleapis.com/workforce_pools", MetricDisplayName: "Number of workforce identity pools",
		QuotaDisplayName: "Workforce identity pools per organization", MetricUnit: "1/{organization}", Default: 100, Enforced: true,
		Container: ContainerOrganization, Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "WORKFORCE-POOL-CREATE-DELETE-REQUESTS-per-minute-per-organization",
		Metric: "iam.googleapis.com/workforce_pool_create_delete_requests", MetricDisplayName: "Workforce Identity Federation create/delete/undelete requests",
		QuotaDisplayName: "Create/delete/undelete requests per minute per organization", MetricUnit: "1/min/{organization}", Default: 60, RefreshInterval: "minute",
		Container: ContainerOrganization, Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "WORKFORCE-POOL-READ-REQUESTS-per-minute-per-organization",
		Metric: "iam.googleapis.com/workforce_pool_read_requests", MetricDisplayName: "Workforce Identity Federation read requests",
		QuotaDisplayName: "Read requests per minute per organization", MetricUnit: "1/min/{organization}", Default: 120, RefreshInterval: "minute",
		Container: ContainerOrganization, Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "WORKFORCE-POOL-UPDATE-REQUESTS-per-minute-per-organization",
		Metric: "iam.googleapis.com/workforce_pool_update_requests", MetricDisplayName: "Workforce Identity Federation update requests",
		QuotaDisplayName: "Update requests per minute per organization", MetricUnit: "1/min/{organization}", Default: 120, RefreshInterval: "minute",
		Container: ContainerOrganization, Source: "https://docs.cloud.google.com/iam/quotas",
	},
	{
		Service: "iam.googleapis.com", QuotaID: "CUSTOM-ROLES-per-organization",
		Metric: "iam.googleapis.com/organization_custom_roles", MetricDisplayName: "Custom roles for an organization",
		QuotaDisplayName: "Custom roles per organization", MetricUnit: "1/{organization}", Default: 300, Fixed: true, Enforced: true,
		Container: ContainerOrganization, Source: "https://docs.cloud.google.com/iam/quotas",
	},
}

// WorkforcePoolQuotaID is the organization's workforce pool count, the
// limit IAM's CreateWorkforcePool enforces (#2102).
const WorkforcePoolQuotaID = "WORKFORCE-POOL-COUNT-per-organization"
