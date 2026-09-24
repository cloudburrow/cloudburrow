// Command coverage generates per-service API coverage from the proto surface
// (#288).
//
// Every RPC of the services CloudBurrow serves is listed from the Go proto
// descriptors, not from a hand-written table, and classified:
//
//   - Verified: a compat test annotated `// covers: <Service>/<Method>`
//     exercises it through an official SDK.
//   - Unimplemented: the registry says so, and servers_test.go proves the
//     in-process server returns codes.Unimplemented for it.
//   - Implemented: the registry says CloudBurrow implements it, and no
//     official-SDK test proves it yet.
//   - Not served: the service is not registered on any port.
//   - Unknown: an upstream emulator's method no annotated test covers.
//
// Storage's JSON API has no proto the Go client exposes, so its methods come
// from a fixed list, and only annotations can make one anything but Unknown.
//
//	go run ./tools/coverage          # write docs/coverage
//	go run ./tools/coverage -check   # fail if docs/coverage is stale or an annotation is wrong
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	_ "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	_ "cloud.google.com/go/run/apiv2/runpb"
	_ "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// area is one generated page.
type area struct {
	Key, Title string
	// Own is true for services CloudBurrow implements: every method must
	// have a registry entry. Upstream-backed ones are classified by
	// annotations alone.
	Own bool
	// Services are fully qualified proto service names, or, with Package,
	// every service in that package.
	Services []string
	Package  string
	// Methods, for an area with no proto (Storage's JSON API).
	Methods []string
}

var areas = []area{
	{Key: "tasks", Title: "Cloud Tasks", Own: true, Services: []string{"google.cloud.tasks.v2.CloudTasks"}},
	{Key: "secretmanager", Title: "Secret Manager", Own: true, Services: []string{"google.cloud.secretmanager.v1.SecretManagerService"}},
	{Key: "run", Title: "Cloud Run v2", Own: true, Package: "google.cloud.run.v2"},
	{Key: "pubsub", Title: "Pub/Sub", Services: []string{"google.pubsub.v1.Publisher", "google.pubsub.v1.Subscriber"}},
	{Key: "storage", Title: "Cloud Storage (JSON API)", Methods: storageMethods},
}

// storageMethods is the JSON API surface, from its discovery document.
var storageMethods = []string{
	"storage.buckets.insert", "storage.buckets.get", "storage.buckets.list", "storage.buckets.patch",
	"storage.buckets.update", "storage.buckets.delete", "storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy",
	"storage.buckets.testIamPermissions", "storage.buckets.lockRetentionPolicy",
	"storage.objects.insert", "storage.objects.get", "storage.objects.list", "storage.objects.patch",
	"storage.objects.update", "storage.objects.delete", "storage.objects.copy", "storage.objects.rewrite",
	"storage.objects.compose", "storage.objects.watchAll", "storage.objects.restore",
	"storage.notifications.insert", "storage.notifications.get", "storage.notifications.list", "storage.notifications.delete",
	"storage.objectAccessControls.list", "storage.bucketAccessControls.list", "storage.defaultObjectAccessControls.list",
	"storage.hmacKeys.create", "storage.hmacKeys.list", "storage.serviceAccount.get", "storage.channels.stop",
}

// Registry statuses.
const (
	regImplemented   = "implemented"
	regUnimplemented = "unimplemented"
	regNotServed     = "not-served"
)

// Row is one method's classification.
type Row struct {
	Method string   `json:"method"`
	Status string   `json:"status"`
	Tests  []string `json:"tests,omitempty"` // file.go:line FuncName
	Note   string   `json:"note,omitempty"`
}

// Page is one area's rows.
type Page struct {
	Key, Title string
	Rows       []Row
}

func main() {
	check := flag.Bool("check", false, "fail instead of writing when docs/coverage is stale")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	pages, err := build(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverage:", err)
		os.Exit(1)
	}
	files := render(pages)
	stale := 0
	for name, want := range files {
		path := filepath.Join(*root, "docs", "coverage", name)
		if *check {
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				fmt.Fprintf(os.Stderr, "coverage: %s is stale; run `go run ./tools/coverage` and commit it\n", path)
				stale++
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "coverage:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "coverage:", err)
			os.Exit(1)
		}
	}
	if stale > 0 {
		os.Exit(1)
	}
}

// methodsOf lists an area's methods, as Service/Method.
func methodsOf(a area) ([]string, error) {
	if a.Methods != nil {
		return append([]string(nil), a.Methods...), nil
	}
	var sds []protoreflect.ServiceDescriptor
	for _, name := range a.Services {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sds = append(sds, d.(protoreflect.ServiceDescriptor))
	}
	if a.Package != "" {
		protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(a.Package), func(f protoreflect.FileDescriptor) bool {
			for i := 0; i < f.Services().Len(); i++ {
				sds = append(sds, f.Services().Get(i))
			}
			return true
		})
	}
	var out []string
	for _, sd := range sds {
		for i := 0; i < sd.Methods().Len(); i++ {
			out = append(out, string(sd.FullName())+"/"+string(sd.Methods().Get(i).Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// annotation is one `// covers:` claim.
type annotation struct {
	method, test string
	refused      bool // "(unimplemented)": the test proves it is refused
}

var coversLine = regexp.MustCompile(`^//\s*covers:\s*(.+)$`)

// annotations reads every `// covers:` line in test/compat. Each must be in
// the doc comment of a Test function: one anywhere else is an error, since
// it would claim coverage for no test at all.
func annotations(root string) ([]annotation, error) {
	files, err := filepath.Glob(filepath.Join(root, "test", "compat", "*_test.go"))
	if err != nil {
		return nil, err
	}
	var out []annotation
	var problems []string
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		attached := map[*ast.Comment]bool{}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				m := coversLine.FindStringSubmatch(c.Text)
				if m == nil {
					continue
				}
				attached[c] = true
				if !strings.HasPrefix(fn.Name.Name, "Test") {
					problems = append(problems, fmt.Sprintf("%s: covers: on %s, which is not a test", fset.Position(c.Pos()), fn.Name.Name))
					continue
				}
				pos := fset.Position(fn.Pos())
				ref := fmt.Sprintf("test/compat/%s:%d %s", filepath.Base(f), pos.Line, fn.Name.Name)
				for _, item := range strings.Split(m[1], ",") {
					item = strings.TrimSpace(item)
					refused := strings.HasSuffix(item, "(unimplemented)")
					item = strings.TrimSpace(strings.TrimSuffix(item, "(unimplemented)"))
					out = append(out, annotation{method: item, test: ref, refused: refused})
				}
			}
		}
		for _, g := range file.Comments {
			for _, c := range g.List {
				if coversLine.MatchString(c.Text) && !attached[c] {
					problems = append(problems, fmt.Sprintf("%s: covers: outside a test function's doc comment", fset.Position(c.Pos())))
				}
			}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("bad annotations:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

func loadRegistry(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "tools", "coverage", "registry.json"))
	if err != nil {
		return nil, err
	}
	var r map[string]string
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("registry.json: %w", err)
	}
	return r, nil
}

func build(root string) ([]Page, error) {
	reg, err := loadRegistry(root)
	if err != nil {
		return nil, err
	}
	anns, err := annotations(root)
	if err != nil {
		return nil, err
	}
	byMethod := map[string][]annotation{}
	for _, a := range anns {
		byMethod[a.method] = append(byMethod[a.method], a)
	}

	var problems []string
	known := map[string]bool{}
	var pages []Page
	for _, a := range areas {
		methods, err := methodsOf(a)
		if err != nil {
			return nil, err
		}
		p := Page{Key: a.Key, Title: a.Title}
		for _, m := range methods {
			known[m] = true
			row := Row{Method: m}
			var tests []string
			refusedOnly := true
			for _, an := range byMethod[m] {
				tests = append(tests, an.test)
				refusedOnly = refusedOnly && an.refused
			}
			sort.Strings(tests)
			row.Tests = tests
			status, inReg := reg[m]
			switch {
			case a.Own && !inReg:
				problems = append(problems, fmt.Sprintf("%s has no registry entry: classify it in tools/coverage/registry.json", m))
			case !a.Own && inReg:
				problems = append(problems, fmt.Sprintf("%s is upstream-backed: only an annotated compat test can classify it", m))
			}
			switch {
			case status == regNotServed:
				row.Status, row.Note = "Not served", "the service is not registered on any port"
			case status == regUnimplemented:
				row.Status, row.Note = "Unimplemented", "returns UNIMPLEMENTED (in-process check)"
				for _, an := range byMethod[m] {
					if !an.refused {
						problems = append(problems, fmt.Sprintf("%s: %s claims to cover %s, which is unimplemented; mark it (unimplemented)", an.test, an.test, m))
					}
				}
			case len(tests) > 0 && refusedOnly:
				row.Status, row.Note = "Refused", "an official-SDK test proves it is refused"
				if a.Own {
					// Only an upstream emulator refuses what the registry
					// cannot speak for; CloudBurrow's own refusals are
					// registry entries, checked in-process.
					problems = append(problems, fmt.Sprintf("%s marks %s (unimplemented), but the registry says implemented", tests[0], m))
				}
			case len(tests) > 0:
				row.Status = "Verified"
				for _, an := range byMethod[m] {
					if an.refused {
						problems = append(problems, fmt.Sprintf("%s marks %s (unimplemented), but the registry says implemented", an.test, m))
					}
				}
			case status == regImplemented:
				row.Status, row.Note = "Implemented", "no official-SDK test proves it yet"
			default:
				row.Status = "Unknown"
			}
			p.Rows = append(p.Rows, row)
		}
		pages = append(pages, p)
	}
	for m := range reg {
		if !known[m] {
			problems = append(problems, fmt.Sprintf("registry entry %s names no method in the proto surface", m))
		}
	}
	for m, as := range byMethod {
		if !known[m] {
			problems = append(problems, fmt.Sprintf("%s covers %s, which is not a method of any listed service", as[0].test, m))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return pages, nil
}

var statusOrder = []string{"Verified", "Refused", "Unimplemented", "Implemented", "Not served", "Unknown"}

func counts(p Page) map[string]int {
	c := map[string]int{}
	for _, r := range p.Rows {
		c[r.Status]++
	}
	return c
}

func render(pages []Page) map[string][]byte {
	out := map[string][]byte{}
	var readme bytes.Buffer
	readme.WriteString("# API coverage\n\n")
	readme.WriteString("Generated by `go run ./tools/coverage` from the Go proto descriptors, a checked-in registry\n" +
		"(`tools/coverage/registry.json`) and `// covers:` annotations on `test/compat` tests. CI runs\n" +
		"`go run ./tools/coverage -check` and fails when this is stale or an annotation is wrong, and\n" +
		"`tools/coverage/servers_test.go` proves every Unimplemented method returns `codes.Unimplemented`.\n" +
		"Do not edit these files by hand.\n\n")
	readme.WriteString("| Service | RPCs | " + strings.Join(statusOrder, " | ") + " |\n|---|---|" + strings.Repeat("---|", len(statusOrder)) + "\n")
	for _, p := range pages {
		c := counts(p)
		fmt.Fprintf(&readme, "| [%s](%s.md) | %d |", p.Title, p.Key, len(p.Rows))
		for _, s := range statusOrder {
			fmt.Fprintf(&readme, " %d |", c[s])
		}
		readme.WriteString("\n")

		var b bytes.Buffer
		fmt.Fprintf(&b, "# %s\n\n", p.Title)
		b.WriteString("Generated by `go run ./tools/coverage`; see [the summary](README.md) for what each status means.\n\n")
		var parts []string
		for _, s := range statusOrder {
			if c[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c[s], strings.ToLower(s)))
			}
		}
		fmt.Fprintf(&b, "%d methods: %s.\n\n", len(p.Rows), strings.Join(parts, ", "))
		b.WriteString("| Method | Status | Evidence |\n|---|---|---|\n")
		for _, r := range p.Rows {
			var ev []string
			for _, t := range r.Tests {
				fileLine, fn, _ := strings.Cut(t, " ")
				file, line, _ := strings.Cut(fileLine, ":")
				ev = append(ev, fmt.Sprintf("[`%s`](../../%s#L%s)", fn, file, line))
			}
			if r.Note != "" {
				ev = append(ev, r.Note)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.Method, r.Status, strings.Join(ev, "; "))
		}
		out[p.Key+".md"] = b.Bytes()
	}
	readme.WriteString("\n**Statuses.** *Verified*: an official-SDK compat test, linked, exercises it. *Refused*: a compat\n" +
		"test proves the upstream emulator refuses it. *Unimplemented*: CloudBurrow returns `UNIMPLEMENTED`, proven\n" +
		"in-process. *Implemented*: served, with no official-SDK test yet. *Not served*: not registered on any port.\n" +
		"*Unknown*: an upstream-backed method no test covers, so nothing is claimed.\n")
	out["README.md"] = readme.Bytes()

	type jsonPage struct {
		Key, Title string
		Counts     map[string]int `json:"counts"`
		Rows       []Row          `json:"rows"`
	}
	var js []jsonPage
	for _, p := range pages {
		js = append(js, jsonPage{p.Key, p.Title, counts(p), p.Rows})
	}
	b, _ := json.MarshalIndent(js, "", "  ")
	out["coverage.json"] = append(b, '\n')
	return out
}
