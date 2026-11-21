package main

// Document AI's analysis page (#2084). Google's Upload Test Document opens
// a page of its own: "You should now be on the layout parser analysis
// page. You can view the blocks or chunks parsed from the document,
// organized by detected types"
// (https://docs.cloud.google.com/document-ai/docs/layout-parse-quickstart,
// read 2026-11-20). Here each test document's result is kept, and opened at
// the processor's address with analysis/{id}: its document view (the page
// images with their layout drawn over them, the text and the elements), and
// a Chunks tab when the processor chunked it. ProcessDocument keeps nothing,
// so the analyses are this console's, held in memory (the last
// docAIAnalysesKept) until the instance stops, as Google's test results are
// not kept either once the page is left.

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// docAIAnalysisSegment is the path segment analyses are under.
const docAIAnalysisSegment = "analysis"

// docAIAnalysesKept bounds the analyses held: each carries its page images.
const docAIAnalysesKept = 20

type docAIAnalysis struct {
	id, project, processor, typ, file, version string
	when                                       time.Time
	result                                     *console.Listing
}

// docAIAnalyses are the console's test document results.
type docAIAnalyses struct {
	mu   sync.Mutex
	next int
	list []docAIAnalysis
}

// add keeps a result and returns the route of its analysis page.
func (a *docAIAnalyses) add(project, processor, typ, file, version string, res *console.Listing) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	id := strconv.Itoa(a.next)
	a.list = append(a.list, docAIAnalysis{id: id, project: project, processor: processor, typ: typ, file: path.Base(file),
		version: version, when: time.Now().UTC(), result: res})
	if len(a.list) > docAIAnalysesKept {
		a.list = a.list[len(a.list)-docAIAnalysesKept:]
	}
	return docAIAnalysisHref(project, processor, id)
}

func (a *docAIAnalyses) get(project, processor, id string) (docAIAnalysis, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.list {
		if x.id == id && x.project == project && x.processor == processor {
			return x, true
		}
	}
	return docAIAnalysis{}, false
}

func docAIAnalysisHref(project, processor, id string) string {
	return "/document-ai/processors/" + url.PathEscape(processor) + "/" + docAIAnalysisSegment + "/" + url.PathEscape(id) +
		"?project=" + url.QueryEscape(project)
}

// docAIAnalysisSections splits a result into the document view and, when
// the layout parser chunked it, its chunks.
func docAIAnalysisSections(res *console.Listing) []console.Section {
	doc := *res
	doc.Items, doc.Open = nil, ""
	chunks := console.Listing{Columns: []string{"Page", "Text"}, NameColumn: "Chunk", Noun: "chunks"}
	for _, it := range res.Items {
		if id, ok := strings.CutPrefix(it.Fields["Element"], "Chunk "); ok {
			chunks.Items = append(chunks.Items, console.Resource{Name: id, Fields: map[string]string{
				"Page": it.Fields["Page"], "Text": it.Fields["Text"]}})
			continue
		}
		doc.Items = append(doc.Items, it)
	}
	doc.Total = len(doc.Items)
	label := "Document"
	if len(chunks.Items) > 0 {
		label = "Blocks"
	}
	out := []console.Section{{ID: "document", Label: label, Kind: console.KindResult, Listing: doc}}
	if len(chunks.Items) > 0 {
		chunks.Total = len(chunks.Items)
		out = append(out, console.Section{ID: "chunks", Label: "Chunks", Listing: chunks,
			Note: "The chunks the layout parser split the document into, for retrieval, in order."})
	}
	return out
}

// analysisDetail is an analysis page.
func (p docAIProcessorsProvider) analysisDetail(project, processor, id string) console.Detail {
	project = p.d.project(project)
	if p.analyses == nil {
		return console.Detail{Unavailable: "this console keeps no analyses"}
	}
	a, ok := p.analyses.get(project, processor, id)
	if !ok {
		return console.Detail{Unavailable: fmt.Sprintf("analysis %s of %s not found: a test document's analysis is kept until the "+
			"instance stops, the last %d of them", id, processor, docAIAnalysesKept)}
	}
	version := "The processor's default"
	if strings.Contains(a.version, "/processorVersions/") {
		version = lastSegment(a.version)
	}
	procHref := "/document-ai/processors/" + url.PathEscape(processor) + "?project=" + url.QueryEscape(project)
	// Google's "layout parser analysis page".
	heading := docAITypeLabel(a.typ) + " analysis"
	if a.typ == "LAYOUT_PARSER_PROCESSOR" {
		heading = "Layout parser analysis"
	}
	return console.Detail{Heading: heading, Title: a.file,
		Trail: []console.Crumb{{Label: processor, Path: []string{processor}}, {Label: a.file}},
		Summary: []console.Property{{Label: "Processor", Value: processor, Link: procHref},
			{Label: "Processor version", Value: version}, {Label: "Document", Value: a.file},
			{Label: "Processed", Value: a.when.Format(time.RFC3339)}},
		Sections: docAIAnalysisSections(a.result)}
}
