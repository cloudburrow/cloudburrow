package main

import (
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// memberIndex maps a policy member, lower case, to the accounts it names
// (#1812): an account's own user: or serviceAccount: member, group:G for
// every Cloud Identity group it belongs to, directly or through nested
// groups, and domain:D for an account of the instance's domain
// (resourcemanager.Principals). Every check that reads a stored policy for
// minted tokens' accounts expands group: and domain: members through it.
type memberIndex map[string][]string

// newMemberIndex indexes the principals of every account in emails.
func newMemberIndex(reg *resourcemanager.Registry, emails map[string]bool) memberIndex {
	accounts := make([]string, 0, len(emails))
	for e := range emails {
		accounts = append(accounts, e)
	}
	sort.Strings(accounts)
	ix := memberIndex{}
	for _, e := range accounts {
		for _, p := range reg.Principals(e) {
			p = strings.ToLower(p)
			ix[p] = append(ix[p], e)
		}
	}
	return ix
}

// accounts are the accounts member names.
func (ix memberIndex) accounts(member string) []string {
	return ix[strings.ToLower(strings.TrimSpace(member))]
}

// principalsOf is each account's principals beyond its own member (its
// groups and domain), for a front that matches dataset access entries
// itself.
func principalsOf(reg *resourcemanager.Registry, emails map[string]bool) map[string][]string {
	out := map[string][]string{}
	for e := range emails {
		ps := reg.Principals(e)
		if len(ps) > 1 {
			out[e] = ps[1:]
		}
	}
	return out
}
