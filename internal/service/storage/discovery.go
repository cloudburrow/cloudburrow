package storage

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// storageAPI is Google's discovery document for the Cloud Storage JSON API,
// copied unchanged from google.golang.org/api v0.298.0 (storage/v1,
// revision 20260821; BSD-3-Clause, the module's licence).
// TestStorageDiscoveryDrift keeps it equal to the pinned module's copy.
//
//go:embed storage-api.json
var storageAPI []byte

// method is one discovery method: its ID ("storage.buckets.list"), HTTP
// method and path under /storage/v1/.
type method struct {
	ID     string
	Verb   string
	Path   string
	Upload bool // supportsMediaUpload
	re     *regexp.Regexp
}

type discoveryDoc struct {
	Resources map[string]discoveryResource `json:"resources"`
}

type discoveryResource struct {
	Methods   map[string]discoveryMethod   `json:"methods"`
	Resources map[string]discoveryResource `json:"resources"`
}

type discoveryMethod struct {
	ID                  string `json:"id"`
	HTTPMethod          string `json:"httpMethod"`
	Path                string `json:"path"`
	SupportsMediaUpload bool   `json:"supportsMediaUpload"`
}

// discoveryMethods parses the embedded document, most specific path first
// (more literal segments), so b/{bucket}/o/{object}/acl beats b/{bucket}/o/{object}.
func discoveryMethods() ([]method, error) {
	var doc discoveryDoc
	if err := json.Unmarshal(storageAPI, &doc); err != nil {
		return nil, fmt.Errorf("parse storage discovery document: %w", err)
	}
	var out []method
	var walk func(map[string]discoveryResource)
	walk = func(res map[string]discoveryResource) {
		for _, r := range res {
			for _, m := range r.Methods {
				out = append(out, method{ID: m.ID, Verb: m.HTTPMethod, Path: m.Path, Upload: m.SupportsMediaUpload, re: pathRE(m.Path)})
			}
			walk(r.Resources)
		}
	}
	walk(doc.Resources)
	sort.Slice(out, func(i, j int) bool {
		li, lj := literals(out[i].Path), literals(out[j].Path)
		if li != lj {
			return li > lj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// literals counts a path template's fixed segments.
func literals(p string) int {
	n := 0
	for _, s := range strings.Split(p, "/") {
		if !strings.HasPrefix(s, "{") {
			n++
		}
	}
	return n
}

// pathRE compiles a discovery path. A {variable} is one escaped path segment:
// clients percent-encode an object name, "/" included, so an object name is
// one segment of the escaped path.
func pathRE(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i, seg := range strings.Split(p, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			b.WriteString("[^/]+")
		} else {
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
