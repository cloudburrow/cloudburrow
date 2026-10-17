package main

// Create repository and Edit repository on the Artifact Registry pages,
// through CreateRepository and UpdateRepository on the Run port: the
// console parity audit found both reachable only from gcloud.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (p artifactRegistryProvider) withClient(ctx context.Context, fn func(*artifactregistry.Client) error) error {
	addr := ""
	if p.runAddr != nil {
		addr = p.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter, which serves Artifact Registry, is not running")
	}
	c, err := artifactregistry.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Artifact Registry: %w", err)
	}
	defer c.Close()
	return fn(c)
}

// CreateForm implements console.Creator.
func (artifactRegistryProvider) CreateForm() (string, []console.Field) {
	return "Create repository", []console.Field{
		{Name: "id", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([a-z0-9\\-]{0,61}[a-z0-9])?$",
			Help: "Lowercase letters, digits and hyphens."},
		{Name: "format", Label: "Format", Type: "select", Options: []string{"DOCKER", "PYTHON", "NPM", "MAVEN", "GENERIC", "APT", "YUM"}, Default: "DOCKER",
			Help: "Each format serves its own protocol on the Run port; the repository's page says how its client reaches it."},
		{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1"},
		{Name: "mode", Label: "Mode", Type: "select", Options: []string{"STANDARD_REPOSITORY", "REMOTE_REPOSITORY", "VIRTUAL_REPOSITORY"}, Default: "STANDARD_REPOSITORY",
			Help: "A remote repository pulls through from, and caches, another repository of this instance (DOCKER, PYTHON, NPM, MAVEN); a virtual one reads its upstreams (PYTHON, NPM, MAVEN)."},
		{Name: "upstream", Label: "Upstream repositories", Type: "text",
			Help: "remote or virtual only: repository IDs in the region, or projects/*/locations/*/repositories/* names, comma separated; a remote repository takes one, a standard repository of its format"},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "labels", Label: "Labels", Type: "text", Help: "optional: key=value pairs, comma separated"},
	}
}

// Create implements console.Creator: CreateRepository, a standard
// repository of the chosen format.
func (p artifactRegistryProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		project = p.defaultProject
	}
	labels, err := parseAnnotations(v["labels"])
	if err != nil {
		return "", err
	}
	id, loc := strings.TrimSpace(v["id"]), strings.TrimSpace(v["location"])
	if id == "" || loc == "" {
		return "", errors.New("a name and a region are required")
	}
	format := arpb.Repository_DOCKER
	if f := strings.TrimSpace(v["format"]); f != "" {
		n, ok := arpb.Repository_Format_value[f]
		if !ok {
			return "", fmt.Errorf("unknown format %q", f)
		}
		format = arpb.Repository_Format(n)
	}
	parent := "projects/" + project + "/locations/" + loc
	repo := &arpb.Repository{Format: format, Description: strings.TrimSpace(v["description"]), Labels: labels}
	if err := setRepositoryMode(repo, parent, v["mode"], v["upstream"]); err != nil {
		return "", err
	}
	err = p.withClient(ctx, func(c *artifactregistry.Client) error {
		op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: repo})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

// setRepositoryMode sets a new repository's mode and its upstreams, from
// the create form: a remote repository's common_repository, the one
// upstream, or a virtual one's upstream policies, the first highest.
func setRepositoryMode(r *arpb.Repository, parent, mode, upstream string) error {
	var ups []string
	for _, u := range strings.Split(upstream, ",") {
		if u = strings.TrimSpace(u); u != "" {
			if !strings.HasPrefix(u, "projects/") {
				u = parent + "/repositories/" + u
			}
			ups = append(ups, u)
		}
	}
	switch strings.TrimSpace(mode) {
	case "", "STANDARD_REPOSITORY":
		if len(ups) > 0 {
			return errors.New("a standard repository has no upstream: choose a remote or virtual mode")
		}
	case "REMOTE_REPOSITORY":
		if len(ups) != 1 {
			return errors.New("a remote repository needs exactly one upstream repository")
		}
		r.Mode = arpb.Repository_REMOTE_REPOSITORY
		r.ModeConfig = &arpb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &arpb.RemoteRepositoryConfig{
			RemoteSource: &arpb.RemoteRepositoryConfig_CommonRepository{CommonRepository: &arpb.RemoteRepositoryConfig_CommonRemoteRepository{Uri: ups[0]}}}}
	case "VIRTUAL_REPOSITORY":
		if len(ups) == 0 {
			return errors.New("a virtual repository needs at least one upstream repository")
		}
		vc := &arpb.VirtualRepositoryConfig{}
		for i, u := range ups {
			vc.UpstreamPolicies = append(vc.UpstreamPolicies, &arpb.UpstreamPolicy{Id: u[strings.LastIndex(u, "/")+1:], Repository: u, Priority: int32(100 - i)})
		}
		r.Mode = arpb.Repository_VIRTUAL_REPOSITORY
		r.ModeConfig = &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: vc}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	return nil
}

// editRepositoryAction is Edit repository on a repository's page,
// prefilled with its description and labels.
func editRepositoryAction(r *arpb.Repository) console.Action {
	return console.Action{ID: "edit-repository", Label: "Edit repository", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: r.GetDescription()},
		{Name: "labels", Label: "Labels", Type: "text", Default: repoLabelsText(r.GetLabels()),
			Help: "key=value pairs, comma separated, replacing the repository's labels; empty clears them"}}}
}

func repoLabelsText(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// editRepository is UpdateRepository of the description and labels.
func (p artifactRegistryProvider) editRepository(ctx context.Context, name string, v map[string]string) error {
	labels, err := parseAnnotations(v["labels"])
	if err != nil {
		return err
	}
	return p.withClient(ctx, func(c *artifactregistry.Client) error {
		_, err := c.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{
			Repository: &arpb.Repository{Name: name, Description: strings.TrimSpace(v["description"]), Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels"}}})
		return err
	})
}

var _ console.Creator = artifactRegistryProvider{}
