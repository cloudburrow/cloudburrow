// Package iamcheck is how the servers CloudBurrow runs in the cluster
// (Cloud Storage, Pub/Sub's front) ask the host's one evaluator,
// internal/iameval, whether a request's caller holds a permission (#1879),
// so the decision is made in one place whatever the server: the server
// sends the caller's bearer token, the permissions, the resource and its
// own policies (a bucket's, a topic's), and the host names the caller from
// the token it minted and evaluates those policies with the resource's
// project, folders and organization. Only `up --iam-enforcement=on` gives a
// server the host's address; without one nothing is checked.
package iamcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Path is the host's check endpoint, on the Resource Manager port the
// cluster reaches through cloudburrow-host.
const Path = "/cloudburrow/v1/iam:check"

// Policy is one of the resource's own policies, the resource's first.
type Policy struct {
	// Resource is the policy's full resource name
	// ("//storage.googleapis.com/projects/_/buckets/b").
	Resource string            `json:"resource"`
	Policy   *iampolicy.Stored `json:"policy,omitempty"`
}

// Request is one check.
type Request struct {
	// Bearer is the request's bearer token, "" for none.
	Bearer string `json:"bearer,omitempty"`
	// Signer is the account whose signature the server verified (a
	// Cloud Storage signed URL's, #1887), checked in the bearer's place:
	// Google checks a signed URL against its signer's permissions.
	Signer string `json:"signer,omitempty"`
	// AgentDomain names a service agent checked in the bearer's place,
	// service-PROJECT_NUMBER@AgentDomain of the project AgentProject, which
	// the host numbers as its registry does (#1998: a Pub/Sub export
	// subscription writes as service-N@gcp-sa-pubsub.iam.gserviceaccount.com).
	AgentDomain  string `json:"agentDomain,omitempty"`
	AgentProject string `json:"agentProject,omitempty"`
	// HostPolicies are hierarchy resources whose stored policies the host
	// holds and adds to Own, the resource's own first: a service account's
	// ("projects/p/serviceAccounts/e"), which Resource Manager keeps
	// (#1998).
	HostPolicies []string `json:"hostPolicies,omitempty"`
	// Anonymous makes a request with no bearer token allUsers alone,
	// rather than the instance's own account (Cloud Run invoke).
	Anonymous bool `json:"anonymous,omitempty"`
	// Service is the API's host ("storage.googleapis.com").
	Service     string   `json:"service"`
	Permissions []string `json:"permissions"`
	// Resource is the checked resource's name as conditions read it
	// (resource.name), Type its resource.type.
	Resource string `json:"resource"`
	Type     string `json:"type,omitempty"`
	// Project is the project (ID or number) whose ancestry applies.
	Project string   `json:"project,omitempty"`
	Own     []Policy `json:"own,omitempty"`
}

// Decision is the host's answer.
type Decision struct {
	// Held are the requested permissions the caller holds.
	Held []string `json:"held"`
	// Owner is the instance's own account (or no bearer token without
	// Anonymous): every permission.
	Owner bool `json:"owner,omitempty"`
	// Unauthenticated is a bearer token the instance did not mint.
	Unauthenticated bool `json:"unauthenticated,omitempty"`
	// Email is the caller's account, "" for an anonymous caller.
	Email string `json:"email,omitempty"`
	// Members are the caller's policy members (user:, serviceAccount:,
	// group:, domain:, allAuthenticatedUsers, allUsers), lower-cased, for
	// a server's ACLs.
	Members []string `json:"members,omitempty"`
	// Teams are the caller's project teams on Project, for Cloud
	// Storage's project-owners-, project-editors- and project-viewers-
	// ACL entities (#1887): "owners", "editors" and "viewers", for the
	// basic roles Owner, Editor and Viewer it holds on the project.
	Teams []string `json:"teams,omitempty"`
}

// Holds reports whether d grants permission.
func (d Decision) Holds(permission string) bool {
	for _, p := range d.Held {
		if p == permission {
			return true
		}
	}
	return false
}

// Client asks the host at Base ("http://host:port").
type Client struct {
	Base   string
	Client *http.Client
}

// Enabled reports whether c checks anything: a nil client or one with no
// address is enforcement off.
func (c *Client) Enabled() bool { return c != nil && c.Base != "" }

var defaultClient = &http.Client{Timeout: 10 * time.Second}

// Check asks the host. An error is the host not answering, which the
// caller refuses as Google refuses when it cannot check (UNAVAILABLE).
func (c *Client) Check(ctx context.Context, req Request) (Decision, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Base, "/")+Path, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hc := c.Client
	if hc == nil {
		hc = defaultClient
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return Decision{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("iam check: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, fmt.Errorf("iam check: %w", err)
	}
	return d, nil
}

// Bearer is an Authorization header's bearer token, "" for none.
func Bearer(header string) string {
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
