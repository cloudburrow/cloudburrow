package admin

import (
	"net/http"
	"sort"
)

// GET /admin/instance (#801): what reset, seed and the state archive accept
// on this instance, read from the same registrations the handlers use, so a
// caller such as the console's Instance page offers exactly what would
// succeed and nothing the handlers refuse.

// InstanceInfo is GET /admin/instance's answer.
type InstanceInfo struct {
	// Reset is every component POST /admin/reset clears, in the order it
	// clears them, and whether each can be confined to one project.
	Reset []ResetTarget `json:"reset"`
	// Reseed names the components of the seed file `up` applied, which
	// reset?reseed=true re-applies; empty when up was given none, in which
	// case reseed=true is refused.
	Reseed []string `json:"reseed"`
	// Seed names the components a POST /admin/seed document may name.
	Seed []string `json:"seed"`
	// State is the manifest an export would begin with now: every service,
	// captured or not, and why not.
	State Manifest `json:"state"`
}

// ResetTarget is one component a reset may name.
type ResetTarget struct {
	Name string `json:"name"`
	// ByProject is true when the component honours ?project=; a
	// project-scoped reset naming one that does not is refused.
	ByProject bool `json:"byProject"`
}

// Info reports what reset, seed and the state archive accept.
func (a *API) Info() InstanceInfo {
	out := InstanceInfo{Reset: []ResetTarget{}, Reseed: []string{}, Seed: []string{}, State: a.manifest()}
	for _, r := range a.resets {
		_, scoped := r.(ProjectResetter)
		out.Reset = append(out.Reset, ResetTarget{Name: r.Name(), ByProject: scoped})
	}
	a.mu.Lock()
	if a.startup != nil {
		out.Reseed = a.startup.Components()
	}
	a.mu.Unlock()
	for name := range a.seeds {
		out.Seed = append(out.Seed, name)
	}
	sort.Strings(out.Seed)
	if out.State.Services == nil {
		out.State.Services = []ManifestService{}
	}
	return out
}

func (a *API) handleInstance(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Info())
}
