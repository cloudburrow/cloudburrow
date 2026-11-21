package main

import (
	"context"
	"net/http"
	"strings"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"

	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/service/siteverification"
)

// alwaysServedQuotaAPIs are APIs with quotas that Service Usage's
// catalogue does not list but the instance always serves: IAM on the
// Resource Manager port, IAM Credentials on the metadata server.
var alwaysServedQuotaAPIs = map[string]bool{"iam.googleapis.com": true, "iamcredentials.googleapis.com": true}

// isCloudQuotasPath matches the Cloud Quotas API v1 (#2030), served on the
// Resource Manager port: {parent}/locations/{l}/quotaPreferences...,
// .../services/{s}/quotaInfos... and .../quotaAdjusterSettings.
func isCloudQuotasPath(r *http.Request) bool {
	p, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok || !strings.Contains(p, "/locations/") {
		return false
	}
	return strings.Contains(p, "/quotaPreferences") || strings.Contains(p, "/quotaInfos") || strings.HasSuffix(p, "/quotaAdjusterSettings")
}

func isSiteVerificationPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, siteverification.Prefix)
}

// serveCloudQuotas mounts the Cloud Quotas API and the Site Verification
// API (#2030) on the Resource Manager port, and makes the quotas the
// services enforce read the granted preferences.
func serveCloudQuotas(rm *resourcemanager.Server, reg *resourcemanager.Registry, su func() *serviceusage.Server, owner string, caller func(string) (string, bool)) (*cloudquotas.Server, *siteverification.Server) {
	served := func(service string) bool {
		if alwaysServedQuotaAPIs[service] {
			return true
		}
		s := su()
		if s == nil {
			return false
		}
		for _, a := range s.Catalog() {
			if a.Name == service {
				return true
			}
		}
		return false
	}
	q := cloudquotas.NewServer(reg.Backing(), serviceUsageProjects{reg: reg}, served)
	rm.HandleService("google.api.cloudquotas.v1", isCloudQuotasPath,
		resourcemanager.Impl{Desc: &pb.CloudQuotas_ServiceDesc, Impl: q},
		resourcemanager.Impl{Desc: &pb.QuotaAdjusterSettingsManager_ServiceDesc, Impl: q.Adjuster()})
	quotalimit.Use(q)
	quotalimit.RegisterUsage(resourcemanager.ServiceAccountQuotaService, resourcemanager.ServiceAccountQuotaID,
		func(project string, _ map[string]string) (int64, error) { return reg.LiveServiceAccounts(project) })
	sv := siteverification.NewServer(reg.Backing(), owner)
	sv.Caller = caller
	rm.HandleJSON(isSiteVerificationPath, sv)
	return q, sv
}

// cloudQuotasResetter forgets each project's quota preferences (#2030).
type cloudQuotasResetter struct{ srv func() *cloudquotas.Server }

func (r *cloudQuotasResetter) Name() string { return "cloudquotas" }

func (r *cloudQuotasResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *cloudQuotasResetter) ResetProject(_ context.Context, project string) error {
	if s := r.srv(); s != nil {
		return s.ResetProject(project)
	}
	return nil
}

// siteVerificationResetter forgets the verified sites and domains.
type siteVerificationResetter struct {
	srv func() *siteverification.Server
}

func (r *siteVerificationResetter) Name() string { return "siteverification" }

func (r *siteVerificationResetter) Reset(context.Context) error {
	if s := r.srv(); s != nil {
		return s.Reset()
	}
	return nil
}

// authPlatformResetter forgets each project's OAuth app and clients.
type authPlatformResetter struct{ store func() *authplatform.Store }

func (r *authPlatformResetter) Name() string { return "authplatform" }

func (r *authPlatformResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *authPlatformResetter) ResetProject(_ context.Context, project string) error {
	if s := r.store(); s != nil {
		return s.ResetProject(project)
	}
	return nil
}
