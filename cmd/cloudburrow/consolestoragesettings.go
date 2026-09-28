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
//   - Activate and Deactivate are hmacKeys.update against the etag just read.
//   - Delete is hmacKeys.delete, offered only on an INACTIVE key: the API
//     refuses an ACTIVE one, so the row does not offer it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
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
		Columns:      []string{"Service account", "Created", "Updated"},
		NameColumn:   "Access ID",
		Noun:         "HMAC keys",
		AlwaysStatus: true,
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
	listing.Note = "Interoperability: the project's HMAC keys. A key's secret is shown once, when it " +
		"is created; a key can be deleted once it is inactive."
	for _, k := range keys {
		listing.Items = append(listing.Items, console.Resource{
			Name:   k.AccessID,
			Status: k.State,
			Fields: map[string]string{
				"Service account": k.ServiceAccountEmail,
				"Created":         shortTime(k.TimeCreated),
				"Updated":         shortTime(k.Updated),
			},
		})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

// Create key.

func (storageSettingsProvider) CreateForm() (string, []console.Field) {
	return "Create key", []console.Field{{
		Name: "serviceAccountEmail", Label: "Service account email", Type: "text", Required: true,
		Pattern: `^[^@\s]+@[^@\s]+$`,
		Help: "The service account the key signs requests as, such as app@PROJECT.iam.gserviceaccount.com. " +
			"A service account has at most 10 keys that are not deleted.",
	}}
}

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

// hmacActions are what a key in a state can have done to it: Delete only when
// INACTIVE, as the API requires.
func hmacActions(state string) []console.Action {
	switch state {
	case hmacActive:
		return []console.Action{{ID: "deactivate", Label: "Deactivate"}}
	case hmacInactive:
		return []console.Action{
			{ID: "activate", Label: "Activate"},
			{ID: "delete", Label: "Delete", Destructive: true},
		}
	}
	return nil
}

// Actions implements console.Actor.
func (storageSettingsProvider) Actions(r console.Resource) []console.Action {
	return hmacActions(r.Status)
}

// Act implements console.Actor. The key is read first and the action checked
// against its current state, so the route can do no more than the row it
// drew offered.
func (p storageSettingsProvider) Act(ctx context.Context, project, name, action string) error {
	if project == "" {
		return errors.New("choose a project")
	}
	var k hmacKeyJSON
	if err := storageCall(ctx, http.MethodGet, p.keyURL(project, name), nil, &k); err != nil {
		return err
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
	_ console.Actor          = storageSettingsProvider{}
)
