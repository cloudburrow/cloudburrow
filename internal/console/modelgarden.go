package console

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// Model Garden and Agent Garden (#1839).
//
// Google's Agent Platform (formerly Vertex AI) opens on two catalogues: Model
// Garden, the models a project can use or deploy, and Agent Garden, the
// agents and samples it can start from. Both are mirrored here as Google
// lists them, so a developer finds the same cards in the same places; what
// differs is what a card can do. A model CloudBurrow runs locally has Deploy
// enabled, and Deploy is the real local download and load the Local models
// page drives. Every other card has Deploy disabled and says it is not
// available locally: nothing here pretends to create an endpoint.

// GardenModel is one Model Garden card.
type GardenModel struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Provider   string   `json:"provider"`
	Collection string   `json:"collection"`
	Tasks      []string `json:"tasks"`
	Licence    string   `json:"licence"`
	Summary    string   `json:"summary"`
	// Docs is Google's model card or documentation page.
	Docs string `json:"docs"`
	// Local is the localai catalogue ID that runs this model on this
	// machine, or empty when CloudBurrow has no local runtime for it.
	Local string `json:"local,omitempty"`
}

// GardenAgent is one Agent Garden card.
type GardenAgent struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // "Agent", "Sample" or "Tool"
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags"`
	Source   string   `json:"source"` // public source repository
	Language string   `json:"language,omitempty"`
}

// gardenModelView is a card with what this instance can do with it.
type gardenModelView struct {
	GardenModel
	// Deployable is true only when the local model can be downloaded or is
	// already downloaded on this instance.
	Deployable bool `json:"deployable"`
	// Unavailable says why Deploy is disabled.
	Unavailable string `json:"unavailable,omitempty"`
	// LocalState is the local model's state (see localai.ModelState).
	LocalState *localai.ModelState `json:"localState,omitempty"`
}

// NotLocal is what a card with no local runtime says.
const NotLocal = "Not available locally: CloudBurrow has no local runtime for this model, " +
	"so Deploy is disabled. On Google Cloud it is deployed or called from its model card."

func (s *Server) handleModelGarden(w http.ResponseWriter, _ *http.Request) {
	var states map[string]localai.ModelState
	if s.localModels != nil {
		states = map[string]localai.ModelState{}
		for _, st := range s.localModels.States() {
			states[st.ID] = st
		}
	}
	out := make([]gardenModelView, 0, len(gardenModels))
	for _, m := range gardenModels {
		v := gardenModelView{GardenModel: m}
		switch st, ok := states[m.Local]; {
		case m.Local == "":
			v.Unavailable = NotLocal
		case states == nil:
			v.Unavailable = "Not available on this instance: local model management is off " +
				"(start CloudBurrow with local AI to deploy it here)."
		case !ok:
			v.Unavailable = NotLocal
		case st.DownloadNote != "" && st.State != "downloaded":
			v.LocalState = &st
			v.Unavailable = "Not available locally: " + st.DownloadNote
		case !st.Pinned:
			v.LocalState = &st
			v.Unavailable = "Not available locally: this model has no pinned download."
		default:
			v.LocalState = &st
			v.Deployable = true
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  out,
		"filters": gardenFilters(),
		"note": "Cards are listed as Google's Model Garden lists them. Deploy is enabled only for a " +
			"model CloudBurrow runs on this machine: it downloads the pinned files and loads them " +
			"into the local endpoint, as the Local models page does. Every other card is shown " +
			"with Deploy disabled.",
	})
}

func (s *Server) handleAgentGarden(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": gardenAgents,
		"note": "Agent Garden's agents and samples, as Google lists them, with a link to each one's " +
			"public source. Deploy is disabled: it deploys to Agent Runtime (formerly Agent Engine, " +
			"projects.locations.reasoningEngines), which CloudBurrow does not serve. " +
			"Run a sample locally with the Agent Development Kit, pointed at this instance's " +
			"local generateContent endpoint.",
	})
}

// gardenFilters are the filter groups, each with the values the cards use,
// in Google's order.
func gardenFilters() []map[string]any {
	group := func(name string, values []string, of func(GardenModel) []string) map[string]any {
		used := map[string]bool{}
		for _, m := range gardenModels {
			for _, v := range of(m) {
				used[v] = true
			}
		}
		keep := []string{}
		for _, v := range values {
			if used[v] {
				keep = append(keep, v)
			}
		}
		return map[string]any{"name": name, "values": keep}
	}
	return []map[string]any{
		group("Model collections", gardenCollections, func(m GardenModel) []string { return []string{m.Collection} }),
		group("Tasks", gardenTasks, func(m GardenModel) []string { return m.Tasks }),
		group("Providers", gardenProviders(), func(m GardenModel) []string { return []string{m.Provider} }),
	}
}

func gardenProviders() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range gardenModels {
		if !seen[m.Provider] {
			seen[m.Provider] = true
			out = append(out, m.Provider)
		}
	}
	return out
}
