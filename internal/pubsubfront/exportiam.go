package pubsubfront

// The writing account's roles (#1998), with `up --iam-enforcement=on`, to
// https://docs.cloud.google.com/pubsub/docs/create-bigquery-subscription,
// https://docs.cloud.google.com/pubsub/docs/create-cloudstorage-subscription
// and their troubleshooting pages (read 2026-11-20):
//
//   - an export subscription writes as the Pub/Sub service agent of its
//     project, service-PROJECT_NUMBER@gcp-sa-pubsub.iam.gserviceaccount.com,
//     or as its service_account_email;
//   - that account needs BigQuery Data Editor on the table (or its dataset,
//     or project): the troubleshooting page names bigquery.tables.get and
//     bigquery.tables.updateData; or Storage Object Creator and Storage
//     Legacy Bucket Reader on the bucket (or project): storage.objects.create
//     and storage.buckets.get;
//   - with a service_account_email, the service agent needs
//     iam.serviceAccounts.getAccessToken on that account, and "only users
//     who have the iam.serviceAccounts.actAs permission on the service
//     account can create a subscription";
//   - CreateSubscription and UpdateSubscription without the roles are
//     refused with the troubleshooting pages' "Service account ... is
//     missing permissions required to write to the BigQuery table: ..." and
//     "Cloud Pub/Sub did not have the necessary permissions configured to
//     access the provided bucket ...". Which status code Google gives with
//     them is UNVERIFIED (the pages quote the text alone): FAILED_PRECONDITION
//     here, as for a disabled API;
//   - a subscription whose account loses a role is PERMISSION_DENIED ("The
//     Pub/Sub service account no longer has permission to write to the
//     table"), its messages kept in the backlog, and "the subscription
//     eventually returns to a healthy state" once the role is granted
//     again.
//
// The host's one evaluator (internal/iameval, through internal/iamcheck)
// decides, with the table's own policy and its dataset's access entries,
// or the bucket's policy, and the project's, folders' and organization's;
// a service account's own policy is the host's (Resource Manager keeps it).
// Without enforcement nothing is checked, as before.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// pubsubAgentDomain is the Pub/Sub service agent's domain.
const pubsubAgentDomain = "gcp-sa-pubsub.iam.gserviceaccount.com"

var (
	// bigqueryWriterPermissions are what a BigQuery subscription's account
	// needs on the table, in the troubleshooting page's order.
	bigqueryWriterPermissions = []string{"bigquery.tables.get", "bigquery.tables.updateData"}
	// storageWriterPermissions are Storage Legacy Bucket Reader's
	// storage.buckets.get and Storage Object Creator's storage.objects.create.
	storageWriterPermissions = []string{"storage.buckets.get", "storage.objects.create"}
)

// writerCache is how long a writer check's answer is trusted.
var writerCache = 2 * time.Second

type writerCheck struct {
	at  time.Time
	err error
}

// serviceAccountResource is a service account's policy resource and its
// project: the project of a user-managed account's domain
// (NAME@PROJECT.iam.gserviceaccount.com), else project.
func serviceAccountResource(email, project string) (string, string) {
	if _, domain, ok := strings.Cut(email, "@"); ok {
		if p, isIAM := strings.CutSuffix(domain, ".iam.gserviceaccount.com"); isIAM && !strings.HasPrefix(p, "gcp-sa-") {
			project = p
		}
	}
	return "projects/" + project + "/serviceAccounts/" + email, project
}

// checkActAs refuses a CreateSubscription or UpdateSubscription that sets
// a service_account_email its caller does not hold
// iam.serviceAccounts.actAs on. The instance's own account, or a call
// with no bearer token, holds it (iameval.CheckRemote).
func (f *Front) checkActAs(ctx context.Context, sub, email string) error {
	if !f.iamCheck.Enabled() || email == "" || stateLoad(ctx) {
		return nil
	}
	const perm = "iam.serviceAccounts.actAs"
	res, project := serviceAccountResource(email, projectOf(sub))
	d, err := f.iamCheck.Check(ctx, iamcheck.Request{Bearer: bearerOf(ctx), Service: "iam.googleapis.com", Permissions: []string{perm},
		Resource: res, Type: "iam.googleapis.com/ServiceAccount", Project: project, HostPolicies: []string{res}})
	if err != nil {
		return status.Errorf(codes.Unavailable, "IAM check unavailable: %v", err)
	}
	if d.Unauthenticated {
		return unauthenticated()
	}
	if !d.Holds(perm) {
		// Google's IAM wording for a missing permission on a service
		// account; Pub/Sub's own text for this case is UNVERIFIED.
		return status.Errorf(codes.PermissionDenied, "Permission '%s' denied on service account %s (or it may not exist).", perm, email)
	}
	return nil
}

// checkWriter checks that the account an export subscription writes as
// holds the roles its destination needs. It returns an *errDestination
// in PERMISSION_DENIED, naming what is missing, when it does not; nil when
// it does or enforcement is off.
func (f *Front) checkWriter(ctx context.Context, s *pubsubpb.Subscription) error {
	if !f.iamCheck.Enabled() {
		return nil
	}
	key := configHash(s) + "\x00" + s.GetName()
	f.exp.mu.Lock()
	c, ok := f.exp.writers[key]
	f.exp.mu.Unlock()
	if ok && time.Since(c.at) < writerCache {
		return c.err
	}
	err := f.checkWriterNow(ctx, s)
	if _, isDest := destinationState(err); isDest || err == nil {
		f.exp.mu.Lock()
		if f.exp.writers == nil {
			f.exp.writers = map[string]writerCheck{}
		}
		f.exp.writers[key] = writerCheck{at: time.Now(), err: err}
		f.exp.mu.Unlock()
	}
	return err
}

func (f *Front) checkWriterNow(ctx context.Context, s *pubsubpb.Subscription) error {
	project := projectOf(s.GetName())
	bq := s.GetBigqueryConfig() != nil
	denied := int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED)
	email := s.GetCloudStorageConfig().GetServiceAccountEmail()
	if bq {
		denied = int32(pubsubpb.BigQueryConfig_PERMISSION_DENIED)
		email = s.GetBigqueryConfig().GetServiceAccountEmail()
	}
	writer := iamcheck.Request{Signer: email}
	if email == "" {
		writer = iamcheck.Request{AgentDomain: pubsubAgentDomain, AgentProject: project}
	} else {
		// The service agent mints the account's tokens.
		const perm = "iam.serviceAccounts.getAccessToken"
		res, saProject := serviceAccountResource(email, project)
		req := iamcheck.Request{AgentDomain: pubsubAgentDomain, AgentProject: project, Service: "iam.googleapis.com",
			Permissions: []string{perm}, Resource: res, Type: "iam.googleapis.com/ServiceAccount", Project: saProject, HostPolicies: []string{res}}
		d, err := f.iamCheck.Check(ctx, req)
		if err != nil {
			return err
		}
		if !d.Holds(perm) {
			return &errDestination{denied, fmt.Sprintf("Service account %s is missing permissions required to act as the service account %s: %s.",
				agentName(d, project), email, perm)}
		}
	}
	req := writer
	if bq {
		t, err := parseTable(s.GetBigqueryConfig().GetTable())
		if err != nil {
			return err
		}
		own, err := f.exp.tablePolicies(ctx, t)
		if err != nil {
			return err
		}
		req.Service, req.Permissions, req.Own = "bigquery.googleapis.com", bigqueryWriterPermissions, own
		req.Resource = "projects/" + t.project + "/datasets/" + t.dataset + "/tables/" + t.table
		req.Type, req.Project = "bigquery.googleapis.com/Table", t.project
		d, err := f.iamCheck.Check(ctx, req)
		if err != nil {
			return err
		}
		if missing := notHeld(d, bigqueryWriterPermissions); len(missing) > 0 {
			return &errDestination{denied, fmt.Sprintf("Service account %s is missing permissions required to write to the BigQuery table: %s.",
				writerName(d, email, project), strings.Join(missing, ", "))}
		}
		return nil
	}
	bucket := s.GetCloudStorageConfig().GetBucket()
	own, bucketProject, err := f.exp.bucketPolicy(ctx, bucket)
	if err != nil {
		return err
	}
	req.Service, req.Permissions, req.Own = "storage.googleapis.com", storageWriterPermissions, own
	req.Resource, req.Type, req.Project = "projects/_/buckets/"+bucket, "storage.googleapis.com/Bucket", bucketProject
	d, err := f.iamCheck.Check(ctx, req)
	if err != nil {
		return err
	}
	if len(notHeld(d, storageWriterPermissions)) > 0 {
		return &errDestination{denied, fmt.Sprintf("Cloud Pub/Sub did not have the necessary permissions configured to access the provided bucket %s "+
			"(or the bucket may not exist). Please verify that the service account %s was granted the Storage Legacy Bucket Reader "+
			"and Storage Object Creator roles for the provided bucket.", bucket, writerName(d, email, project))}
	}
	return nil
}

func notHeld(d iamcheck.Decision, perms []string) []string {
	var out []string
	for _, p := range perms {
		if !d.Holds(p) {
			out = append(out, p)
		}
	}
	return out
}

// agentName is the service agent's email as the host named it.
func agentName(d iamcheck.Decision, project string) string {
	if d.Email != "" {
		return d.Email
	}
	return "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@" + pubsubAgentDomain
}

func writerName(d iamcheck.Decision, email, project string) string {
	if email != "" {
		return email
	}
	return agentName(d, project)
}

// getJSON reads a JSON resource of the instance's BigQuery or Cloud
// Storage into v; found is false on 404.
func (e *exporter) getJSON(ctx context.Context, method, u string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return false, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(`{"options":{"requestedPolicyVersion":3}}`))
	}
	resp, err := e.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode/100 != 2:
		return false, fmt.Errorf("%s %s: %s", method, u, resp.Status)
	}
	return true, json.Unmarshal(body, v)
}

// datasetAccessRoles are a dataset access entry's basic roles, as
// BigQuery maps them (https://cloud.google.com/bigquery/docs/access-control-basic-roles).
var datasetAccessRoles = map[string]string{
	"READER": "roles/bigquery.dataViewer",
	"WRITER": "roles/bigquery.dataEditor",
	"OWNER":  "roles/bigquery.dataOwner",
}

// tablePolicies are a table's own policy and its dataset's access entries
// as a policy, the table's first.
func (e *exporter) tablePolicies(ctx context.Context, t bqTable) ([]iamcheck.Policy, error) {
	base := e.bigquery + "/bigquery/v2/projects/" + url.PathEscape(t.project) + "/datasets/" + url.PathEscape(t.dataset)
	var tp policyJSON
	if _, err := e.getJSON(ctx, http.MethodPost, e.tableURL(t)+":getIamPolicy", &tp); err != nil {
		return nil, err
	}
	var ds struct {
		Access []struct {
			Role         string `json:"role"`
			UserByEmail  string `json:"userByEmail"`
			GroupByEmail string `json:"groupByEmail"`
			Domain       string `json:"domain"`
			SpecialGroup string `json:"specialGroup"`
			IAMMember    string `json:"iamMember"`
			Condition    *struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Expression  string `json:"expression"`
			} `json:"condition"`
		} `json:"access"`
	}
	if _, err := e.getJSON(ctx, http.MethodGet, base, &ds); err != nil {
		return nil, err
	}
	dp := &iampolicy.Stored{}
	for _, a := range ds.Access {
		role := a.Role
		if r, ok := datasetAccessRoles[role]; ok {
			role = r
		}
		var member string
		switch {
		case a.UserByEmail != "":
			member = "user:" + a.UserByEmail
			if strings.HasSuffix(strings.ToLower(a.UserByEmail), ".gserviceaccount.com") {
				member = "serviceAccount:" + a.UserByEmail
			}
		case a.GroupByEmail != "":
			member = "group:" + a.GroupByEmail
		case a.Domain != "":
			member = "domain:" + a.Domain
		case a.IAMMember != "":
			member = a.IAMMember
		case a.SpecialGroup == "allAuthenticatedUsers":
			member = "allAuthenticatedUsers"
		}
		if role == "" || member == "" {
			continue
		}
		b := iampolicy.Binding{Role: role, Members: []string{member}}
		if a.Condition != nil {
			b.Condition = &iampolicy.Condition{Title: a.Condition.Title, Description: a.Condition.Description, Expression: a.Condition.Expression}
		}
		dp.Bindings = append(dp.Bindings, b)
	}
	return []iamcheck.Policy{
		{Resource: "//bigquery.googleapis.com/projects/" + t.project + "/datasets/" + t.dataset + "/tables/" + t.table, Policy: tp.stored()},
		{Resource: "//bigquery.googleapis.com/projects/" + t.project + "/datasets/" + t.dataset, Policy: dp},
	}, nil
}

// bucketPolicy is a bucket's own policy and its project's number.
func (e *exporter) bucketPolicy(ctx context.Context, bucket string) ([]iamcheck.Policy, string, error) {
	base := e.storage + "/storage/v1/b/" + url.PathEscape(bucket)
	var b struct {
		ProjectNumber json.RawMessage `json:"projectNumber"`
	}
	if _, err := e.getJSON(ctx, http.MethodGet, base, &b); err != nil {
		return nil, "", err
	}
	var p policyJSON
	if _, err := e.getJSON(ctx, http.MethodGet, base+"/iam?optionsRequestedPolicyVersion=3", &p); err != nil {
		return nil, "", err
	}
	return []iamcheck.Policy{{Resource: "//storage.googleapis.com/projects/_/buckets/" + bucket, Policy: p.stored()}},
		strings.Trim(string(b.ProjectNumber), `"`), nil
}

// policyJSON is an IAM policy's bindings as BigQuery and Cloud Storage
// answer getIamPolicy.
type policyJSON struct {
	Bindings []iampolicy.Binding `json:"bindings"`
}

func (p policyJSON) stored() *iampolicy.Stored { return &iampolicy.Stored{Bindings: p.Bindings} }
