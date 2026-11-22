package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
)

// The Credentials page's API keys (#1915): the list, Create credentials >
// API key, each key's page (name, application and API restrictions, Show
// key, Rotate key, Delete, and Delete the previous key after a rotation),
// and Restore deleted credentials. They are the serviceusage provider's
// Studio calls, over the same apikeys.Server gcloud and Terraform call.

// consoleKey is a key as the pages read it. The key string is only in the
// answers that show it (Show key, a create, a rotation).
type consoleKey struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	DisplayName  string                `json:"displayName"`
	CreateTime   string                `json:"createTime"`
	DeleteTime   string                `json:"deleteTime,omitempty"`
	Restrictions *apikeys.Restrictions `json:"restrictions,omitempty"`
	Summary      string                `json:"summary"`
	PreviousKey  string                `json:"previousKey,omitempty"`
	Previous     *consoleKey           `json:"previous,omitempty"`
	KeyString    string                `json:"keyString,omitempty"`
	ServiceAcct  string                `json:"serviceAccountEmail,omitempty"`
	Annotations  map[string]string     `json:"annotations,omitempty"`
}

// keySummary is the list's Restrictions column, as Google words it.
func keySummary(r *apikeys.Restrictions) string {
	if r == nil {
		return "None"
	}
	app := ""
	switch {
	case r.BrowserKeyRestrictions != nil:
		app = "Websites"
	case r.ServerKeyRestrictions != nil:
		app = "IP addresses"
	case r.AndroidKeyRestrictions != nil:
		app = "Android apps"
	case r.IosKeyRestrictions != nil:
		app = "iOS apps"
	}
	apis := ""
	if n := len(r.APITargets); n == 1 {
		apis = "1 API"
	} else if n > 1 {
		apis = fmt.Sprintf("%d APIs", n)
	}
	switch {
	case app != "" && apis != "":
		return app + ", " + apis
	case app != "":
		return app
	case apis != "":
		return apis
	}
	return "None"
}

func toConsoleKey(k *apikeys.Key, withString bool) consoleKey {
	c := consoleKey{ID: k.ID, Name: k.Name(), DisplayName: k.DisplayName, CreateTime: k.CreateTime.UTC().Format(time.RFC3339),
		Restrictions: k.Restrictions, Summary: keySummary(k.Restrictions), PreviousKey: k.PreviousKey,
		ServiceAcct: k.ServiceAccountEmail, Annotations: k.Annotations}
	if k.Deleted() {
		c.DeleteTime = k.DeleteTime.UTC().Format(time.RFC3339)
	}
	if withString {
		c.KeyString = k.KeyString
	}
	return c
}

// keysCall answers the key calls:
//
//   - keys {deleted}: the project's live keys, or its deleted ones;
//   - key {id}: one key, with its previous key after a rotation;
//   - key-create {displayName, restrictions, serviceAccountEmail}: Create
//     credentials > API key, Google's Create API key page (#2145);
//   - key-copy {id, displayName, restrictions}: Create a copy (#2145);
//   - key-update {id, displayName, restrictions}: the key page's Save;
//   - key-string {id}: Show key;
//   - key-delete / key-undelete {id}: Delete and Restore;
//   - key-rotate {id, displayName}: Rotate key;
//   - key-delete-previous {id}: Delete the previous key.
func (p serviceUsageProvider) keysCall(project, call string, body json.RawMessage) (any, error) {
	if p.apiKeys == nil {
		return nil, errors.New("the API Keys API is not served by this instance")
	}
	var req struct {
		ID           string                `json:"id"`
		DisplayName  string                `json:"displayName"`
		Restrictions *apikeys.Restrictions `json:"restrictions"`
		Deleted      bool                  `json:"deleted"`
		// ServiceAccount is Create API key's "Authenticate API calls
		// through a service account" (#2145).
		ServiceAccount string `json:"serviceAccountEmail"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %v", err)
		}
	}
	if call != "keys" && call != "key-create" && req.ID == "" {
		return nil, errors.New("a key is required")
	}
	switch call {
	case "keys":
		keys, err := p.apiKeys.List(project, req.Deleted)
		if err != nil {
			return nil, err
		}
		out := []consoleKey{}
		for _, k := range keys {
			if k.Deleted() == req.Deleted {
				out = append(out, toConsoleKey(k, false))
			}
		}
		return map[string]any{"project": project, "keys": out}, nil
	case "key":
		k, err := p.apiKeys.Get(project, req.ID)
		if err != nil {
			return nil, err
		}
		c := toConsoleKey(k, false)
		if k.PreviousKey != "" {
			if prev, err := p.apiKeys.Get(project, k.PreviousKey); err == nil && !prev.Deleted() {
				pc := toConsoleKey(prev, false)
				c.Previous = &pc
			}
		}
		return c, nil
	case "key-create":
		k, err := p.apiKeys.Create(project, "", req.DisplayName, req.Restrictions, nil, strings.TrimSpace(req.ServiceAccount))
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, true), nil
	case "key-copy":
		// Create a copy (#2145): a new key with this one's restrictions,
		// under the name given, as Google's console-only Create a copy.
		src, err := p.apiKeys.Get(project, req.ID)
		if err != nil {
			return nil, err
		}
		r := src.Restrictions
		if req.Restrictions != nil {
			r = req.Restrictions
		}
		k, err := p.apiKeys.Create(project, "", req.DisplayName, r, nil, src.ServiceAccountEmail)
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, true), nil
	case "key-update":
		k, err := p.apiKeys.Update(project, req.ID, req.DisplayName, req.Restrictions)
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, false), nil
	case "key-string":
		k, err := p.apiKeys.Get(project, req.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"keyString": k.KeyString}, nil
	case "key-delete":
		k, err := p.apiKeys.Delete(project, req.ID)
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, false), nil
	case "key-undelete":
		k, err := p.apiKeys.Undelete(project, req.ID)
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, false), nil
	case "key-rotate":
		k, err := p.apiKeys.Rotate(project, req.ID, req.DisplayName)
		if err != nil {
			return nil, err
		}
		return toConsoleKey(k, true), nil
	case "key-delete-previous":
		k, err := p.apiKeys.Get(project, req.ID)
		if err != nil {
			return nil, err
		}
		if k.PreviousKey == "" {
			return nil, errors.New("this key has no previous key")
		}
		if _, err := p.apiKeys.Delete(project, k.PreviousKey); err != nil {
			return nil, err
		}
		if err := p.apiKeys.ClearPrevious(project, req.ID); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": k.PreviousKey}, nil
	}
	return nil, fmt.Errorf("unknown call %q", call)
}
