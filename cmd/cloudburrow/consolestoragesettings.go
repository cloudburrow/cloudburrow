package main

// The Cloud Storage Settings screen (#792), at /storage/settings: the
// project's Cloud Storage service account (projects.serviceAccount.get) and
// its HMAC keys (projects.hmacKeys), the page Google's console calls
// Settings, with its Project access and Interoperability tabs.
//
// Every call is the storage server's own JSON API, the requests an
// application makes, and a refusal is the API's own message:
//
//   - Create key is hmacKeys.create. Its response is the only one that holds
//     the key's secret, so it goes to the console's create response as a
//     console.OneTime and nowhere else: not the operations ledger, not a log
//     entry, not the listing, and no page of its own.
//   - The Interoperability tab lists the service accounts with keys, each
//     opening its keys (#2047), as Google's "Access keys for service
//     accounts" does.
//   - A key's state is a button: clicked, it is hmacKeys.update against the
//     etag just read, to Inactive after a confirmation, to Active at once.
//   - Delete is hmacKeys.delete, offered only on an INACTIVE key (the API
//     refuses an ACTIVE one), confirmed by the access ID's first 10
//     characters.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// storageSettingsProvider is the Settings page of Cloud Storage.
type storageSettingsProvider struct{ endpoint string }

func (storageSettingsProvider) ID() string    { return "storage-settings" }
func (storageSettingsProvider) Title() string { return "Cloud Storage settings" }

// hmacKeyJSON is a key's metadata, as get, list and update return it.
type hmacKeyJSON struct {
	AccessID            string `json:"accessId"`
	ServiceAccountEmail string `json:"serviceAccountEmail"`
	State               string `json:"state"`
	TimeCreated         string `json:"timeCreated"`
	Updated             string `json:"updated"`
	ETag                string `json:"etag"`
}

// The states a key can be changed between; a deleted one is not listed.
const (
	hmacActive   = "ACTIVE"
	hmacInactive = "INACTIVE"
)

func (p storageSettingsProvider) projectURL(project string) string {
	return fmt.Sprintf("http://%s/storage/v1/projects/%s", p.endpoint, urlpkg.PathEscape(project))
}

func (p storageSettingsProvider) keyURL(project, accessID string) string {
	return p.projectURL(project) + "/hmacKeys/" + urlpkg.PathEscape(accessID)
}

// storageCall sends one JSON API request and decodes its answer into dst, or
// returns the API's own message.
func storageCall(ctx context.Context, method, url string, body any, dst any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// serviceAccount is the project's Cloud Storage service agent.
func (p storageSettingsProvider) serviceAccount(ctx context.Context, project string) (string, error) {
	var sa struct {
		Email string `json:"email_address"`
	}
	if err := storageCall(ctx, http.MethodGet, p.projectURL(project)+"/serviceAccount", nil, &sa); err != nil {
		return "", fmt.Errorf("read the project's service account: %w", err)
	}
	return sa.Email, nil
}

// hmacKeys lists the project's keys, every page of them.
func (p storageSettingsProvider) hmacKeys(ctx context.Context, project string) ([]hmacKeyJSON, error) {
	var out []hmacKeyJSON
	token := ""
	for {
		url := p.projectURL(project) + "/hmacKeys"
		if token != "" {
			url += "?pageToken=" + urlpkg.QueryEscape(token)
		}
		var page struct {
			Items         []hmacKeyJSON `json:"items"`
			NextPageToken string        `json:"nextPageToken"`
		}
		if err := storageCall(ctx, http.MethodGet, url, nil, &page); err != nil {
			return nil, fmt.Errorf("list HMAC keys: %w", err)
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		token = page.NextPageToken
	}
}

func (p storageSettingsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{
		Columns:    []string{"HMAC keys", "Active keys"},
		NameColumn: "Service account",
		Noun:       "service accounts",
	}
	if project == "" {
		listing.Prompt = "Cloud Storage settings are per project. Choose one in the toolbar."
		return listing, nil
	}
	email, err := p.serviceAccount(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	keys, err := p.hmacKeys(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	listing.Summary = []console.PropertyGroup{{Heading: "Project access", Properties: []console.Property{
		{Label: "Project ID", Value: project},
		{Label: "Cloud Storage service account", Value: email},
	}}}
	listing.Tabs = []console.Section{
		{ID: "project-access", Label: "Project access",
			Note: "The project's Cloud Storage service account, which acts for Cloud Storage in this project."},
		{ID: "interoperability", Label: "Interoperability"},
	}
	listing.ListTab = "interoperability"
	// Google's Interoperability tab (#2047, managing-hmackeys): the
	// Service account HMAC section's "Access keys for service accounts"
	// lists the service accounts with keys, each opening its keys.
	listing.TableTitle = "Access keys for service accounts"
	listing.Note = "Service account HMAC: the service accounts with HMAC keys in this project. Open one to see " +
		"its keys, change a key's state by clicking it, or delete an inactive key. A key's secret is shown once, when it is created."
	total, active := map[string]int{}, map[string]int{}
	var order []string
	for _, k := range keys {
		if total[k.ServiceAccountEmail] == 0 {
			order = append(order, k.ServiceAccountEmail)
		}
		total[k.ServiceAccountEmail]++
		if k.State == hmacActive {
			active[k.ServiceAccountEmail]++
		}
	}
	sort.Strings(order)
	for _, sa := range order {
		listing.Items = append(listing.Items, console.Resource{
			Name:   sa,
			Fields: map[string]string{"HMAC keys": strconv.Itoa(total[sa]), "Active keys": strconv.Itoa(active[sa])},
		})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// Detail implements console.Driller: a service account's HMAC keys, each
// with its state as a button that changes it and, once inactive, Delete.
func (p storageSettingsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	keys, err := p.hmacKeys(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	sec := console.Section{ID: "keys", Label: "HMAC keys", Listing: console.Listing{
		Columns: []string{"State", "Created", "Updated"}, NameColumn: "Access ID", Noun: "HMAC keys",
		Items: []console.Resource{},
		Note:  "Click a key's state to change it: deactivating asks first. Delete a key once it is inactive, typing the first 10 characters of its access ID.",
	}}
	for _, k := range keys {
		if k.ServiceAccountEmail != path[0] {
			continue
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{
			Name: k.AccessID,
			Fields: map[string]string{"State": hmacStateLabel(k.State),
				"Created": shortTime(k.TimeCreated), "Updated": shortTime(k.Updated)},
			Target:  []string{path[0], k.AccessID},
			Actions: hmacActions(k.State),
		})
	}
	if len(sec.Listing.Items) == 0 {
		return console.Detail{Unavailable: path[0] + " has no HMAC keys in this project."}, nil
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return console.Detail{Title: path[0], Summary: []console.Property{
		{Label: "Service account", Value: path[0]},
		{Label: "HMAC keys", Value: strconv.Itoa(len(sec.Listing.Items))},
	}, Sections: []console.Section{sec}}, nil
}

// hmacStateLabel is a key's state as Google's console words it.
func hmacStateLabel(state string) string {
	switch state {
	case hmacActive:
		return "Active"
	case hmacInactive:
		return "Inactive"
	}
	return state
}

// Create key.

func (storageSettingsProvider) CreateForm() (string, []console.Field) {
	// Google's Interoperability tab: "Create a key for a service account"
	// (#1843, managing-hmackeys).
	return "Create a key for a service account", []console.Field{{
		Name: "serviceAccountEmail", Label: "Service account email", Type: "text", Required: true,
		Pattern: `^[^@\s]+@[^@\s]+$`,
		Help: "The service account the key signs requests as, such as app@PROJECT.iam.gserviceaccount.com. " +
			"A service account has at most 10 keys that are not deleted.",
	}}
}

// CreateSubmit implements console.CreateSubmitter: the dialog that "Create
// a key for a service account" opens ends in Create key.
func (storageSettingsProvider) CreateSubmit() string { return "Create key" }

// Create is CreateOneTime without the secret. The create route calls
// CreateOneTime, so the secret is shown; this exists for the Creator
// interface.
func (p storageSettingsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	name, _, err := p.CreateOneTime(ctx, project, values)
	return name, err
}

// CreateOneTime implements console.OneTimeCreator: hmacKeys.create, whose
// secret is returned for the dialog and kept nowhere.
func (p storageSettingsProvider) CreateOneTime(ctx context.Context, project string, values map[string]string) (string, *console.OneTime, error) {
	if project == "" {
		return "", nil, errors.New("choose a project before creating an HMAC key")
	}
	email := strings.TrimSpace(values["serviceAccountEmail"])
	if email == "" {
		return "", nil, errors.New("name the service account the key is for")
	}
	var created struct {
		Metadata hmacKeyJSON `json:"metadata"`
		Secret   string      `json:"secret"`
	}
	url := p.projectURL(project) + "/hmacKeys?serviceAccountEmail=" + urlpkg.QueryEscape(email)
	if err := storageCall(ctx, http.MethodPost, url, nil, &created); err != nil {
		return "", nil, err
	}
	k := created.Metadata
	if k.AccessID == "" || created.Secret == "" {
		return "", nil, errors.New("the storage server's create response holds no access ID or secret")
	}
	return k.AccessID, &console.OneTime{
		Title: "HMAC key created",
		Properties: []console.Property{
			{Label: "Access ID", Value: k.AccessID},
			{Label: "Service account", Value: k.ServiceAccountEmail},
			{Label: "State", Value: k.State},
		},
		Label: "Secret",
		Value: created.Secret,
		Note: "You won't see this secret again. Copy it now and keep it somewhere safe: " +
			"the API returns it only in this create response, and the console does not keep it. " +
			"If it is lost, create another key and delete this one.",
	}, nil
}

// Activate, Deactivate and Delete.

// hmacActions are a key's buttons: its state, clicked to change it, and
// Delete only when INACTIVE, as the API requires. Google's console asks
// first only on the way to Inactive, and has Delete confirmed by the access
// ID's first 10 characters (managing-hmackeys, #2047).
func hmacActions(state string) []console.Action {
	switch state {
	case hmacActive:
		return []console.Action{{ID: "deactivate", Label: "Active", Button: true,
			ConfirmTitle: "Deactivate HMAC key?", Submit: "Deactivate", ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "Requests signed with this key are refused until it is activated again."}}
	case hmacInactive:
		return []console.Action{
			{ID: "activate", Label: "Inactive", Button: true},
			{ID: "delete", Label: "Delete", Button: true, Destructive: true, ConfirmTitle: "Delete HMAC key?",
				ConfirmStyle: console.DeleteConfirmPrefix10},
		}
	}
	return nil
}

// DetailActions implements console.PathActor: a key's, read now.
func (p storageSettingsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 2 || project == "" {
		return nil
	}
	var k hmacKeyJSON
	if err := storageCall(ctx, http.MethodGet, p.keyURL(project, path[1]), nil, &k); err != nil || k.ServiceAccountEmail != path[0] {
		return nil
	}
	return hmacActions(k.State)
}

// ActAt implements console.PathActor on [service account, access ID]. The
// key is read first and the action checked against its current state, so
// the route can do no more than the row it drew offered.
func (p storageSettingsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if project == "" {
		return errors.New("choose a project")
	}
	if len(path) != 2 {
		return errors.New("an HMAC key is addressed serviceAccount/accessId")
	}
	name := path[1]
	var k hmacKeyJSON
	if err := storageCall(ctx, http.MethodGet, p.keyURL(project, name), nil, &k); err != nil {
		return err
	}
	if k.ServiceAccountEmail != path[0] {
		return fmt.Errorf("%s is not a key of %s", name, path[0])
	}
	offered := false
	for _, a := range hmacActions(k.State) {
		offered = offered || a.ID == action
	}
	if !offered {
		return fmt.Errorf("%s is not available on a key that is %s", action, k.State)
	}
	switch action {
	case "activate", "deactivate":
		state := hmacActive
		if action == "deactivate" {
			state = hmacInactive
		}
		// Against the etag just read, so a change made meanwhile is refused
		// rather than overwritten.
		return storageCall(ctx, http.MethodPut, p.keyURL(project, name),
			map[string]string{"state": state, "etag": k.ETag}, nil)
	case "delete":
		return storageCall(ctx, http.MethodDelete, p.keyURL(project, name), nil, nil)
	}
	return fmt.Errorf("%s is not an HMAC key action", action)
}

var (
	_ console.OneTimeCreator = storageSettingsProvider{}
	_ console.PathActor      = storageSettingsProvider{}
	_ console.Driller        = storageSettingsProvider{}
)
