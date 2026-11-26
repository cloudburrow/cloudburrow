package main

// Vertex ML Metadata's console page (#2205): the Metadata page of Model
// development, its artifacts, executions and contexts in a project and
// region's default metadata store, in Google's layout
// (https://docs.cloud.google.com/vertex-ai/docs/pipelines/lineage, read
// 2026-11-24):
//   - "go to the Metadata page", which "lists the artifacts that have been
//     created in the default metadata store"; "In the Region drop-down
//     list, select the region that your run was created in";
//   - "Click the Display name of an artifact to see its lineage graph": "A
//     static graph showing the artifacts and executions that are a part of
//     this lineage graph appears. Click an artifact or execution to learn
//     more about it."
//
// UNVERIFIED (re-read 2026-11-24, the lineage page and
// .../ml-metadata/managing-metadata, which does deletes "Use either REST or
// the Vertex AI SDK for Python"): the Executions and Contexts tabs, every
// list's columns, the pages' properties and their Metadata, Executions,
// Input artifacts, Output artifacts, Artifacts and Child contexts sections,
// the lineage drawn as a table of its artifacts and executions, and Delete
// with its confirmation are CloudBurrow's; Google's docs describe none of
// them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (v vertexAPI) metadata(ctx context.Context) (*aiplatform.MetadataClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewMetadataClient(ctx, v.opts()...)
}

// metadataProvider is one tab of the Metadata page: kind is "artifacts",
// "executions" or "contexts".
type metadataProvider struct {
	api  vertexAPI
	kind string
}

func (p metadataProvider) ID() string     { return "vertex-metadata-" + p.kind }
func (metadataProvider) Title() string    { return "Metadata" }
func (metadataProvider) ScopeKey() string { return colabRegionKey }
func metadataStoreOf(ctx context.Context, project string) string {
	return colabParent(ctx, project) + "/metadataStores/default"
}

// metadataRoute is a node's page: /agent-platform/metadata/{kind}/{id}.
func metadataRoute(ctx context.Context, name, project string) string {
	segs := strings.Split(name, "/")
	if len(segs) != 8 {
		return ""
	}
	return "/agent-platform/metadata/" + segs[6] + "/" + url.PathEscape(segs[7]) + "?project=" + url.QueryEscape(project) +
		"&" + colabRegionKey + "=" + url.QueryEscape(colabRegion(ctx))
}

func metadataArtifactState(s aiplatformpb.Artifact_State) string {
	switch s {
	case aiplatformpb.Artifact_PENDING:
		return "Pending"
	case aiplatformpb.Artifact_LIVE:
		return "Live"
	}
	return "—"
}

func metadataExecutionState(s aiplatformpb.Execution_State) string {
	switch s {
	case aiplatformpb.Execution_NEW:
		return "New"
	case aiplatformpb.Execution_RUNNING:
		return "Running"
	case aiplatformpb.Execution_COMPLETE:
		return "Complete"
	case aiplatformpb.Execution_FAILED:
		return "Failed"
	case aiplatformpb.Execution_CACHED:
		return "Cached"
	case aiplatformpb.Execution_CANCELLED:
		return "Cancelled"
	}
	return "—"
}

func metadataEventType(t aiplatformpb.Event_Type) string {
	switch t {
	case aiplatformpb.Event_INPUT:
		return "Input"
	case aiplatformpb.Event_OUTPUT:
		return "Output"
	}
	return "—"
}

func metadataJSON(s *structpb.Struct) string {
	if len(s.GetFields()) == 0 {
		return "{}"
	}
	b, err := json.MarshalIndent(s.AsMap(), "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func (p metadataProvider) noun() string { return p.kind }

func (p metadataProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: map[string]string{"artifacts": "Artifact ID", "executions": "Execution ID", "contexts": "Context ID"}[p.kind],
		Noun: p.noun(), Scope: colabScope(ctx),
		Note: "The " + p.noun() + " of the region's default metadata store, where Vertex AI Pipelines records each run."}
	switch p.kind {
	case "artifacts":
		l.Columns = []string{"Display name", "Type", "URI", "State", "Created"}
	case "executions":
		l.Columns = []string{"Display name", "Type", "State", "Created"}
	default:
		l.Columns = []string{"Display name", "Type", "Parent contexts", "Created"}
	}
	if project == "" {
		l.Prompt = "Metadata is listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.metadata(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	store := metadataStoreOf(ctx, project)
	switch p.kind {
	case "artifacts":
		for it := c.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store}); ; {
			a, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return l, err
			}
			l.Items = append(l.Items, console.Resource{Name: vertexLast(a.GetName()), Fields: map[string]string{
				"Display name": dashIfEmpty(a.GetDisplayName()), "Type": dashIfEmpty(a.GetSchemaTitle()), "URI": dashIfEmpty(a.GetUri()),
				"State": metadataArtifactState(a.GetState()), "Created": vertexTime(a.GetCreateTime())}})
		}
	case "executions":
		for it := c.ListExecutions(ctx, &aiplatformpb.ListExecutionsRequest{Parent: store}); ; {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return l, err
			}
			l.Items = append(l.Items, console.Resource{Name: vertexLast(e.GetName()), Fields: map[string]string{
				"Display name": dashIfEmpty(e.GetDisplayName()), "Type": dashIfEmpty(e.GetSchemaTitle()),
				"State": metadataExecutionState(e.GetState()), "Created": vertexTime(e.GetCreateTime())}})
		}
	default:
		for it := c.ListContexts(ctx, &aiplatformpb.ListContextsRequest{Parent: store}); ; {
			x, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return l, err
			}
			var parents []string
			for _, pc := range x.GetParentContexts() {
				parents = append(parents, vertexLast(pc))
			}
			l.Items = append(l.Items, console.Resource{Name: vertexLast(x.GetName()), Fields: map[string]string{
				"Display name": dashIfEmpty(x.GetDisplayName()), "Type": dashIfEmpty(x.GetSchemaTitle()),
				"Parent contexts": dashIfEmpty(strings.Join(parents, ", ")), "Created": vertexTime(x.GetCreateTime())}})
		}
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

// nodeListing is a listing of lineage nodes, each opening its page.
func nodeListing(noun string) console.Listing {
	return console.Listing{Columns: []string{"Display name", "Kind", "Type", "State"}, NameColumn: "ID", Noun: noun}
}

func artifactRow(ctx context.Context, a *aiplatformpb.Artifact, project string, extra map[string]string) console.Resource {
	f := map[string]string{"Display name": dashIfEmpty(a.GetDisplayName()), "Kind": "Artifact", "Type": dashIfEmpty(a.GetSchemaTitle()),
		"State": metadataArtifactState(a.GetState()), "URI": dashIfEmpty(a.GetUri())}
	for k, v := range extra {
		f[k] = v
	}
	return console.Resource{Name: vertexLast(a.GetName()), Link: metadataRoute(ctx, a.GetName(), project), Fields: f}
}

func executionRow(ctx context.Context, e *aiplatformpb.Execution, project string, extra map[string]string) console.Resource {
	f := map[string]string{"Display name": dashIfEmpty(e.GetDisplayName()), "Kind": "Execution", "Type": dashIfEmpty(e.GetSchemaTitle()),
		"State": metadataExecutionState(e.GetState())}
	for k, v := range extra {
		f[k] = v
	}
	return console.Resource{Name: vertexLast(e.GetName()), Link: metadataRoute(ctx, e.GetName(), project), Fields: f}
}

// lineageListing is a subgraph's artifacts and executions but self.
func lineageListing(ctx context.Context, g *aiplatformpb.LineageSubgraph, self, project string) console.Listing {
	l := nodeListing("artifacts and executions")
	for _, a := range g.GetArtifacts() {
		if a.GetName() != self {
			l.Items = append(l.Items, artifactRow(ctx, a, project, nil))
		}
	}
	for _, e := range g.GetExecutions() {
		l.Items = append(l.Items, executionRow(ctx, e, project, nil))
	}
	l.Total = len(l.Items)
	return l
}

func (p metadataProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.metadata(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	name := metadataStoreOf(ctx, project) + "/" + p.kind + "/" + path[0]
	switch p.kind {
	case "artifacts":
		return p.artifactDetail(ctx, c, project, name)
	case "executions":
		return p.executionDetail(ctx, c, project, name)
	}
	return p.contextDetail(ctx, c, project, name)
}

func (p metadataProvider) artifactDetail(ctx context.Context, c *aiplatform.MetadataClient, project, name string) (console.Detail, error) {
	a, err := c.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	d := console.Detail{Title: dashIfEmpty(a.GetDisplayName()), Summary: []console.Property{
		{Label: "Display name", Value: dashIfEmpty(a.GetDisplayName())},
		{Label: "Name", Value: a.GetName(), Copy: a.GetName()},
		{Label: "Type", Value: dashIfEmpty(a.GetSchemaTitle())},
		{Label: "Schema version", Value: dashIfEmpty(a.GetSchemaVersion())},
		{Label: "URI", Value: dashIfEmpty(a.GetUri()), Copy: a.GetUri()},
		{Label: "State", Value: metadataArtifactState(a.GetState())},
		{Label: "Description", Value: dashIfEmpty(a.GetDescription())},
		{Label: "Labels", Value: pipelineLabels(a.GetLabels())},
		{Label: "Created", Value: vertexTime(a.GetCreateTime())},
		{Label: "Updated", Value: vertexTime(a.GetUpdateTime())},
	}}
	lineage := console.Section{ID: "lineage", Label: "Lineage"}
	if g, err := c.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: name}); err != nil {
		lineage.Unavailable = err.Error()
	} else {
		lineage.Listing = lineageListing(ctx, g, name, project)
	}
	execs := console.Section{ID: "executions", Label: "Executions"}
	if g, err := c.QueryArtifactLineageSubgraph(ctx, &aiplatformpb.QueryArtifactLineageSubgraphRequest{Artifact: name, MaxHops: 1}); err != nil {
		execs.Unavailable = err.Error()
	} else {
		byName := map[string]*aiplatformpb.Execution{}
		for _, e := range g.GetExecutions() {
			byName[e.GetName()] = e
		}
		l := nodeListing("executions")
		l.Columns = []string{"Display name", "Event", "Type", "State"}
		for _, ev := range g.GetEvents() {
			if e := byName[ev.GetExecution()]; e != nil && ev.GetArtifact() == name {
				l.Items = append(l.Items, executionRow(ctx, e, project, map[string]string{"Event": metadataEventType(ev.GetType())}))
			}
		}
		l.Total = len(l.Items)
		execs.Listing = l
		execs.Note = "An Output event is the execution that produced this artifact; an Input event one that consumed it."
	}
	d.Sections = []console.Section{lineage, execs, {ID: "metadata", Label: "Metadata", Kind: console.KindText, Text: metadataJSON(a.GetMetadata())}}
	return d, nil
}

func (p metadataProvider) executionDetail(ctx context.Context, c *aiplatform.MetadataClient, project, name string) (console.Detail, error) {
	e, err := c.GetExecution(ctx, &aiplatformpb.GetExecutionRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	d := console.Detail{Title: dashIfEmpty(e.GetDisplayName()), Summary: []console.Property{
		{Label: "Display name", Value: dashIfEmpty(e.GetDisplayName())},
		{Label: "Name", Value: e.GetName(), Copy: e.GetName()},
		{Label: "Type", Value: dashIfEmpty(e.GetSchemaTitle())},
		{Label: "Schema version", Value: dashIfEmpty(e.GetSchemaVersion())},
		{Label: "State", Value: metadataExecutionState(e.GetState())},
		{Label: "Description", Value: dashIfEmpty(e.GetDescription())},
		{Label: "Labels", Value: pipelineLabels(e.GetLabels())},
		{Label: "Created", Value: vertexTime(e.GetCreateTime())},
		{Label: "Updated", Value: vertexTime(e.GetUpdateTime())},
	}}
	in := console.Section{ID: "inputs", Label: "Input artifacts"}
	out := console.Section{ID: "outputs", Label: "Output artifacts"}
	if g, err := c.QueryExecutionInputsAndOutputs(ctx, &aiplatformpb.QueryExecutionInputsAndOutputsRequest{Execution: name}); err != nil {
		in.Unavailable, out.Unavailable = err.Error(), err.Error()
	} else {
		byName := map[string]*aiplatformpb.Artifact{}
		for _, a := range g.GetArtifacts() {
			byName[a.GetName()] = a
		}
		li, lo := nodeListing("input artifacts"), nodeListing("output artifacts")
		li.Columns = []string{"Display name", "Type", "URI", "State"}
		lo.Columns = li.Columns
		for _, ev := range g.GetEvents() {
			a := byName[ev.GetArtifact()]
			if a == nil {
				continue
			}
			switch ev.GetType() {
			case aiplatformpb.Event_INPUT:
				li.Items = append(li.Items, artifactRow(ctx, a, project, nil))
			default:
				lo.Items = append(lo.Items, artifactRow(ctx, a, project, nil))
			}
		}
		li.Total, lo.Total = len(li.Items), len(lo.Items)
		in.Listing, out.Listing = li, lo
	}
	d.Sections = []console.Section{in, out, {ID: "metadata", Label: "Metadata", Kind: console.KindText, Text: metadataJSON(e.GetMetadata())}}
	return d, nil
}

func (p metadataProvider) contextDetail(ctx context.Context, c *aiplatform.MetadataClient, project, name string) (console.Detail, error) {
	x, err := c.GetContext(ctx, &aiplatformpb.GetContextRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	var parents []string
	for _, pc := range x.GetParentContexts() {
		parents = append(parents, vertexLast(pc))
	}
	d := console.Detail{Title: dashIfEmpty(x.GetDisplayName()), Summary: []console.Property{
		{Label: "Display name", Value: dashIfEmpty(x.GetDisplayName())},
		{Label: "Name", Value: x.GetName(), Copy: x.GetName()},
		{Label: "Type", Value: dashIfEmpty(x.GetSchemaTitle())},
		{Label: "Schema version", Value: dashIfEmpty(x.GetSchemaVersion())},
		{Label: "Parent contexts", Value: dashIfEmpty(strings.Join(parents, ", "))},
		{Label: "Description", Value: dashIfEmpty(x.GetDescription())},
		{Label: "Labels", Value: pipelineLabels(x.GetLabels())},
		{Label: "Created", Value: vertexTime(x.GetCreateTime())},
		{Label: "Updated", Value: vertexTime(x.GetUpdateTime())},
	}}
	store := metadataStoreOf(ctx, project)
	in := fmt.Sprintf("in_context(%q)", name)
	arts := console.Section{ID: "artifacts", Label: "Artifacts"}
	la := nodeListing("artifacts")
	la.Columns = []string{"Display name", "Type", "URI", "State"}
	for it := c.ListArtifacts(ctx, &aiplatformpb.ListArtifactsRequest{Parent: store, Filter: in}); ; {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			arts.Unavailable = err.Error()
			break
		}
		la.Items = append(la.Items, artifactRow(ctx, a, project, nil))
	}
	la.Total = len(la.Items)
	arts.Listing = la
	execs := console.Section{ID: "executions", Label: "Executions"}
	le := nodeListing("executions")
	le.Columns = []string{"Display name", "Type", "State"}
	for it := c.ListExecutions(ctx, &aiplatformpb.ListExecutionsRequest{Parent: store, Filter: in}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			execs.Unavailable = err.Error()
			break
		}
		le.Items = append(le.Items, executionRow(ctx, e, project, nil))
	}
	le.Total = len(le.Items)
	execs.Listing = le
	kids := console.Section{ID: "children", Label: "Child contexts"}
	lk := nodeListing("child contexts")
	lk.Columns = []string{"Display name", "Type"}
	for it := c.ListContexts(ctx, &aiplatformpb.ListContextsRequest{Parent: store, Filter: fmt.Sprintf("parent_contexts:%q", name)}); ; {
		k, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			kids.Unavailable = err.Error()
			break
		}
		lk.Items = append(lk.Items, console.Resource{Name: vertexLast(k.GetName()), Link: metadataRoute(ctx, k.GetName(), project),
			Fields: map[string]string{"Display name": dashIfEmpty(k.GetDisplayName()), "Type": dashIfEmpty(k.GetSchemaTitle())}})
	}
	lk.Total = len(lk.Items)
	kids.Listing = lk
	d.Sections = []console.Section{arts, execs, kids, {ID: "metadata", Label: "Metadata", Kind: console.KindText, Text: metadataJSON(x.GetMetadata())}}
	return d, nil
}

func (p metadataProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.metadata(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	full := metadataStoreOf(ctx, project) + "/" + p.kind + "/" + name
	switch p.kind {
	case "artifacts":
		op, err := c.DeleteArtifact(ctx, &aiplatformpb.DeleteArtifactRequest{Name: full})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	case "executions":
		op, err := c.DeleteExecution(ctx, &aiplatformpb.DeleteExecutionRequest{Name: full})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	op, err := c.DeleteContext(ctx, &aiplatformpb.DeleteContextRequest{Name: full})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}
