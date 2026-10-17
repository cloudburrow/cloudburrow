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
		{Name: "format", Label: "Format", Type: "select", Options: []string{"DOCKER"}, Default: "DOCKER",
			Help: "Docker is the format served: images built and pushed on this instance are its artifacts."},
		{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1"},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "labels", Label: "Labels", Type: "text", Help: "optional: key=value pairs, comma separated"},
	}
}

// Create implements console.Creator: CreateRepository, a standard DOCKER
// repository.
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
	err = p.withClient(ctx, func(c *artifactregistry.Client) error {
		op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: "projects/" + project + "/locations/" + loc, RepositoryId: id,
			Repository: &arpb.Repository{Format: arpb.Repository_DOCKER, Description: strings.TrimSpace(v["description"]), Labels: labels}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
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
