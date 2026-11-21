package main

import (
	"crypto/rsa"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

// serviceAccountDirectory is the metadata server's view of the IAM service
// accounts in the project registry (#1747): IAM Credentials' accounts and
// permission checks, and the keys the token endpoint accepts.
type serviceAccountDirectory struct {
	reg *resourcemanager.Registry
}

var _ metadata.ServiceAccountDirectory = serviceAccountDirectory{}

func (d serviceAccountDirectory) Account(name string) (metadata.DirectoryAccount, bool) {
	a, ok := d.reg.ServiceAccountByName(name)
	return metadata.DirectoryAccount{Email: a.Email, UniqueID: a.UniqueID, Disabled: a.Disabled}, ok
}

// Granted checks the account's own policy, its project's, and those of
// the project's folders and organization, which Google's resource
// hierarchy makes every account in the project inherit (#1750). A role is
// a custom role's permissions or a predefined role's from Google's catalog
// (internal/iamroles, #1746), so any role holding the permission counts.
// A deleted account's bindings read deleted:serviceAccount:...?uid= and
// match no caller.
func (d serviceAccountDirectory) Granted(member, account, permission string) bool {
	a, ok := d.reg.ServiceAccountByName(account)
	if !ok {
		return false
	}
	_, domain, _ := strings.Cut(a.Email, "@")
	project := strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
	resources := []string{"projects/" + project + "/serviceAccounts/" + a.Email}
	if anc, err := d.reg.Ancestry(project); err == nil {
		for _, s := range anc {
			switch s.Type {
			case "project":
				resources = append(resources, "projects/"+s.ID)
			case "folder":
				resources = append(resources, "folders/"+s.ID)
			case "organization":
				resources = append(resources, resourcemanager.OrganizationName)
			}
		}
	} else {
		resources = append(resources, "projects/"+project)
	}
	for _, res := range resources {
		pol, err := d.reg.IAMPolicy(res)
		if err != nil || pol == nil {
			continue
		}
		for _, b := range pol.Bindings {
			held := false
			for _, m := range b.Members {
				// A group: or domain: member holds it for its accounts
				// (#1812).
				if d.heldBy(m, member) {
					held = true
				}
			}
			if held && iamroles.Grants(d.rolePermissions(b.Role), permission) {
				return true
			}
		}
	}
	return false
}

// rolePermissions are a custom or predefined role's permissions.
func (d serviceAccountDirectory) rolePermissions(role string) []string {
	if perms, custom := d.reg.RolePermissions(role); custom {
		return perms
	}
	if r := iamroles.Predefined(role); r != nil {
		return r.Permissions
	}
	return nil
}

func (d serviceAccountDirectory) Signer(account string) (*rsa.PrivateKey, string, error) {
	return d.reg.ServiceAccountSigner(account)
}

func (d serviceAccountDirectory) Key(keyID string, now time.Time) (*rsa.PublicKey, string, bool, bool) {
	return d.reg.ServiceAccountKeyState(keyID, now)
}

func (d serviceAccountDirectory) PublicCerts(account string) ([]metadata.PublicCert, bool) {
	certs, ok := d.reg.ServiceAccountPublicCerts(account)
	out := make([]metadata.PublicCert, 0, len(certs))
	for _, c := range certs {
		out = append(out, metadata.PublicCert{ID: c.ID, CertPEM: c.CertPEM, Key: c.Key})
	}
	return out, ok
}

// signAuthorizer checks the caller of the IAM API's deprecated SignBlob
// and SignJwt (#1751) as IAM Credentials checks its own: the account a
// live access token this instance minted names, holding the permission on
// the account or above it.
func signAuthorizer(creds *metadata.Credentials, d serviceAccountDirectory) resourcemanager.SignAuthorizer {
	return func(bearer, account, permission string) error {
		caller, ok := "", false
		if bearer != "" {
			caller, ok = creds.TokenAccount(bearer, time.Now())
		}
		if !ok {
			return status.Error(codes.Unauthenticated, "Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.")
		}
		if account == "" || !d.Granted(metadata.Member(caller), account, permission) {
			return status.Errorf(codes.PermissionDenied, "Permission '%s' denied on resource (or it may not exist).", permission)
		}
		return nil
	}
}

// heldBy is whether the policy member m names the account member
// (serviceAccount:EMAIL): itself, a group it is in (nested too) or its
// domain (#1812).
func (d serviceAccountDirectory) heldBy(m, member string) bool {
	if wif.IsFederated(member) {
		// A federated subject (#1941) holds what its principal:// or
		// one of its principalSet:// identifiers is granted.
		for _, p := range d.reg.Principals(member) {
			if strings.EqualFold(m, p) {
				return true
			}
		}
		return false
	}
	_, email, _ := strings.Cut(member, ":")
	return strings.EqualFold(m, member) || d.reg.MemberNames(m, email)
}
