package main

// A Secure Source Manager pull request's code owners and a CODEOWNERS
// file's checks (#1928), as Google's "Manage pull request approvals with
// CODEOWNERS" describes them
// (https://docs.cloud.google.com/secure-source-manager/docs/codeowners,
// read 2026-11-19):
//   - the pull request page's Code owners tab lists what the base branch's
//     CODEOWNERS files require of the changed files: per section, the
//     owners, the approvals needed, the files, and who of them approved
//     the head commit, each Satisfied, Pending or Optional; the page's
//     summary says how many are satisfied and whether the branch rule
//     enforces them (Google's page does not show its own layout for this);
//   - a CODEOWNERS file's page has a Checks tab: its status and its line-level
//     warnings and information, such as the lines that do not parse and
//     those whose branch glob does not match the branch shown, which
//     Google's draws as highlighted lines with the details on hover.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// codeOwnersSummary is the pull request summary's Code owners line: ""
// when no CODEOWNERS file asks anything of it.
func codeOwnersSummary(co runadapter.SourceManagerCodeOwnerStatus, err error) string {
	if err != nil || len(co.Requirements) == 0 {
		if err == nil && co.Enforced {
			return "Required by branch rule " + co.Rule + "; no changed file has owners"
		}
		return ""
	}
	met := 0
	for _, q := range co.Requirements {
		if q.Satisfied {
			met++
		}
	}
	line := fmt.Sprintf("%d of %d satisfied", met, len(co.Requirements))
	if co.Enforced {
		return line + "; required by branch rule " + co.Rule
	}
	return line + "; not required by a branch rule"
}

// codeOwnersSection is the pull request's Code owners tab.
func codeOwnersSection(co runadapter.SourceManagerCodeOwnerStatus, err error) console.Section {
	sec := console.Section{ID: "codeowners", Label: "Code owners", Listing: console.Listing{
		Columns: []string{"Section", "Owners", "Approvals needed", "Approved by", "Files"}, NameColumn: "Requirement",
		Noun: "code owner requirements", AlwaysStatus: true}}
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	switch {
	case len(co.Files) == 0:
		sec.Note = "The base branch has no CODEOWNERS file."
	case co.Enforced:
		sec.Note = "Branch rule " + co.Rule + " requires code owner review: every requirement must be satisfied before merging. Read from " + strings.Join(co.Files, ", ") + " on the base branch."
	default:
		sec.Note = "No branch rule requires code owner review on the base branch. Read from " + strings.Join(co.Files, ", ") + " on the base branch."
	}
	if co.Problems > 0 {
		sec.Note += fmt.Sprintf(" %d lines of them do not parse and are ignored.", co.Problems)
	}
	for i, q := range co.Requirements {
		st := "Pending"
		switch {
		case q.Approvals == 0:
			st = "Optional"
		case q.Satisfied:
			st = "Satisfied"
		}
		section := q.Section
		if section == "" {
			section = "(no section)"
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: strconv.Itoa(i + 1), Status: st, Fields: map[string]string{
			"Section": section, "Owners": strings.Join(q.Owners, ", "), "Approvals needed": strconv.Itoa(q.Approvals),
			"Approved by": orDash(strings.Join(q.ApprovedBy, ", ")), "Files": strings.Join(q.Files, ", ")}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// codeOwnersFileSection is a CODEOWNERS file's checks at a ref: its status
// and each line's warning or information.
func (p sourceManagerProvider) codeOwnersFileSection(ctx context.Context, repo, ref, file, text string) console.Section {
	branch := ""
	if bs, err := runadapter.SourceManagerBranches(ctx, repo); err == nil {
		for _, b := range bs {
			if b.Name == ref {
				branch = ref
			}
		}
	}
	notes := runadapter.CheckSourceManagerCodeOwners(file, text, branch)
	sec := console.Section{ID: "codeowners", Label: "Checks", Listing: console.Listing{
		Columns: []string{"Text", "Details"}, NameColumn: "Line", Noun: "lines with notes", AlwaysStatus: true}}
	lines := strings.Split(text, "\n")
	errs := 0
	for _, n := range notes {
		st := "Information"
		if n.Error {
			st, errs = "Error", errs+1
		}
		txt := ""
		if n.Line-1 < len(lines) {
			txt = strings.TrimSpace(lines[n.Line-1])
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: strconv.Itoa(n.Line), Status: st,
			Fields: map[string]string{"Text": txt, "Details": n.Message}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	switch {
	case errs > 0:
		sec.Note = fmt.Sprintf("Status: %d lines have syntax errors and are ignored. A branch whose rule requires code owner review refuses a push of this file.", errs)
	default:
		sec.Note = "Status: valid."
	}
	if branch != "" {
		sec.Note += " Checked for the branch " + branch + "."
	}
	return sec
}
