package iameval

import (
	"fmt"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Method is one served method's permission check: the permission Google's
// reference documents for it, and the request field naming the resource it
// is checked on (the resource itself, or, for a list or a create, its
// parent).
type Method struct {
	Permission string
	// Field is the request's field holding the resource name, a dotted
	// path for a nested message ("secret.name").
	Field string
	// PermissionFor, when set, picks the permission by the resource
	// (the IAMPolicy mixin, whose permission is the resource's type's).
	PermissionFor func(resource string) string
}

// Service is one API's table: its methods by gRPC full method, the
// collections whose resources keep their own policy, and how Google
// words a denial.
type Service struct {
	// Name is the service's host, "secretmanager.googleapis.com".
	Name string
	// Methods are keyed by gRPC full method
	// ("/google.cloud.secretmanager.v1.SecretManagerService/GetSecret").
	Methods map[string]Method
	// PolicyCollections are the collections whose resources have their
	// own IAM policy ("secrets"), parent first.
	PolicyCollections []string
	// Types are resource.type by a name's last collection.
	Types map[string]string
	// Deny is the error a denied call gets.
	Deny func(permission, resource string) error
}

// Type is resource.type for a resource name.
func (s *Service) Type(resource string) string {
	parts := strings.Split(resource, "/")
	if len(parts) == 2 && parts[0] == "projects" {
		return "cloudresourcemanager.googleapis.com/Project"
	}
	for i := len(parts) - 2; i >= 0; i -= 2 {
		if t, ok := s.Types[parts[i]]; ok {
			return t
		}
	}
	return ""
}

// PolicyResources are the resources, the deepest first, of name and its
// parents that keep their own policy in this service.
func (s *Service) PolicyResources(name string) []string {
	parts := strings.Split(name, "/")
	var out []string
	for i := len(parts) - 2; i >= 0; i -= 2 {
		for _, c := range s.PolicyCollections {
			if parts[i] == c && i+1 < len(parts) && parts[i+1] != "" && parts[i+1] != "-" {
				out = append(out, strings.Join(parts[:i+2], "/"))
			}
		}
	}
	return out
}

// Project is the project segment of a resource name, "" for none.
func Project(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "projects" {
			return parts[i+1]
		}
	}
	return ""
}

// grpcDenied is PERMISSION_DENIED with msg and, when withInfo, the
// ErrorInfo Google's newer APIs attach: reason IAM_PERMISSION_DENIED,
// domain iam.googleapis.com, the permission and resource in its metadata.
func grpcDenied(msg, permission, resource string, withInfo bool) error {
	st := status.New(codes.PermissionDenied, msg)
	if withInfo {
		if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: "IAM_PERMISSION_DENIED", Domain: "iam.googleapis.com",
			Metadata: map[string]string{"permission": permission, "resource": resource}}); err == nil {
			st = d
		}
	}
	return st.Err()
}

// Services are the APIs whose methods are enforced (#1879), by the gRPC
// service name the methods' full names begin with. A method not listed is
// not enforced: docs/compatibility.md's "IAM enforcement" lists what is.
var Services = map[string]*Service{
	"google.cloud.secretmanager.v1.SecretManagerService": SecretManager,
	"google.cloud.kms.v1.KeyManagementService":           KMS,
	"google.cloud.tasks.v2.CloudTasks":                   Tasks,
}

// ForMethod is the service and method row of a gRPC full method on the
// service whose port it arrived at (the IAMPolicy mixin is shared by
// name), nil for an unenforced method.
func ForMethod(port *Service, fullMethod string) (*Service, *Method) {
	if port == nil {
		return nil, nil
	}
	if m, ok := port.Methods[fullMethod]; ok {
		return port, &m
	}
	return nil, nil
}

// SecretManager is Secret Manager's table: "Access control with IAM"
// (https://docs.cloud.google.com/secret-manager/docs/access-control, read
// 2026-11-19) and each method's reference. Google words a denial
// "Permission 'P' denied for resource 'R' (or it may not exist).", with
// ErrorInfo IAM_PERMISSION_DENIED.
var SecretManager = &Service{
	Name:              "secretmanager.googleapis.com",
	PolicyCollections: []string{"secrets"},
	Types: map[string]string{"secrets": "secretmanager.googleapis.com/Secret",
		"versions": "secretmanager.googleapis.com/SecretVersion"},
	Methods: sm(map[string][2]string{
		"ListSecrets":          {"secretmanager.secrets.list", "parent"},
		"CreateSecret":         {"secretmanager.secrets.create", "parent"},
		"GetSecret":            {"secretmanager.secrets.get", "name"},
		"UpdateSecret":         {"secretmanager.secrets.update", "secret.name"},
		"DeleteSecret":         {"secretmanager.secrets.delete", "name"},
		"AddSecretVersion":     {"secretmanager.versions.add", "parent"},
		"ListSecretVersions":   {"secretmanager.versions.list", "parent"},
		"GetSecretVersion":     {"secretmanager.versions.get", "name"},
		"AccessSecretVersion":  {"secretmanager.versions.access", "name"},
		"DisableSecretVersion": {"secretmanager.versions.disable", "name"},
		"EnableSecretVersion":  {"secretmanager.versions.enable", "name"},
		"DestroySecretVersion": {"secretmanager.versions.destroy", "name"},
		"GetIamPolicy":         {"secretmanager.secrets.getIamPolicy", "resource"},
		"SetIamPolicy":         {"secretmanager.secrets.setIamPolicy", "resource"},
	}, "/google.cloud.secretmanager.v1.SecretManagerService/"),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied for resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
}

// KMS is Cloud KMS's table: "Permissions and roles"
// (https://docs.cloud.google.com/kms/docs/reference/permissions-and-roles,
// read 2026-11-19). Google words a denial "Permission 'P' denied on
// resource 'R' (or it may not exist).", with ErrorInfo
// IAM_PERMISSION_DENIED. A key's policy and its key ring's both apply to
// the key and its versions.
var KMS = &Service{
	Name:              "cloudkms.googleapis.com",
	PolicyCollections: []string{"keyRings", "cryptoKeys"},
	Types: map[string]string{"keyRings": "cloudkms.googleapis.com/KeyRing", "cryptoKeys": "cloudkms.googleapis.com/CryptoKey",
		"cryptoKeyVersions": "cloudkms.googleapis.com/CryptoKeyVersion", "importJobs": "cloudkms.googleapis.com/ImportJob"},
	Methods: kmsMethods(),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
}

func kmsMethods() map[string]Method {
	m := sm(map[string][2]string{
		"ListKeyRings":                  {"cloudkms.keyRings.list", "parent"},
		"GetKeyRing":                    {"cloudkms.keyRings.get", "name"},
		"CreateKeyRing":                 {"cloudkms.keyRings.create", "parent"},
		"ListCryptoKeys":                {"cloudkms.cryptoKeys.list", "parent"},
		"GetCryptoKey":                  {"cloudkms.cryptoKeys.get", "name"},
		"CreateCryptoKey":               {"cloudkms.cryptoKeys.create", "parent"},
		"UpdateCryptoKey":               {"cloudkms.cryptoKeys.update", "crypto_key.name"},
		"UpdateCryptoKeyPrimaryVersion": {"cloudkms.cryptoKeys.update", "name"},
		"ListCryptoKeyVersions":         {"cloudkms.cryptoKeyVersions.list", "parent"},
		"GetCryptoKeyVersion":           {"cloudkms.cryptoKeyVersions.get", "name"},
		"CreateCryptoKeyVersion":        {"cloudkms.cryptoKeyVersions.create", "parent"},
		"UpdateCryptoKeyVersion":        {"cloudkms.cryptoKeyVersions.update", "crypto_key_version.name"},
		"DestroyCryptoKeyVersion":       {"cloudkms.cryptoKeyVersions.destroy", "name"},
		"RestoreCryptoKeyVersion":       {"cloudkms.cryptoKeyVersions.restore", "name"},
		"GetPublicKey":                  {"cloudkms.cryptoKeyVersions.viewPublicKey", "name"},
		"Encrypt":                       {"cloudkms.cryptoKeyVersions.useToEncrypt", "name"},
		"Decrypt":                       {"cloudkms.cryptoKeyVersions.useToDecrypt", "name"},
		"AsymmetricSign":                {"cloudkms.cryptoKeyVersions.useToSign", "name"},
		"AsymmetricDecrypt":             {"cloudkms.cryptoKeyVersions.useToDecrypt", "name"},
		"MacSign":                       {"cloudkms.cryptoKeyVersions.useToSign", "name"},
		"MacVerify":                     {"cloudkms.cryptoKeyVersions.useToVerify", "name"},
		"RawEncrypt":                    {"cloudkms.cryptoKeyVersions.useToEncrypt", "name"},
		"RawDecrypt":                    {"cloudkms.cryptoKeyVersions.useToDecrypt", "name"},
		"ListImportJobs":                {"cloudkms.importJobs.list", "parent"},
		"GetImportJob":                  {"cloudkms.importJobs.get", "name"},
		"CreateImportJob":               {"cloudkms.importJobs.create", "parent"},
	}, "/google.cloud.kms.v1.KeyManagementService/")
	iam := func(verb string) func(string) string {
		return func(resource string) string {
			kind := "keyRings"
			switch {
			case strings.Contains(resource, "/importJobs/"):
				kind = "importJobs"
			case strings.Contains(resource, "/cryptoKeys/"):
				kind = "cryptoKeys"
			}
			return "cloudkms." + kind + "." + verb
		}
	}
	m["/google.iam.v1.IAMPolicy/GetIamPolicy"] = Method{Field: "resource", PermissionFor: iam("getIamPolicy")}
	m["/google.iam.v1.IAMPolicy/SetIamPolicy"] = Method{Field: "resource", PermissionFor: iam("setIamPolicy")}
	return m
}

// Tasks is Cloud Tasks's table: "Access control with IAM"
// (https://docs.cloud.google.com/tasks/docs/access-control, read
// 2026-11-19). Google words a denial 'The principal (user or service
// account) lacks IAM permission "P" for the resource "R" (or the resource
// may not exist).', with no ErrorInfo. A task inherits its queue's
// policy.
var Tasks = &Service{
	Name:              "cloudtasks.googleapis.com",
	PolicyCollections: []string{"queues"},
	Types:             map[string]string{"queues": "cloudtasks.googleapis.com/Queue", "tasks": "cloudtasks.googleapis.com/Task"},
	Methods: sm(map[string][2]string{
		"ListQueues":   {"cloudtasks.queues.list", "parent"},
		"GetQueue":     {"cloudtasks.queues.get", "name"},
		"CreateQueue":  {"cloudtasks.queues.create", "parent"},
		"UpdateQueue":  {"cloudtasks.queues.update", "queue.name"},
		"DeleteQueue":  {"cloudtasks.queues.delete", "name"},
		"PurgeQueue":   {"cloudtasks.queues.purge", "name"},
		"PauseQueue":   {"cloudtasks.queues.pause", "name"},
		"ResumeQueue":  {"cloudtasks.queues.resume", "name"},
		"GetIamPolicy": {"cloudtasks.queues.getIamPolicy", "resource"},
		"SetIamPolicy": {"cloudtasks.queues.setIamPolicy", "resource"},
		"ListTasks":    {"cloudtasks.tasks.list", "parent"},
		"GetTask":      {"cloudtasks.tasks.get", "name"},
		"CreateTask":   {"cloudtasks.tasks.create", "parent"},
		"DeleteTask":   {"cloudtasks.tasks.delete", "name"},
		"RunTask":      {"cloudtasks.tasks.run", "name"},
	}, "/google.cloud.tasks.v2.CloudTasks/"),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("The principal (user or service account) lacks IAM permission \"%s\" for the resource \"%s\" (or the resource may not exist).", permission, resource),
			permission, resource, false)
	},
}

func sm(rows map[string][2]string, prefix string) map[string]Method {
	out := make(map[string]Method, len(rows))
	for name, r := range rows {
		out[prefix+name] = Method{Permission: r[0], Field: r[1]}
	}
	return out
}

// PermissionOf is the method's permission on resource.
func (m *Method) PermissionOf(resource string) string {
	if m.PermissionFor != nil {
		return m.PermissionFor(resource)
	}
	return m.Permission
}

// ServiceByHost is the table of a service host
// ("secretmanager.googleapis.com"), nil for none.
func ServiceByHost(host string) *Service {
	for _, s := range Services {
		if s.Name == host {
			return s
		}
	}
	return nil
}
