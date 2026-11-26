package main

// Cloud Build repositories, 2nd gen (#2197), on Repositories' 2nd gen tab:
// the project's host connections (cloudbuild.googleapis.com v2) and the
// repositories linked through them, beside the Developer Connect
// connections to this instance's Secure Source Manager the tab already
// listed (consolecloudbuild.go). Google's "Connect to a GitLab host",
// "Connect to a Bitbucket Data Center host" and "Connect to a GitHub
// Enterprise host" pages document Create host connection: a region, a
// name, a provider, the host URL and the access tokens, which the console
// stores in Secret Manager for the connection, with a webhook secret it
// makes; then Link repository picks one of the host's repositories. The
// host is this instance's git server (runadapter.LocalGitHost), as no
// hosted provider is reachable; GitHub (github.com) makes a connection
// that waits for authorization, as Google's does before OAuth.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	cloudbuildv2 "cloud.google.com/go/cloudbuild/apiv2"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cb2Providers are Create host connection's providers, as Google's form
// names them.
var cb2Providers = []string{"GitLab", "Bitbucket Data Center", "GitHub Enterprise", "GitHub"}

// withRepoManager dials Cloud Build v2 on the Run port as an SDK would.
func (p buildRepositoriesProvider) withRepoManager(ctx context.Context, fn func(*cloudbuildv2.RepositoryManagerClient) error) error {
	addr := ""
	if p.ssm.b.runAddr != nil {
		addr = p.ssm.b.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := cloudbuildv2.NewRepositoryManagerClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Cloud Build: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// storeSecret keeps value in a new Secret Manager secret of the project,
// as Google's Connect does with the tokens it is given, and names its
// first version.
func (p buildRepositoriesProvider) storeSecret(ctx context.Context, project, id, value string) (string, error) {
	addr := ""
	if p.secretsAddr != nil {
		addr = p.secretsAddr()
	}
	if addr == "" {
		return "", errors.New("Secret Manager is not enabled: a connection's tokens are stored there")
	}
	c, err := secretmanager.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	parent := "projects/" + project
	if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		return "", fmt.Errorf("store secret %s: %w", id, err)
	}
	v, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + id,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)}})
	if err != nil {
		return "", fmt.Errorf("store secret %s: %w", id, err)
	}
	return v.GetName(), nil
}

// cb2ProviderName is a connection's provider, as the tab shows it.
func cb2ProviderName(c *cb2pb.Connection) string {
	switch {
	case c.GetGithubConfig() != nil:
		return "GitHub"
	case c.GetGithubEnterpriseConfig() != nil:
		return "GitHub Enterprise"
	case c.GetGitlabConfig() != nil:
		return "GitLab"
	case c.GetBitbucketDataCenterConfig() != nil:
		return "Bitbucket Data Center"
	case c.GetBitbucketCloudConfig() != nil:
		return "Bitbucket Cloud"
	}
	return "-"
}

// cb2Status is a connection's status column.
func cb2Status(c *cb2pb.Connection) string {
	switch {
	case c.GetDisabled():
		return "Disabled"
	case c.GetInstallationState().GetStage() == cb2pb.InstallationState_COMPLETE:
		return "Installed"
	}
	return "Pending"
}

// cb2Items are the tab's rows of Cloud Build connections and repositories.
func cb2Items(project string) []console.Resource {
	var out []console.Resource
	for _, c := range runadapter.CloudBuildConnections(project) {
		host := cb2ProviderName(c) + ": " + runadapter.ConnectionHost(c)
		out = append(out, console.Resource{Name: lastSegment(c.GetName()), Status: cb2Status(c), Fields: map[string]string{
			"Type": "Host connection", "Region": locationOf(c.GetName()), "Host": host, "Connection": "-", "Clone URI": "-",
			"Age": stampAge(c.GetCreateTime())}, Actions: []console.Action{{ID: "delete-cb2-connection", Label: "Delete",
			Destructive: true, Confirm: "the host connection and its linked repositories"}},
			Target: []string{"cb2connection", c.GetName()}})
		for _, r := range runadapter.CloudBuildRepositories(c.GetName()) {
			out = append(out, console.Resource{Name: lastSegment(r.GetName()), Status: "Linked", Fields: map[string]string{
				"Type": "Repository", "Region": locationOf(c.GetName()), "Host": host, "Connection": lastSegment(c.GetName()),
				"Clone URI": r.GetRemoteUri(), "Age": stampAge(r.GetCreateTime())}, Actions: []console.Action{{ID: "delete-cb2-repository",
				Label: "Delete", Destructive: true, Confirm: "the linked repository; triggers on it stop running"}},
				Target: []string{"cb2repository", r.GetName()}})
		}
	}
	return out
}

// cb2CreateFields are Create host connection's fields for the Cloud Build
// providers, each shown for the providers it applies to.
func cb2CreateFields() []console.Field {
	tokens := "provider=GitLab|Bitbucket Data Center"
	return []console.Field{
		{Name: "region", Label: "Region", Type: "text", Default: "us-central1", Pattern: "^[a-z]+-[a-z]+[0-9]+$",
			ShowWhen: "provider=" + strings.Join(cb2Providers, "|"), Help: "Connections are regional; triggers on their repositories run in the same region."},
		{Name: "host", Label: "Host URL", Type: "text", Default: runadapter.LocalGitHost(),
			ShowWhen: "provider=GitLab|Bitbucket Data Center|GitHub Enterprise",
			Help:     "This instance's git server, whose repositories are its Secure Source Manager repositories."},
		{Name: "apiToken", Label: "API access token", Type: "password", ShowWhen: tokens,
			Help: "Stored in Secret Manager for the connection. GitLab: a token with the api scope; Bitbucket: REPO_ADMIN."},
		{Name: "readToken", Label: "Read API access token", Type: "password", ShowWhen: tokens,
			Help: "Stored in Secret Manager for the connection. GitLab: a token with the read_api scope; Bitbucket: REPO_READ."},
		{Name: "apiKey", Label: "API key", Type: "text", ShowWhen: "provider=GitHub Enterprise"},
		{Name: "appId", Label: "App ID", Type: "text", Pattern: "^[0-9]*$", ShowWhen: "provider=GitHub Enterprise"},
		{Name: "appSlug", Label: "App slug", Type: "text", ShowWhen: "provider=GitHub Enterprise"},
		{Name: "installationId", Label: "Installation ID", Type: "text", Pattern: "^[0-9]*$", ShowWhen: "provider=GitHub Enterprise"},
		{Name: "privateKey", Label: "Private key", Type: "textarea", ShowWhen: "provider=GitHub Enterprise",
			Help: "The GitHub App's private key, stored in Secret Manager for the connection."},
	}
}

// createCB2Connection makes a Cloud Build connection from the form.
func (p buildRepositoriesProvider) createCB2Connection(ctx context.Context, project string, v map[string]string) error {
	id, region := v["id"], v["region"]
	if id == "" || region == "" {
		return errors.New("a name and a region are required")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	secret := func(kind, value string) (string, error) {
		if value == "" {
			return "", fmt.Errorf("the %s is required", strings.ReplaceAll(kind, "-", " "))
		}
		return p.storeSecret(ctx, project, id+"-"+kind+"-"+hex.EncodeToString(random[:4]), value)
	}
	conn := &cb2pb.Connection{}
	switch v["provider"] {
	case "GitHub":
		conn.ConnectionConfig = &cb2pb.Connection_GithubConfig{GithubConfig: &cb2pb.GitHubConfig{}}
	case "GitLab", "Bitbucket Data Center":
		hook, err := secret("webhook-secret", hex.EncodeToString(random))
		if err != nil {
			return err
		}
		api, err := secret("api-token", v["apiToken"])
		if err != nil {
			return err
		}
		read, err := secret("read-api-token", v["readToken"])
		if err != nil {
			return err
		}
		if v["provider"] == "GitLab" {
			conn.ConnectionConfig = &cb2pb.Connection_GitlabConfig{GitlabConfig: &cb2pb.GitLabConfig{HostUri: v["host"],
				WebhookSecretSecretVersion: hook, AuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: api},
				ReadAuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: read}}}
		} else {
			conn.ConnectionConfig = &cb2pb.Connection_BitbucketDataCenterConfig{BitbucketDataCenterConfig: &cb2pb.BitbucketDataCenterConfig{
				HostUri: v["host"], WebhookSecretSecretVersion: hook, AuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: api},
				ReadAuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: read}}}
		}
	case "GitHub Enterprise":
		g := &cb2pb.GitHubEnterpriseConfig{HostUri: v["host"], ApiKey: v["apiKey"], AppSlug: v["appSlug"]}
		var err error
		if s := v["appId"]; s != "" {
			if g.AppId, err = strconv.ParseInt(s, 10, 64); err != nil {
				return fmt.Errorf("app ID %q is not a number", s)
			}
		}
		if s := v["installationId"]; s != "" {
			if g.AppInstallationId, err = strconv.ParseInt(s, 10, 64); err != nil {
				return fmt.Errorf("installation ID %q is not a number", s)
			}
		}
		if v["privateKey"] != "" {
			if g.PrivateKeySecretVersion, err = secret("private-key", v["privateKey"]); err != nil {
				return err
			}
			if g.WebhookSecretSecretVersion, err = secret("webhook-secret", hex.EncodeToString(random)); err != nil {
				return err
			}
		}
		conn.ConnectionConfig = &cb2pb.Connection_GithubEnterpriseConfig{GithubEnterpriseConfig: g}
	default:
		return fmt.Errorf("unknown provider %q", v["provider"])
	}
	return p.withRepoManager(ctx, func(c *cloudbuildv2.RepositoryManagerClient) error {
		op, err := c.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/" + region,
			ConnectionId: id, Connection: conn})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// isCB2Connection reports whether name is one of the project's Cloud
// Build connections, and whether it is installed.
func isCB2Connection(project, name string) (found, installed bool) {
	for _, c := range runadapter.CloudBuildConnections(project) {
		if c.GetName() == name {
			return true, cb2Status(c) == "Installed"
		}
	}
	return false, false
}

// linkCB2Repository links a repository through a Cloud Build connection.
func (p buildRepositoriesProvider) linkCB2Repository(ctx context.Context, v map[string]string) error {
	return p.withRepoManager(ctx, func(c *cloudbuildv2.RepositoryManagerClient) error {
		remote := runadapter.SourceManagerCloneURI(v["repository"])
		if remote == "" {
			return fmt.Errorf("repository %s not found", v["repository"])
		}
		op, err := c.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: v["connection"], RepositoryId: v["id"],
			Repository: &cb2pb.Repository{RemoteUri: remote}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// deleteCB2 deletes a Cloud Build connection or repository.
func (p buildRepositoriesProvider) deleteCB2(ctx context.Context, action, name string) error {
	return p.withRepoManager(ctx, func(c *cloudbuildv2.RepositoryManagerClient) error {
		if action == "delete-cb2-connection" {
			op, err := c.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: name})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		}
		op, err := c.DeleteRepository(ctx, &cb2pb.DeleteRepositoryRequest{Name: name})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}
