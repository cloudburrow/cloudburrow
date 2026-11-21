package parametermanager

import (
	"context"

	"github.com/cloudburrow/cloudburrow/internal/apienable"
	"github.com/cloudburrow/cloudburrow/internal/ratequota"
)

// Request quotas (#2063), per project per minute, as the Secret Manager
// quotas page documents them for Parameter Manager
// (docs.cloud.google.com/secret-manager/quotas, read 2026-11-20):
//
//   - Render requests, 1,800: "calls to the render API method",
//     RenderParameterVersion and RenderTemplateVersion;
//   - Read requests, 1,200: the non-mutating calls other than render, Get
//     and List of parameters, versions, templates and template versions;
//   - Write requests, 1,200: the mutating calls, Create, Update and Delete.
//
// "Shared across all applications and IP addresses using that project": a
// call is charged to the project its resource is in. The metric and limit
// names are UNVERIFIED (docs re-read 2026-11-20:
// https://docs.cloud.google.com/secret-manager/quotas, which gives none): they follow
// error_reason.proto's example, "<service>/read_requests" and
// "ReadRequestsPerMinutePerProject". The Locations mixin is not charged.

// Service is Parameter Manager's API name.
const Service = "parametermanager.googleapis.com"

const (
	quotaRead   = Service + "/read_requests"
	quotaWrite  = Service + "/write_requests"
	quotaRender = Service + "/render_requests"
)

// quotasPage documents Parameter Manager's quotas.
const quotasPage = "https://docs.cloud.google.com/secret-manager/quotas"

// Quotas are Parameter Manager's request quotas, at Google's defaults.
func Quotas() []ratequota.Metric {
	return []ratequota.Metric{
		{Service: Service, Name: quotaRead, Title: "Read requests", Limit: "ReadRequestsPerMinutePerProject",
			LimitTitle: "Read requests per minute per project", Default: 1200, Source: quotasPage},
		{Service: Service, Name: quotaWrite, Title: "Write requests", Limit: "WriteRequestsPerMinutePerProject",
			LimitTitle: "Write requests per minute per project", Default: 1200, Source: quotasPage},
		{Service: Service, Name: quotaRender, Title: "Render requests", Limit: "RenderRequestsPerMinutePerProject",
			LimitTitle: "Render requests per minute per project", Default: 1800, Source: quotasPage},
	}
}

// charge counts a call to metric in project.
func (s *Server) charge(_ context.Context, metric, project string) error {
	return s.opts.Quotas.Charge(metric, project)
}

// chargeName counts a call to metric in the project name is in.
func (s *Server) chargeName(ctx context.Context, metric, name string) error {
	return s.charge(ctx, metric, apienable.ProjectOfName(name))
}
