// Command depcheck discovers newer upstream versions of the components
// CloudBurrow builds on. It reports every inventory component as candidate,
// current, skipped (with the reason) or unreachable; none is left out.
//
// It separates two questions that are easy to conflate:
//
//   - What is the latest upstream version?       (discovery)
//   - Is that version actually compatible?        (testing)
//
// This tool only answers the first, and never edits the lock set. No CI job
// runs the suites against a candidate: promotion is a reviewed pull request
// that changes the pin, and that pull request's CI is the test, because
// "newest" and "works" are different claims.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// inventory mirrors the parts of dependencies.json this tool reads.
//
// Entries are RawMessage because the file carries "$comment" strings alongside
// component objects, and a strict decode would reject the whole document over
// a comment.
type inventory struct {
	Components map[string]map[string]json.RawMessage `json:"components"`
}

type component struct {
	Version    string `json:"version"`
	Image      string `json:"image"`
	Digest     string `json:"digest"`
	Source     string `json:"source"`
	UpdateFeed string `json:"updateFeed"`
	Module     string `json:"module"`
	// SkipDiscovery explains why this component's version cannot be compared
	// against its source repository's releases. A reason is required, so a
	// component is never silently excluded from checking.
	SkipDiscovery string `json:"skipDiscovery"`
}

// status is what depcheck concluded about one component. Every component in
// the inventory gets exactly one, so none is silently excluded from checking.
type status string

const (
	statusCandidate   status = "candidate"   // a newer upstream release exists
	statusCurrent     status = "current"     // the pin matches the latest release
	statusSkipped     status = "skipped"     // not auto-checkable; Detail says why
	statusUnreachable status = "unreachable" // the upstream could not be asked
)

// result is depcheck's finding for one component.
type result struct {
	Group   string
	Name    string
	Status  status
	Current string
	Latest  string // set for candidate and current
	Detail  string // the reason for skipped, the error for unreachable
}

// githubAPI is the GitHub REST API base. A variable so tests can point it at a
// fake server; the tests never reach the network.
var githubAPI = "https://api.github.com"

func main() {
	path := flag.String("inventory", "dependencies.json", "path to the component inventory")
	timeout := flag.Duration("timeout", 60*time.Second, "overall network timeout")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		fail("read %s: %v", *path, err)
	}
	results, err := check(raw, &http.Client{Timeout: *timeout}, githubAPI)
	if err != nil {
		fail("parse %s: %v", *path, err)
	}

	report(os.Stdout, os.Stderr, results)
	// Discovery finding an update is information, not a failure: exiting
	// non-zero would make an ordinary upstream release break the build.
	for _, r := range results {
		if r.Status == statusUnreachable {
			os.Exit(2)
		}
	}
}

// check classifies every component in the inventory, in group then name order.
func check(raw []byte, client *http.Client, api string) ([]result, error) {
	var inv inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, err
	}

	groups := make([]string, 0, len(inv.Components))
	for g := range inv.Components {
		if !strings.HasPrefix(g, "$") {
			groups = append(groups, g)
		}
	}
	sort.Strings(groups)

	var results []result
	for _, g := range groups {
		names := make([]string, 0, len(inv.Components[g]))
		for n := range inv.Components[g] {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, n := range names {
			if strings.HasPrefix(n, "$") {
				continue
			}
			var c component
			if err := json.Unmarshal(inv.Components[g][n], &c); err != nil {
				// Not a component object (a comment, say). Skipping is right:
				// failing here would make one stray key block every check.
				continue
			}
			r := classify(client, api, c)
			r.Group, r.Name, r.Current = g, n, c.Version
			results = append(results, r)
		}
	}
	return results, nil
}

// classify decides one component's status. Every path returns a status: a
// component depcheck cannot check is reported as skipped with the reason,
// never dropped.
func classify(client *http.Client, api string, c component) result {
	if c.SkipDiscovery != "" {
		return result{Status: statusSkipped, Detail: c.SkipDiscovery}
	}
	repo, ok := githubRepo(c.Source)
	if !ok {
		return result{Status: statusSkipped, Detail: "no skipDiscovery reason, and depcheck has no discovery for " +
			describeSource(c) + "; record a skipDiscovery reason or implement the feed"}
	}
	if c.Version == "" {
		return result{Status: statusSkipped, Detail: "no version recorded to compare against " + c.Source}
	}
	latest, err := latestRelease(client, api, repo)
	if err != nil {
		// An unreachable upstream is reported, never treated as "no update
		// available": silence would look like currency.
		return result{Status: statusUnreachable, Detail: err.Error()}
	}
	if latest == "" {
		return result{Status: statusSkipped, Detail: c.Source + " publishes no GitHub releases to compare against"}
	}
	// Compare without a leading "v": an inventory recording 1.56.1 and a tag
	// of v1.56.1 are the same release, and reporting that as an update would
	// train a reader to ignore the output.
	if normalize(latest) == normalize(c.Version) {
		return result{Status: statusCurrent, Latest: latest}
	}
	return result{Status: statusCandidate, Latest: latest}
}

// githubRepo returns owner/repo for a https://github.com/owner/repo source.
func githubRepo(source string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(source), "https://github.com/")
	if !ok {
		return "", false
	}
	repo := strings.TrimSuffix(rest, "/")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return repo, true
}

// describeSource names what a component would be discovered from, for the
// skipped reason of one depcheck cannot check.
func describeSource(c component) string {
	var parts []string
	if c.UpdateFeed != "" {
		parts = append(parts, fmt.Sprintf("updateFeed %q", c.UpdateFeed))
	}
	switch {
	case c.Source != "":
		parts = append(parts, "source "+c.Source)
	case c.Module != "":
		parts = append(parts, "module "+c.Module)
	case c.Image != "":
		parts = append(parts, "image "+c.Image)
	}
	if len(parts) == 0 {
		return "a component with no source, module, image or updateFeed"
	}
	return strings.Join(parts, ", ")
}

// latestRelease returns the latest release tag of a GitHub owner/repo, or ""
// when the repository publishes no releases.
func latestRelease(client *http.Client, api, repo string) (string, error) {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(api, "/")+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// No releases is a legitimate state, not an error.
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// normalize strips a leading "v" so version strings compare by value.
func normalize(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// report prints every result, grouped by status. Unreachable components go to
// errw, the rest to w.
func report(w, errw io.Writer, results []result) {
	by := map[status][]result{}
	for _, r := range results {
		by[r.Status] = append(by[r.Status], r)
	}
	fmt.Fprintf(w, "Checked %d component(s): %d candidate, %d current, %d skipped, %d unreachable.\n",
		len(results), len(by[statusCandidate]), len(by[statusCurrent]),
		len(by[statusSkipped]), len(by[statusUnreachable]))

	if cs := by[statusCandidate]; len(cs) == 0 {
		fmt.Fprintln(w, "\nNo newer upstream versions found.")
	} else {
		fmt.Fprintf(w, "\n%d component(s) have a newer upstream release:\n\n", len(cs))
		for _, c := range cs {
			fmt.Fprintf(w, "  %s/%s\n    current: %s\n    latest:  %s\n    discovered only; compatibility is not implied\n\n",
				c.Group, c.Name, c.Current, c.Latest)
		}
		fmt.Fprintln(w, "These are candidates only. A version is promoted into the lock set")
		fmt.Fprintln(w, "by a reviewed pull request after the suites pass against it.")
	}

	if cs := by[statusCurrent]; len(cs) > 0 {
		fmt.Fprintf(w, "\n%d component(s) match their latest upstream release:\n", len(cs))
		for _, c := range cs {
			fmt.Fprintf(w, "  %s/%s: %s\n", c.Group, c.Name, c.Latest)
		}
	}

	if cs := by[statusSkipped]; len(cs) > 0 {
		fmt.Fprintf(w, "\n%d component(s) are not auto-checkable:\n", len(cs))
		for _, c := range cs {
			fmt.Fprintf(w, "  %s/%s: %s\n", c.Group, c.Name, c.Detail)
		}
	}

	if cs := by[statusUnreachable]; len(cs) > 0 {
		fmt.Fprintf(errw, "\n%d component(s) could not be checked:\n", len(cs))
		for _, c := range cs {
			fmt.Fprintf(errw, "  %s/%s: %s\n", c.Group, c.Name, c.Detail)
		}
		fmt.Fprintln(errw, "\nUnreachable is not the same as up to date.")
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
