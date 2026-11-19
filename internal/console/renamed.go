package console

import "strings"

// AgentPlatformBase is where the console's AI pages live since Google
// renamed Vertex AI to Gemini Enterprise Agent Platform (April 2026) and
// moved its console from /vertex-ai/ to /agent-platform/ (#1839).
const AgentPlatformBase = "/agent-platform"

// renamedAIPages maps each page's address before the rename to its address
// now. The left side is what CloudBurrow served; /vertex-ai/ is Google's
// own old prefix, redirected as Google redirects it.
var renamedAIPages = map[string]string{
	"/ai/models":       AgentPlatformBase + "/model-garden",
	"/ai/playground":   AgentPlatformBase + "/studio",
	"/ai/embeddings":   AgentPlatformBase + "/studio/embeddings",
	"/ai/local-models": AgentPlatformBase + "/model-garden/local-models",
	"/ai/predict":      AgentPlatformBase + "/online-prediction/endpoints",
}

// RenamedAIPath returns where an address from before the rename now lives,
// and false for every other address.
func RenamedAIPath(path string) (string, bool) {
	if to, ok := renamedAIPages[strings.TrimSuffix(path, "/")]; ok {
		return to, true
	}
	// A model's page under the old catalogue opens Model Garden.
	if strings.HasPrefix(path, "/ai/models/") {
		return AgentPlatformBase + "/model-garden", true
	}
	if path == "/vertex-ai" || path == "/vertex-ai/" {
		return AgentPlatformBase + "/overview", true
	}
	if rest, ok := strings.CutPrefix(path, "/vertex-ai/"); ok {
		return AgentPlatformBase + "/" + rest, true
	}
	return "", false
}
