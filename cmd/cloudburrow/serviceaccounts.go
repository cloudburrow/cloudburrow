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

// tokenCreatorRoles are the predefined roles that grant IAM Credentials'
// permissions, with what each grants, from Google's IAM roles reference
// (docs.cloud.google.com/iam/docs/roles-permissions/iam, read 2026-11-17).
// Owner and Editor grant none of them.
var tokenCreatorRoles = map[string][]string{
	"roles/iam.serviceAccountTokenCreator": {"iam.serviceAccounts.get", "iam.serviceAccounts.getAccessToken",
		"iam.serviceAccounts.getOpenIdToken", "iam.serviceAccounts.implicitDelegation", "iam.serviceAccounts.list",
		"iam.serviceAccounts.signBlob", "iam.serviceAccounts.signJwt"},
	"roles/iam.serviceAccountOpenIdTokenCreator": {"iam.serviceAccounts.getOpenIdToken"},
	"roles/iam.workloadIdentityUser":             {"iam.serviceAccounts.get", "iam.serviceAccounts.getAccessToken", "iam.serviceAccounts.getOpenIdToken", "iam.serviceAccounts.list"},
}

func (d serviceAccountDirectory) Account(name string) (metadata.DirectoryAccount, bool) {
	a, ok := d.reg.ServiceAccountByName(name)
	return metadata.DirectoryAccount{Email: a.Email, UniqueID: a.UniqueID, Disabled: a.Disabled}, ok
}

// Granted checks the account's own policy and its project's.
func (d serviceAccountDirectory) Granted(member, account, permission string) bool {
	a, ok := d.reg.ServiceAccountByName(account)
	if !ok {
		return false
	}
	_, domain, _ := strings.Cut(a.Email, "@")
	project := strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
	for _, res := range []string{"projects/" + project + "/serviceAccounts/" + a.Email, "projects/" + project} {
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
			if !held {
				continue
			}
			perms, custom := d.reg.RolePermissions(b.Role)
			if !custom {
				perms = tokenCreatorRoles[b.Role]
			}
			if iamroles.Grants(perms, permission) {
				return true
			}
		}
	}
	return false
}

func (d serviceAccountDirectory) Signer(account string) (*rsa.PrivateKey, string, error) {
	return d.reg.ServiceAccountSigner(account)
}

func (d serviceAccountDirectory) Key(keyID string, now time.Time) (*rsa.PublicKey, string, bool, bool) {
	return d.reg.ServiceAccountKeyState(keyID, now)
}
