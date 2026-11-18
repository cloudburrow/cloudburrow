package main

import (
	"crypto/rsa"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
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
				if strings.EqualFold(m, member) {
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
