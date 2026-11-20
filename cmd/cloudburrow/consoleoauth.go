package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudburrow/cloudburrow/internal/authplatform"
)

// Google Auth Platform's pages (#2030): calls of the serviceusage Studio
// over the local app and clients (internal/authplatform).
//
//   - oauth-app: the project's app (nil before Get started), its clients,
//     the endpoints a client uses, and the instance's user;
//   - oauth-app-set {brand}: Get started, and Branding, Audience and Data
//     Access's Save;
//   - oauth-clients {deleted}: Clients, or Deleted credentials;
//   - oauth-client {id}: a client's page;
//   - oauth-client-create {client}: Create client, which answers the
//     secret once;
//   - oauth-client-update {id, client}: Save;
//   - oauth-client-delete / oauth-client-restore {id};
//   - oauth-secret-add {id}; oauth-secret-set {id, secretId, action}.

// consoleOAuthClient is a client as the pages show it: the secrets' last
// four characters, never their values.
type consoleOAuthClient struct {
	*authplatform.Client
	TypeLabel string `json:"typeLabel"`
}

func toConsoleOAuth(c *authplatform.Client) consoleOAuthClient {
	cp := *c
	cp.Secrets = nil
	for _, s := range c.Secrets {
		s.Hash = "" // the page shows the last four characters only
		cp.Secrets = append(cp.Secrets, s)
	}
	return consoleOAuthClient{Client: &cp, TypeLabel: authplatform.TypeLabels[c.Type]}
}

func (p serviceUsageProvider) oauthCall(project, call string, body json.RawMessage) (any, error) {
	if p.auth == nil {
		return nil, errors.New("Google Auth Platform is not served by this instance")
	}
	var req struct {
		ID       string              `json:"id"`
		SecretID string              `json:"secretId"`
		Action   string              `json:"action"`
		Deleted  bool                `json:"deleted"`
		Brand    authplatform.Brand  `json:"brand"`
		Client   authplatform.Client `json:"client"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("bad request: %v", err)
		}
	}
	list := func(deleted bool) ([]consoleOAuthClient, error) {
		cs, err := p.auth.Clients(project, deleted)
		if err != nil {
			return nil, err
		}
		out := []consoleOAuthClient{}
		for _, c := range cs {
			out = append(out, toConsoleOAuth(c))
		}
		return out, nil
	}
	switch call {
	case "oauth-app":
		b, err := p.auth.Brand(project)
		if err != nil {
			return nil, err
		}
		cs, err := list(false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"project": project, "brand": b, "clients": cs, "types": authplatform.TypeLabels,
			"authorizePath": authplatform.AuthorizePath, "revokePath": authplatform.RevokePath, "base": p.oauthBase()}, nil
	case "oauth-app-set":
		return p.auth.SetBrand(project, req.Brand)
	case "oauth-clients":
		cs, err := list(req.Deleted)
		if err != nil {
			return nil, err
		}
		return map[string]any{"clients": cs}, nil
	case "oauth-client":
		c, err := p.auth.Client(project, req.ID)
		if err != nil {
			return nil, err
		}
		return toConsoleOAuth(c), nil
	case "oauth-client-create":
		c, secret, err := p.auth.CreateClient(project, req.Client)
		if err != nil {
			return nil, err
		}
		// The secret is answered once, to the page that created the
		// client, as Google's "OAuth client created" dialog shows it.
		return map[string]any{"client": toConsoleOAuth(c), "secret": secret}, nil
	case "oauth-client-update":
		c, err := p.auth.UpdateClient(project, req.ID, req.Client)
		if err != nil {
			return nil, err
		}
		return toConsoleOAuth(c), nil
	case "oauth-client-delete", "oauth-client-restore":
		return map[string]any{}, p.auth.DeleteClient(project, req.ID, call == "oauth-client-restore")
	case "oauth-secret-add":
		c, secret, err := p.auth.AddSecret(project, req.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"client": toConsoleOAuth(c), "secret": secret}, nil
	case "oauth-secret-set":
		c, err := p.auth.SetSecret(project, req.ID, req.SecretID, req.Action)
		if err != nil {
			return nil, err
		}
		return toConsoleOAuth(c), nil
	}
	return nil, fmt.Errorf("unknown call %q", call)
}

// oauthBase is the metadata server's URL, where the OAuth endpoints are.
func (p serviceUsageProvider) oauthBase() string {
	if p.metaAddr == nil || p.metaAddr() == "" {
		return ""
	}
	return "http://" + p.metaAddr()
}
