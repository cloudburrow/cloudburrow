package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/storagejobs"
)

// insightsLinks makes Storage Insights linked datasets (#1976) in the
// instance's BigQuery. Its requests carry the linked writer header, as the
// Logging server's do, so the front keeps the datasets read-only to every
// other caller and leaves the tables the views read out of tables.list
// (bigqueryfront/loglinks.go).
type insightsLinks struct{ tunnel func() *netfwd.Forwarder }

// insightsLinkLabel marks a Storage Insights linked dataset.
const insightsLinkLabel = "cloudburrow_storage_insights_link"

func (l insightsLinks) client(ctx context.Context, project string) (*bigquery.Client, error) {
	f := l.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("BigQuery has not started on this instance")
	}
	hc := &http.Client{Transport: linkedTransport{next: http.DefaultTransport}, Timeout: 2 * time.Minute}
	return bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithHTTPClient(hc))
}

func (l insightsLinks) CreateDataset(ctx context.Context, project, dataset, location, description string) error {
	c, err := l.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	err = c.Dataset(dataset).Create(ctx, &bigquery.DatasetMetadata{Location: location, Description: description,
		Labels: map[string]string{insightsLinkLabel: "true"}})
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusConflict {
		return fmt.Errorf("%s:%s: %w", project, dataset, storagejobs.ErrDatasetExists)
	}
	return err
}

func toBQSchema(fs []storagejobs.Field) bigquery.Schema {
	out := make(bigquery.Schema, 0, len(fs))
	for _, f := range fs {
		out = append(out, &bigquery.FieldSchema{Name: f.Name, Type: bigquery.FieldType(f.Type), Repeated: f.Repeated,
			Schema: toBQSchema(f.Fields)})
	}
	return out
}

func (l insightsLinks) ReplaceTable(ctx context.Context, project, dataset string, t storagejobs.LinkedTable, rows []map[string]any) error {
	c, err := l.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	tb := c.Dataset(dataset).Table(t.ID)
	if err := tb.Delete(ctx); err != nil && !isNotFound(err) {
		return err
	}
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: toBQSchema(t.Schema), Description: t.Description}); err != nil {
		return err
	}
	for len(rows) > 0 {
		n := min(len(rows), 500)
		savers := make([]bigquery.ValueSaver, n)
		for i := range savers {
			savers[i] = sinkRow{values: rows[i]}
		}
		if err := tb.Inserter().Put(ctx, savers); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

func (l insightsLinks) CreateView(ctx context.Context, project, dataset string, v storagejobs.LinkedView) error {
	c, err := l.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Dataset(dataset).Table(v.ID).Create(ctx, &bigquery.TableMetadata{ViewQuery: v.Query, Description: v.Description})
}

func (l insightsLinks) DeleteDataset(ctx context.Context, project, dataset string) error {
	c, err := l.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Dataset(dataset).DeleteWithContents(ctx); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// insightsProjects is the project registry, for a dataset
// configuration's scope.
type insightsProjects struct{ r *resourcemanager.Registry }

func (p insightsProjects) info(pr resourcemanager.Project) storagejobs.ProjectInfo {
	n, _ := p.r.NumberOf(pr.ProjectID)
	out := storagejobs.ProjectInfo{ID: pr.ProjectID, Name: pr.DisplayName, Number: n}
	if out.Name == "" {
		out.Name = pr.ProjectID
	}
	anc, _ := p.r.Ancestry(pr.ProjectID)
	for _, a := range anc {
		switch a.Type {
		case "folder":
			out.Folders = append(out.Folders, a.ID)
		case "organization":
			out.Organization = a.ID
		}
	}
	return out
}

func (p insightsProjects) ListProjects(context.Context) ([]storagejobs.ProjectInfo, error) {
	ps, err := p.r.List()
	if err != nil {
		return nil, err
	}
	var out []storagejobs.ProjectInfo
	for _, pr := range ps {
		if pr.State != resourcemanager.StateActive {
			continue
		}
		out = append(out, p.info(pr))
	}
	return out, nil
}

func (p insightsProjects) Project(ctx context.Context, ref string) (storagejobs.ProjectInfo, error) {
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		all, err := p.ListProjects(ctx)
		if err != nil {
			return storagejobs.ProjectInfo{}, err
		}
		for _, x := range all {
			if strconv.FormatInt(x.Number, 10) == ref {
				return x, nil
			}
		}
		return storagejobs.ProjectInfo{}, fmt.Errorf("project %s not found", ref)
	}
	pr, err := p.r.EnsureExists(ref)
	if err != nil {
		return storagejobs.ProjectInfo{}, err
	}
	return p.info(pr), nil
}
