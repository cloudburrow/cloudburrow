package main

// IAM & Admin > Service accounts (#1747): the selected project's service
// accounts, as Google's console lists them, with Create service account,
// the account's page (its details, its keys and its Permissions tab), Edit,
// Disable, Enable, Delete, Undelete by unique ID, Create key (the JSON key
// file shown once, never recorded), Upload key, and a key's Disable,
// Enable and Delete, over the registry the IAM API serves, so an account
// made here is one gcloud iam service-accounts and Terraform see, and the
// API's refusals are the forms'.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

type iamServiceAccountsProvider struct {
	registry *resourcemanager.Registry
}

func (iamServiceAccountsProvider) ID() string    { return "iam-service-accounts" }
func (iamServiceAccountsProvider) Title() string { return "Service accounts" }

const iamServiceAccountsNote = "Service accounts of this project. A caller granted Service Account Token Creator on one, or on the project, " +
	"impersonates it through IAM Credentials; a key file signs in at this instance's token endpoint only. " +
	"A deleted account can be undeleted by its unique ID for 30 days."

const serviceAccountIDPattern = `^[a-z][a-z0-9-]{4,28}[a-z0-9]$`

func (p iamServiceAccountsProvider) admin() *resourcemanager.IAMAdminServer {
	return resourcemanager.NewIAMAdminServer(p.registry)
}

func saName(project, email string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	return "projects/" + project + "/serviceAccounts/" + email, nil
}

func saStatus(a *adminpb.ServiceAccount) string {
	if a.GetDisabled() {
		return "DISABLED"
	}
	return "ENABLED"
}

func (p iamServiceAccountsProvider) userKeys(name string) []*adminpb.ServiceAccountKey {
	l, err := p.registry.ListServiceAccountKeys(&adminpb.ListServiceAccountKeysRequest{Name: name})
	if err != nil {
		return nil
	}
	return l.GetKeys()
}

func (p iamServiceAccountsProvider) List(_ context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Email", Columns: []string{"Name", "Description", "Unique ID", "Keys"}, Noun: "service accounts", Note: iamServiceAccountsNote}
	if project == "" {
		l.Unavailable = "choose a project in the toolbar"
		return l, nil
	}
	resp, err := p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100})
	for err == nil {
		for _, a := range resp.GetAccounts() {
			n := 0
			for _, k := range p.userKeys(a.GetName()) {
				if k.GetKeyType() == adminpb.ListServiceAccountKeysRequest_USER_MANAGED {
					n++
				}
			}
			row := console.Resource{Name: a.GetEmail(), Status: saStatus(a), Fields: map[string]string{
				"Name": a.GetDisplayName(), "Description": a.GetDescription(), "Unique ID": a.GetUniqueId(), "Keys": strconv.Itoa(n)}}
			row.Actions = p.Actions(row)
			l.Items = append(l.Items, row)
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		resp, err = p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100, PageToken: resp.GetNextPageToken()})
	}
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l, nil
	}
	l.Total = len(l.Items)
	return l, nil
}

func saFields(a *adminpb.ServiceAccount) []console.Field {
	return []console.Field{
		{Name: "displayName", Label: "Service account name", Type: "text", Default: a.GetDisplayName(), Help: "At most 100 bytes."},
		{Name: "description", Label: "Service account description", Type: "text", Default: a.GetDescription(), Help: "At most 256 bytes."},
	}
}

func (iamServiceAccountsProvider) CreateForm() (string, []console.Field) {
	return "Create service account", append([]console.Field{
		{Name: "accountId", Label: "Service account ID", Type: "text", Required: true, Pattern: serviceAccountIDPattern,
			Help: "6 to 30 lowercase letters, digits and dashes, starting with a letter. The email is ID@PROJECT.iam.gserviceaccount.com; it cannot change."},
	}, saFields(nil)...)
}

func (iamServiceAccountsProvider) CreateOnPage() bool { return true }

func (p iamServiceAccountsProvider) Create(_ context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	a, err := p.registry.CreateServiceAccount(&adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: strings.TrimSpace(values["accountId"]),
		ServiceAccount: &adminpb.ServiceAccount{DisplayName: values["displayName"], Description: values["description"]}})
	if err != nil {
		return "", err
	}
	return a.GetEmail(), nil
}

func keyRow(k *adminpb.ServiceAccountKey) console.Resource {
	id := k.GetName()[strings.LastIndex(k.GetName(), "/")+1:]
	status := "ENABLED"
	if k.GetDisabled() {
		status = "DISABLED"
	}
	expires := k.GetValidBeforeTime().AsTime().UTC().Format("2006-01-02T15:04:05Z")
	return console.Resource{Name: id, Status: status, Fields: map[string]string{
		"Type": strings.TrimSuffix(k.GetKeyType().String(), "_MANAGED"), "Origin": k.GetKeyOrigin().String(),
		"Created": k.GetValidAfterTime().AsTime().UTC().Format("2006-01-02T15:04:05Z"), "Expires": expires}}
}

func (p iamServiceAccountsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	name, err := saName(project, path[0])
	if err != nil {
		return console.Detail{Prompt: err.Error()}, nil
	}
	a, err := p.registry.GetServiceAccount(name)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the service account: " + consoleErr(err)}, nil
	}
	if len(path) == 2 {
		k, err := p.registry.GetServiceAccountKey(name+"/keys/"+path[1], adminpb.ServiceAccountPublicKeyType_TYPE_X509_PEM_FILE)
		if err != nil {
			return console.Detail{Unavailable: "cannot read the key: " + consoleErr(err)}, nil
		}
		r := keyRow(k)
		props := []console.Property{{Label: "Key ID", Value: r.Name}, {Label: "Status", Value: r.Status}, {Label: "Type", Value: r.Fields["Type"]},
			{Label: "Origin", Value: r.Fields["Origin"]}, {Label: "Created", Value: r.Fields["Created"]}, {Label: "Expires", Value: r.Fields["Expires"]}}
		return console.Detail{Summary: props[:2], Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Key", Properties: props}}},
			{ID: "certificate", Label: "Public key certificate", Kind: console.KindText, Text: string(k.GetPublicKeyData())},
		}}, nil
	}
	props := []console.Property{
		{Label: "Email", Value: a.GetEmail()},
		{Label: "Name", Value: orDash(a.GetDisplayName())},
		{Label: "Description", Value: orDash(a.GetDescription())},
		{Label: "Unique ID", Value: a.GetUniqueId()},
		{Label: "OAuth 2 client ID", Value: a.GetOauth2ClientId()},
		{Label: "Status", Value: saStatus(a)},
	}
	keys := console.Listing{NameColumn: "Key ID", Columns: []string{"Type", "Origin", "Created", "Expires"}, Noun: "keys"}
	for _, k := range p.userKeys(name) {
		r := keyRow(k)
		r.Actions = p.DetailActions(ctx, project, []string{path[0], r.Name})
		keys.Items = append(keys.Items, r)
	}
	keys.Total = len(keys.Items)
	return console.Detail{Summary: props[:2], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Service account", Properties: props}}},
		{ID: "keys", Label: "Keys", Listing: keys},
	}, Edit: &console.EditForm{Label: "Edit service account", Fields: saFields(a), Note: "The ID and email cannot change, as on Google."}}, nil
}

func (p iamServiceAccountsProvider) Edit(_ context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a service account can be edited")
	}
	name, err := saName(project, path[0])
	if err != nil {
		return err
	}
	_, err = p.registry.PatchServiceAccount(&adminpb.ServiceAccount{Name: name, DisplayName: values["displayName"], Description: values["description"]},
		[]string{"display_name", "description"})
	return err
}

func (p iamServiceAccountsProvider) Delete(_ context.Context, project, row string) error {
	name, err := saName(project, row)
	if err != nil {
		return err
	}
	return p.registry.DeleteServiceAccount(name)
}

// Actions offers Disable or Enable on a list row; Delete is the list's own.
func (iamServiceAccountsProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "DISABLED" {
		return []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return []console.Action{{ID: "disable", Label: "Disable", Confirm: "the account can no longer authenticate: IAM Credentials refuses it and its keys do not sign in"}}
}

func (p iamServiceAccountsProvider) Act(ctx context.Context, project, row, action string) error {
	return p.ActAt(ctx, project, []string{row}, action, nil)
}

// ListActions offers Undelete, by unique ID, as Google's does.
func (iamServiceAccountsProvider) ListActions(context.Context, string) []console.Action {
	return []console.Action{{ID: "undelete", Label: "Undelete service account", Fields: []console.Field{
		{Name: "uniqueId", Label: "Unique ID", Type: "text", Required: true, Pattern: `^\d{21}$`,
			Help: "The deleted account's 21-digit unique ID. An account deleted less than 30 days ago can be undeleted, unless one with its email was created since."}}}}
}

// DetailActions are an account's Create key, Upload key, Disable or Enable,
// and a key's Disable or Enable and Delete.
func (p iamServiceAccountsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		name, err := saName(project, path[0])
		if err != nil {
			return nil
		}
		a, err := p.registry.GetServiceAccount(name)
		if err != nil {
			return nil
		}
		out := []console.Action{
			{ID: "create-key", Label: "Create key", Confirm: "the JSON key file is shown once, and is not kept: save it"},
			{ID: "upload-key", Label: "Upload key", Fields: []console.Field{{Name: "certificate", Label: "X.509 certificate (PEM)", Type: "textarea", Required: true,
				Help: "The certificate of an RSA 2048 public key, -----BEGIN CERTIFICATE----- to -----END CERTIFICATE-----."}}},
		}
		return append(out, p.Actions(console.Resource{Status: saStatus(a)})...)
	case 2:
		name, err := saName(project, path[0])
		if err != nil {
			return nil
		}
		k, err := p.registry.GetServiceAccountKey(name+"/keys/"+path[1], adminpb.ServiceAccountPublicKeyType_TYPE_NONE)
		if err != nil || k.GetKeyType() != adminpb.ListServiceAccountKeysRequest_USER_MANAGED {
			return nil
		}
		toggle := console.Action{ID: "disable-key", Label: "Disable"}
		if k.GetDisabled() {
			toggle = console.Action{ID: "enable-key", Label: "Enable"}
		}
		return []console.Action{toggle, {ID: "delete-key", Label: "Delete", Destructive: true, Leaves: true}}
	}
	return nil
}

func (p iamServiceAccountsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult performs an action; Create key answers with the key file,
// which goes to the response only, never to the operations ledger.
func (p iamServiceAccountsProvider) ActAtResult(_ context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 {
		if action != "undelete" {
			return nil, fmt.Errorf("unknown action %q", action)
		}
		_, err := p.registry.UndeleteServiceAccount("projects/-/serviceAccounts/" + strings.TrimSpace(values["uniqueId"]))
		return nil, err
	}
	name, err := saName(project, path[0])
	if err != nil {
		return nil, err
	}
	key := ""
	if len(path) == 2 {
		key = name + "/keys/" + path[1]
	}
	switch {
	case action == "disable" && key == "":
		return nil, p.registry.SetServiceAccountDisabled(name, true)
	case action == "enable" && key == "":
		return nil, p.registry.SetServiceAccountDisabled(name, false)
	case action == "create-key" && key == "":
		k, err := p.registry.CreateServiceAccountKey(&adminpb.CreateServiceAccountKeyRequest{Name: name})
		if err != nil {
			return nil, err
		}
		r := keyRow(k)
		r.Fields["Key file (JSON)"] = string(k.GetPrivateKeyData())
		return &console.Listing{NameColumn: "Key ID", Columns: []string{"Type", "Expires", "Key file (JSON)"}, Noun: "keys",
			Note: "Save the key file now: it is not shown again. It signs in at this instance's token endpoint only.", Items: []console.Resource{r}, Total: 1}, nil
	case action == "upload-key" && key == "":
		_, err := p.registry.UploadServiceAccountKey(name, []byte(strings.TrimSpace(values["certificate"])+"\n"))
		return nil, err
	case action == "disable-key" && key != "":
		return nil, p.registry.SetServiceAccountKeyDisabled(key, true)
	case action == "enable-key" && key != "":
		return nil, p.registry.SetServiceAccountKeyDisabled(key, false)
	case action == "delete-key" && key != "":
		return nil, p.registry.DeleteServiceAccountKey(key)
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// PolicyOn gives an account's page its Permissions tab: the account's own
// policy, which grants who may impersonate or act as it.
func (iamServiceAccountsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "iam-custom-roles--googleiamadminv1",
		RoleHelp: roleHelp("IAM", "roles/iam.serviceAccountTokenCreator")}
}

func (p iamServiceAccountsProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	name, err := saName(project, path[0])
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, p.admin(), name)
}

func (p iamServiceAccountsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	name, err := saName(project, path[0])
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, p.admin(), name, pol)
}

func (p iamServiceAccountsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	name, err := saName(project, path[0])
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, p.admin(), name, perms)
}

var _ console.Creator = iamServiceAccountsProvider{}
var _ console.PageCreator = iamServiceAccountsProvider{}
var _ console.Deleter = iamServiceAccountsProvider{}
var _ console.Driller = iamServiceAccountsProvider{}
var _ console.Editor = iamServiceAccountsProvider{}
var _ console.Actor = iamServiceAccountsProvider{}
var _ console.ResultActor = iamServiceAccountsProvider{}
var _ console.ListActor = iamServiceAccountsProvider{}
