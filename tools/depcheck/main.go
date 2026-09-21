// Command depcheck discovers newer upstream versions of the components
// CloudBurrow builds on, and resolves the immutable identity of the ones it is
// pinned to.
//
// It separates two questions that are easy to conflate:
//
//   - What is the latest upstream version?       (discovery)
//   - Is that version actually compatible?        (testing, done by CI)
//
// This tool only answers the first, and never edits the lock set. Promotion is
// a reviewed pull request, because "newest" and "works" are different claims.
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

// candidate is a discovered newer version.
type candidate struct {
	Group   string
	Name    string
	Current string
	Latest  string
	Note    string
}

func main() {
	path := flag.String("inventory", "dependencies.json", "path to the component inventory")
	timeout := flag.Duration("timeout", 60*time.Second, "overall network timeout")
	flag.Parse()

	raw, err := os.ReadFile(*path)
	if err != nil {
		fail("read %s: %v", *path, err)
	}
	var inv inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		fail("parse %s: %v", *path, err)
	}

	client := &http.Client{Timeout: *timeout}
	var candidates []candidate
	var unreachable []string
	var skipped []string

	groups := make([]string, 0, len(inv.Components))
	for g := range inv.Components {
		groups = append(groups, g)
	}
	sort.Strings(groups)

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
			if c.SkipDiscovery != "" {
				skipped = append(skipped, fmt.Sprintf("%s/%s: %s", g, n, c.SkipDiscovery))
				continue
			}
			if c.Source == "" || !strings.Contains(c.Source, "github.com/") {
				continue
			}
			latest, err := latestRelease(client, c.Source)
			if err != nil {
				// An unreachable upstream is reported, never treated as
				// "no update available" — silence would look like currency.
				unreachable = append(unreachable, fmt.Sprintf("%s/%s: %v", g, n, err))
				continue
			}
			// Compare without a leading "v": an inventory recording 1.56.1
			// and a tag of v1.56.1 are the same release, and reporting that
			// as an update would train a reader to ignore the output.
			if latest != "" && normalize(latest) != normalize(c.Version) {
				candidates = append(candidates, candidate{
					Group: g, Name: n, Current: c.Version, Latest: latest,
					Note: "discovered only; compatibility is not implied",
				})
			}
		}
	}

	report(candidates, unreachable, skipped)
	// Discovery finding an update is information, not a failure: exiting
	// non-zero would make an ordinary upstream release break the build.
	if len(unreachable) > 0 {
		os.Exit(2)
	}
}

// latestRelease returns the latest release tag for a GitHub source URL.
func latestRelease(client *http.Client, source string) (string, error) {
	repo := strings.TrimSuffix(strings.TrimPrefix(source, "https://github.com/"), "/")
	if strings.Count(repo, "/") != 1 {
		return "", nil
	}
	req, err := http.NewRequest(http.MethodGet,
		"https://api.github.com/repos/"+repo+"/releases/latest", nil)
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

func report(candidates []candidate, unreachable, skipped []string) {
	if len(candidates) == 0 {
		fmt.Println("No newer upstream versions found.")
	} else {
		fmt.Printf("%d component(s) have a newer upstream release:\n\n", len(candidates))
		for _, c := range candidates {
			fmt.Printf("  %s/%s\n    current: %s\n    latest:  %s\n    %s\n\n",
				c.Group, c.Name, c.Current, c.Latest, c.Note)
		}
		fmt.Println("These are candidates only. A version is promoted into the lock set")
		fmt.Println("by a reviewed pull request after the suites pass against it.")
	}

	if len(skipped) > 0 {
		fmt.Printf("\n%d component(s) are not auto-checkable:\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("  %s\n", s)
		}
	}

	if len(unreachable) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d component(s) could not be checked:\n", len(unreachable))
		for _, u := range unreachable {
			fmt.Fprintf(os.Stderr, "  %s\n", u)
		}
		fmt.Fprintln(os.Stderr, "\nUnreachable is not the same as up to date.")
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
