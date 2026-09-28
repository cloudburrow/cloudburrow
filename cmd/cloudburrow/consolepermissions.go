package main

// The Permissions tab (#793) on a Cloud Tasks queue, a Secret Manager secret,
// a Cloud KMS key ring or key, a Cloud Storage bucket and a Cloud Storage
// managed folder (#847).
//
// Each provider reads and writes the policy through its service's own
// GetIamPolicy and SetIamPolicy: the in-process gRPC servers for Cloud Tasks,
// Secret Manager and Cloud KMS, and the official storage client's bucket IAM
// handle for Cloud Storage, the calls an SDK makes. The console package does
// the read-modify-write and sends the etag the page read, so a stale one is
// refused by the service with its own message.
//
// What a service accepts is what the form accepts. All four store any
// principal string as given and any non-empty role: none checks a member's
// format or a role against Google's list, so the form offers free text and
// says so, rather than a list of roles it would be inventing, or a check the
// API does not make.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// compatibilityDoc is the compatibility page, whose rows say what each
// service stores and that nothing is enforced.
const compatibilityDoc = "https://github.com/cloudburrow/cloudburrow/blob/main/docs/compatibility.md#"

// roleHelp is the role field's help: an example of a role Google defines for
// the service, and what this service actually checks.
func roleHelp(service, example string) string {
	return fmt.Sprintf("A role name, such as %s. %s here stores any role that is not empty, "+
		"and does not check it against Google's roles.", example, service)
}

// policyFromProto is an iampb.Policy as the console shows it, with its etag
// base64-encoded as the JSON API spells it.
func policyFromProto(p *iampb.Policy) console.Policy {
	out := console.Policy{Etag: base64.StdEncoding.EncodeToString(p.GetEtag()), Bindings: []console.Binding{}}
	for _, b := range p.GetBindings() {
		out.Bindings = append(out.Bindings, console.Binding{
			Role: b.GetRole(), Members: append([]string(nil), b.GetMembers()...), Condition: conditionText(b.GetCondition()),
		})
	}
	return out
}

func conditionText(c *expr.Expr) string {
	switch {
	case c == nil:
		return ""
	case c.GetTitle() != "":
		return c.GetTitle() + ": " + c.GetExpression()
	}
	return c.GetExpression()
}

// policyToProto is the policy to send with SetIamPolicy, carrying etag.
func policyToProto(p console.Policy, etag []byte) (*iampb.Policy, error) {
	out := &iampb.Policy{Version: 1, Etag: etag}
	for _, b := range p.Bindings {
		// None of these services stores a condition, so none is read back;
		// one that were would be lost by a binding written without it.
		if b.Condition != "" {
			return nil, fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		out.Bindings = append(out.Bindings, &iampb.Binding{Role: b.Role, Members: append([]string(nil), b.Members...)})
	}
	return out, nil
}

// protoEtag decodes an etag policyFromProto encoded.
func protoEtag(p console.Policy) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(p.Etag)
	if err != nil {
		return nil, fmt.Errorf("the etag %q is not one this console read", p.Etag)
	}
	return b, nil
}

// iamServer is the GetIamPolicy and SetIamPolicy pair of a gRPC service.
type iamServer interface {
	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)
}

func getProtoPolicy(ctx context.Context, api iamServer, resource string) (console.Policy, error) {
	p, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		return console.Policy{}, err
	}
	return policyFromProto(p), nil
}

func setProtoPolicy(ctx context.Context, api iamServer, resource string, p console.Policy) error {
	etag, err := protoEtag(p)
	if err != nil {
		return err
	}
	pol, err := policyToProto(p, etag)
	if err != nil {
		return err
	}
	_, err = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
	return err
}

// Cloud Tasks: a queue's page.

func (tasksProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "cloud-tasks--googlecloudtasksv2",
		RoleHelp: roleHelp("Cloud Tasks", "roles/cloudtasks.enqueuer")}
}

func (p tasksProvider) queueIAM(project string, path []string) (iamServer, string, error) {
	api := p.api()
	if api == nil {
		return nil, "", errors.New("Cloud Tasks has not started")
	}
	if err := tasksInProject(project, path[0]); err != nil {
		return nil, "", err
	}
	return api, path[0], nil
}

func (p tasksProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, queue, err := p.queueIAM(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, queue)
}

func (p tasksProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, queue, err := p.queueIAM(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, queue, pol)
}

// Secret Manager: a secret's page. A version has no policy of its own.

func (secretsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "secret-manager--googlecloudsecretmanagerv1",
		RoleHelp: roleHelp("Secret Manager", "roles/secretmanager.secretAccessor")}
}

func (p secretsProvider) secretIAM(project string, path []string) (iamServer, string, error) {
	st := p.svc.Store()
	if st == nil {
		return nil, "", errors.New("Secret Manager has not started")
	}
	if project == "" {
		return nil, "", errors.New("choose a project first")
	}
	return secrets.NewGRPCServer(st), "projects/" + project + "/secrets/" + lastSegment(path[0]), nil
}

func (p secretsProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.secretIAM(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p secretsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.secretIAM(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

// Cloud KMS: a key ring's page and a key's. Cloud KMS keeps a policy on each,
// and a key's is its own, not the ring's; a version has none.

func (kmsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 && len(path) != 2 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "cloud-kms--googlecloudkmsv1",
		RoleHelp: roleHelp("Cloud KMS", "roles/cloudkms.cryptoKeyEncrypterDecrypter")}
}

func (p kmsProvider) kmsIAM(project string, path []string) (iamServer, string, error) {
	api := p.api()
	if api == nil {
		return nil, "", errors.New(kmsNotStarted)
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return nil, "", err
	}
	if len(path) == 2 {
		return api.IAMPolicy(), ring + "/cryptoKeys/" + path[1], nil
	}
	return api.IAMPolicy(), ring, nil
}

func (p kmsProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.kmsIAM(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p kmsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.kmsIAM(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

// Cloud Storage: a bucket's page (its root) through the official client's
// bucket IAM handle, storage.buckets.getIamPolicy and setIamPolicy, and a
// managed folder's page (#847), the folder page of a prefix that is one,
// through managedFolders.getIamPolicy and setIamPolicy on Google's generated
// JSON API client, since the official client has no managed folder call. A
// plain folder, an object and the other pages have no policy. The JSON API's
// etag is a string, which the official client carries as its bytes, so it is
// shown and sent back as it is.

func (p storageProvider) PolicyOn(ctx context.Context, path []string) *console.PolicyTarget {
	target := &console.PolicyTarget{Link: compatibilityDoc + "cloud-storage--json-api-v1",
		RoleHelp: roleHelp("Cloud Storage", "roles/storage.objectViewer")}
	switch {
	case len(path) == 0 || strings.HasPrefix(path[0], "_"):
		return nil
	case len(path) == 1:
		return target
	}
	if _, _, ok := notificationPath(path); ok || !p.isManagedFolder(ctx, path[0], folderName(path)) {
		return nil
	}
	return target
}

// folderName is the prefix a folder page shows, ending in "/": the managed
// folder's name when the folder is one.
func folderName(path []string) string { return strings.Join(path[1:], "/") + "/" }

// isManagedFolder says whether managedFolders.get finds name in bucket.
func (p storageProvider) isManagedFolder(ctx context.Context, bucket, name string) bool {
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return false
	}
	_, err = s.ManagedFolders.Get(bucket, name).Context(ctx).Do()
	return err == nil
}

func (p storageProvider) bucketIAM(ctx context.Context, path []string) (*iam.Handle, func(), error) {
	c, err := p.storageClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c.Bucket(path[0]).IAM(), func() { _ = c.Close() }, nil
}

func (p storageProvider) GetPolicy(ctx context.Context, _ string, path []string) (console.Policy, error) {
	if len(path) > 1 {
		return p.managedFolderPolicy(ctx, path)
	}
	h, done, err := p.bucketIAM(ctx, path)
	if err != nil {
		return console.Policy{}, err
	}
	defer done()
	pol, err := h.Policy(ctx)
	if err != nil {
		return console.Policy{}, err
	}
	out := policyFromProto(pol.InternalProto)
	out.Etag = string(pol.InternalProto.GetEtag())
	return out, nil
}

func (p storageProvider) SetPolicy(ctx context.Context, _ string, path []string, pol console.Policy) error {
	if len(path) > 1 {
		return p.setManagedFolderPolicy(ctx, path, pol)
	}
	h, done, err := p.bucketIAM(ctx, path)
	if err != nil {
		return err
	}
	defer done()
	proto, err := policyToProto(pol, []byte(pol.Etag))
	if err != nil {
		return err
	}
	return h.SetPolicy(ctx, &iam.Policy{InternalProto: proto})
}

// managedFolderPolicy is managedFolders.getIamPolicy for a folder page.
func (p storageProvider) managedFolderPolicy(ctx context.Context, path []string) (console.Policy, error) {
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return console.Policy{}, err
	}
	pol, err := s.ManagedFolders.GetIamPolicy(path[0], folderName(path)).Context(ctx).Do()
	if err != nil {
		return console.Policy{}, err
	}
	out := console.Policy{Etag: pol.Etag, Bindings: []console.Binding{}}
	for _, b := range pol.Bindings {
		out.Bindings = append(out.Bindings, console.Binding{
			Role: b.Role, Members: append([]string(nil), b.Members...), Condition: jsonConditionText(b.Condition),
		})
	}
	return out, nil
}

func jsonConditionText(c *storagev1.Expr) string {
	switch {
	case c == nil:
		return ""
	case c.Title != "":
		return c.Title + ": " + c.Expression
	}
	return c.Expression
}

// setManagedFolderPolicy is managedFolders.setIamPolicy with the etag the
// page read. As for every service here, a condition is refused rather than
// dropped.
func (p storageProvider) setManagedFolderPolicy(ctx context.Context, path []string, pol console.Policy) error {
	out := &storagev1.Policy{Version: 1, Etag: pol.Etag}
	for _, b := range pol.Bindings {
		if b.Condition != "" {
			return fmt.Errorf("the binding for %s has a condition, which the console cannot write back", b.Role)
		}
		out.Bindings = append(out.Bindings, &storagev1.PolicyBindings{Role: b.Role, Members: append([]string(nil), b.Members...)})
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return err
	}
	_, err = s.ManagedFolders.SetIamPolicy(path[0], folderName(path), out).Context(ctx).Do()
	return err
}

var (
	_ console.PolicyEditor = tasksProvider{}
	_ console.PolicyEditor = secretsProvider{}
	_ console.PolicyEditor = kmsProvider{}
	_ console.PolicyEditor = storageProvider{}
)
