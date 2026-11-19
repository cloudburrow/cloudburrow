package serviceusage

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// services.generateServiceIdentity (Service Usage v1beta1,
// POST v1beta1/{parent=*/*/services/*}:generateServiceIdentity) "Generates
// service identity for service": the service's agent in the project, the
// Google-managed service account the service acts as there. `gcloud
// privateca roots create --kms-key-version/--bucket` calls it for the
// Certificate Authority Service agent and grants it roles on the key and
// the bucket (#1834), and `gcloud beta services identity create` calls it
// directly.
//
// The answer is a done Operation whose response is a ServiceIdentity
// {email, uniqueId}. Google names the agent per service; serviceAgents
// holds the ones Google documents for services CloudBurrow serves, each
// service-PROJECT_NUMBER@DOMAIN. A served service without an entry is 501
// naming it: whether Google gives it an agent, and under which name, is not
// guessed.

// serviceAgents is the service agent's domain by service, from each
// service's own documentation of its service agent.
var serviceAgents = map[string]string{
	"privateca.googleapis.com":        "gcp-sa-privateca.iam.gserviceaccount.com",
	"cloudkms.googleapis.com":         "gcp-sa-cloudkms.iam.gserviceaccount.com",
	"pubsub.googleapis.com":           "gcp-sa-pubsub.iam.gserviceaccount.com",
	"secretmanager.googleapis.com":    "gcp-sa-secretmanager.iam.gserviceaccount.com",
	"sqladmin.googleapis.com":         "gcp-sa-cloud-sql.iam.gserviceaccount.com",
	"logging.googleapis.com":          "gcp-sa-logging.iam.gserviceaccount.com",
	"cloudtasks.googleapis.com":       "gcp-sa-cloudtasks.iam.gserviceaccount.com",
	"cloudscheduler.googleapis.com":   "gcp-sa-cloudscheduler.iam.gserviceaccount.com",
	"artifactregistry.googleapis.com": "gcp-sa-artifactregistry.iam.gserviceaccount.com",
	"run.googleapis.com":              "serverless-robot-prod.iam.gserviceaccount.com",
}

// ServiceAgent is the email of svc's service agent in project, or "" for
// a service with none in serviceAgents.
func ServiceAgent(project, svc string) string {
	d, ok := serviceAgents[svc]
	if !ok {
		return ""
	}
	return fmt.Sprintf("service-%d@%s", resource.ProjectNumber(project), d)
}

// uniqueID is the agent's numeric unique ID: 21 digits, as a service
// account's uniqueId is, the same for the same email every time.
func uniqueID(email string) string {
	h := sha256.Sum256([]byte(email))
	return fmt.Sprintf("1%020d", binary.BigEndian.Uint64(h[:8]))
}

const typePrefixV1beta1 = "type.googleapis.com/google.api.serviceusage.v1beta1."

// routeV1beta1 serves the one v1beta1 method CloudBurrow serves,
// generateServiceIdentity; every other v1beta1 path is 501, unsupported.
func (s *Server) routeV1beta1(r *http.Request, path string) (string, any, error) {
	const method = "serviceusage.services.generateServiceIdentity"
	p := strings.Split(strings.Trim(path, "/"), "/")
	last := p[len(p)-1]
	svc, verb, _ := strings.Cut(last, ":")
	if len(p) != 4 || p[2] != "services" || verb != "generateServiceIdentity" || r.Method != http.MethodPost {
		return "", nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"%s %s: CloudBurrow serves only services.generateServiceIdentity of Service Usage v1beta1; use v1", r.Method, r.URL.Path)
	}
	if p[0] != "projects" {
		return method, nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"generateServiceIdentity for %s: CloudBurrow serves service identities of projects only", p[0]+"/"+p[1])
	}
	if err := decode(r, &struct{}{}); err != nil {
		return method, nil, err
	}
	project, err := s.project(p[1])
	if err != nil {
		return method, nil, err
	}
	if _, err := s.lookup(svc); err != nil {
		return method, nil, err
	}
	email := ServiceAgent(project, svc)
	if email == "" {
		return method, nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"generateServiceIdentity for %s: CloudBurrow does not know this service's agent, so it does not guess one", svc)
	}
	resp := map[string]any{"@type": typePrefixV1beta1 + "ServiceIdentity", "email": email, "uniqueId": uniqueID(email)}
	// Google answers this method with its finished-operation sentinel,
	// not an operation operations.get can read.
	return method, map[string]any{"name": "operations/finished.DONE_OPERATION", "done": true, "response": resp}, nil
}
