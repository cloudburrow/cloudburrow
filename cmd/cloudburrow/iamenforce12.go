package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// useIAMEnforcement12DataTransfer checks the BigQuery Data Transfer
// Service's calls, gRPC and JSON (#1879, round 12). The service keeps no
// resource-level policies: the project's and its ancestors' apply.
func useIAMEnforcement12DataTransfer(e *iameval.Enforcer, s *dataTransferSvc) {
	if e == nil || s == nil || e.On == nil || !e.On() {
		return
	}
	s.interpose = append(s.interpose, e.Interceptor(iameval.DataTransfer))
	s.wrapJSON = jsonEnforcer(e, iameval.DataTransfer, datatransfer.Transcoder().Mixins)
}

// tagBucketName is a bucket's resource name in a tag binding,
// "projects/_/buckets/B" (a managed folder or object below it).
var tagBucketName = regexp.MustCompile(`^projects/_/buckets/([^/]+)`)

// useIAMEnforcement12Tags names the project of a bucket a tag binding
// names (#1879, round 12), "projects/_/buckets/B", whose name does not
// show it: storageAddr's bucket's projectNumber, so the project's and its
// ancestors' policies apply to storage.buckets.createTagBinding and the
// other binding permissions.
func useIAMEnforcement12Tags(e *iameval.Enforcer, storageAddr func() string, reg func() *resourcemanager.Registry) {
	if e == nil || e.On == nil || !e.On() {
		return
	}
	if e.Scopes == nil {
		e.Scopes = map[string]func(string) iameval.Scope{}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	src := registrySource{reg: reg}
	e.Scopes["storage.googleapis.com"] = func(resource string) iameval.Scope {
		g := tagBucketName.FindStringSubmatch(resource)
		addr := ""
		if storageAddr != nil {
			addr = storageAddr()
		}
		if g == nil || addr == "" {
			return iameval.Scope{}
		}
		resp, err := client.Get("http://" + addr + "/storage/v1/b/" + url.PathEscape(g[1]) + "?fields=projectNumber")
		if err != nil {
			return iameval.Scope{}
		}
		defer resp.Body.Close()
		var b struct {
			ProjectNumber string `json:"projectNumber"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&b) != nil || b.ProjectNumber == "" {
			return iameval.Scope{}
		}
		project := b.ProjectNumber
		if r := src.registry(); r != nil {
			project = src.projectID(r, project)
		}
		return iameval.Scope{Parent: "projects/" + project}
	}
}

// sourceManagerGit checks a git request to a Secure Source Manager
// repository (#1879, round 12): "To get the permissions that you need to
// push to and pull from a repository", Secure Source Manager Instance
// Accessor on the instance and Repo Reader or Writer on the repository
// ("Access control with IAM", https://docs.cloud.google.com/secure-source-manager/docs/access-control,
// read 2026-11-20), whose permissions are securesourcemanager.instances.access
// on the instance and securesourcemanager.repositories.fetch (a clone or
// fetch) or .push (a push) on the repository. A Developer Connect token
// is its link's to check (developerconnecttokens.go).
func sourceManagerGit(e *iameval.Enforcer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, instance, push, token, link, ok := runadapter.SourceManagerGitRequest(r)
		if !ok || link || e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		perm := "securesourcemanager.repositories.fetch"
		if push {
			perm = "securesourcemanager.repositories.push"
		}
		svc := iameval.SecureSourceManager
		for _, c := range []iameval.Check{{Permission: "securesourcemanager.instances.access", Resource: instance}, {Permission: perm, Resource: repo}} {
			if c.Resource == "" {
				continue
			}
			if err := e.Authorize(r.Context(), svc, token, c.Permission, c.Resource); err != nil {
				iameval.WriteJSONStatus(w, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
