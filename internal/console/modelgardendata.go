package console

import "github.com/cloudburrow/cloudburrow/internal/localai"

// The Model Garden and Agent Garden cards (#1839), as Google's documentation
// lists them (docs.cloud.google.com/gemini-enterprise-agent-platform/models/
// model-garden/explore-models, .../open-models/use-hugging-face-models and
// .../build/agent-garden). The filter groups are the ones that page
// documents: Model collections, Tasks and Providers; its Features filter's
// values are not documented, so it is not drawn.

// consoleCard is the documented pattern of a Hugging Face model's card,
// publishers/{hf-org}/model-garden/{model};hfSource=true. The other cards'
// slugs are not documented, so they open Model Garden itself.
const consoleCard = "https://console.cloud.google.com/agent-platform/publishers/"

const gardenHome = "https://console.cloud.google.com/agent-platform/model-garden"

// Model collections, in the order the filter shows them.
const (
	collectionGoogle  = "Google models"
	collectionPartner = "Partner models"
	collectionOpen    = "Self-deploy open models"
	collectionHF      = "Open models on Hugging Face"
)

var gardenCollections = []string{collectionGoogle, collectionPartner, collectionOpen, collectionHF}

var gardenTasks = []string{"Generation", "Embedding", "Image generation", "Video generation",
	"Speech recognition", "Classification"}

var gardenModels = []GardenModel{
	{ID: "gemini-3-pro", Name: "Gemini 3 Pro", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Generation"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Google's most capable Gemini model for reasoning, coding and multimodal understanding. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Generation"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Fast, low-cost Gemini model for agents and high-volume tasks. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Generation"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Price-performance Gemini model with thinking. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "gemini-embedding-001", Name: "Gemini Embedding", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Embedding"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Text embeddings for search, retrieval and classification. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "imagen-4", Name: "Imagen 4", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Image generation"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Text-to-image generation. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "veo-3", Name: "Veo 3", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Video generation"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Text- and image-to-video generation with audio. Managed: called, not deployed.",
		Docs:    gardenHome},
	{ID: "chirp-3", Name: "Chirp 3", Provider: "Google", Collection: collectionGoogle,
		Tasks: []string{"Speech recognition"}, Licence: "Google Cloud terms (managed model)",
		Summary: "Speech-to-text across many languages. Managed: called through Speech-to-Text.",
		Docs:    gardenHome},
	{ID: "gemma-4", Name: "Gemma 4", Provider: "Google", Collection: collectionOpen,
		Tasks: []string{"Generation"}, Licence: "Gemma Terms of Use",
		Summary: "Open-weight Gemma model, self-deployed to an endpoint. Locally: the E2B instruction-tuned LiteRT-LM file.",
		Docs:    gardenHome, Local: "gemma-4-e2b-it-community"},
	{ID: "gemma-3n", Name: "Gemma 3n", Provider: "Google", Collection: collectionOpen,
		Tasks: []string{"Generation"}, Licence: "Gemma Terms of Use",
		Summary: "Gemma for on-device use, with audio and image input.",
		Docs:    gardenHome, Local: "gemma-3n-e2b-it"},
	{ID: "gemma-3", Name: "Gemma 3", Provider: "Google", Collection: collectionOpen,
		Tasks: []string{"Generation"}, Licence: "Gemma Terms of Use",
		Summary: "Open-weight multimodal Gemma models from 1B to 27B parameters.",
		Docs:    gardenHome},
	{ID: "embeddinggemma", Name: "EmbeddingGemma", Provider: "Google", Collection: collectionOpen,
		Tasks: []string{"Embedding"}, Licence: "Gemma Terms of Use",
		Summary: "A 300M-parameter open embedding model. Locally: the ONNX conversion on ONNX Runtime.",
		Docs:    gardenHome, Local: localai.EmbeddingGemmaONNXID},
	{ID: "medgemma", Name: "MedGemma", Provider: "Google", Collection: collectionOpen,
		Tasks: []string{"Generation"}, Licence: "Health AI Developer Foundations terms",
		Summary: "Gemma variants for medical text and image comprehension.",
		Docs:    gardenHome},
	{ID: "claude", Name: "Claude", Provider: "Anthropic", Collection: collectionPartner,
		Tasks: []string{"Generation"}, Licence: "Partner terms (enabled through Marketplace)",
		Summary: "Anthropic's Claude models as a managed service: Enable, then call.",
		Docs:    gardenHome},
	{ID: "llama-4", Name: "Llama 4", Provider: "Meta", Collection: collectionPartner,
		Tasks: []string{"Generation"}, Licence: "Llama 4 Community License",
		Summary: "Meta's Llama 4 Scout and Maverick, managed or self-deployed.",
		Docs:    gardenHome},
	{ID: "deepseek-r1", Name: "DeepSeek R1", Provider: "DeepSeek", Collection: collectionPartner,
		Tasks: []string{"Generation"}, Licence: "MIT",
		Summary: "Reasoning model, managed or self-deployed.",
		Docs:    gardenHome},
	{ID: "hf-google-gemma-3-27b-it", Name: "google/gemma-3-27b-it", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Generation"}, Licence: "Gemma Terms of Use (gated on Hugging Face)",
		Summary: "Deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "google/model-garden/gemma-3-27b-it;hfSource=true"},
	{ID: "hf-meta-llama-llama-4-scout", Name: "meta-llama/Llama-4-Scout-17B-16E-Instruct", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Generation"}, Licence: "Llama 4 Community License (gated on Hugging Face)",
		Summary: "Deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "meta-llama/model-garden/Llama-4-Scout-17B-16E-Instruct;hfSource=true"},
	{ID: "hf-qwen-qwq-32b", Name: "Qwen/QwQ-32B", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Generation"}, Licence: "Apache-2.0",
		Summary: "Deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "Qwen/model-garden/QwQ-32B;hfSource=true"},
	{ID: "hf-baai-bge-m3", Name: "BAAI/bge-m3", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Embedding"}, Licence: "MIT",
		Summary: "Multilingual embedding model, deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "BAAI/model-garden/bge-m3;hfSource=true"},
	{ID: "hf-intfloat-multilingual-e5-large-instruct", Name: "intfloat/multilingual-e5-large-instruct", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Embedding"}, Licence: "MIT",
		Summary: "Multilingual embedding model, deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "intfloat/model-garden/multilingual-e5-large-instruct;hfSource=true"},
	{ID: "hf-black-forest-labs-flux-1-dev", Name: "black-forest-labs/FLUX.1-dev", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Image generation"}, Licence: "FLUX.1 [dev] Non-Commercial License (gated on Hugging Face)",
		Summary: "Text-to-image model, deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "black-forest-labs/model-garden/FLUX.1-dev;hfSource=true"},
	{ID: "hf-huggingfacefw-fineweb-edu-classifier", Name: "HuggingFaceFW/fineweb-edu-classifier", Provider: "Hugging Face", Collection: collectionHF,
		Tasks: []string{"Classification"}, Licence: "Apache-2.0",
		Summary: "Text classifier, deployed from Hugging Face with a verified deployment config.",
		Docs:    consoleCard + "HuggingFaceFW/model-garden/fineweb-edu-classifier;hfSource=true"},
}

const adkSamples = "https://github.com/google/adk-samples/tree/main/python/agents/"

// gardenAgents are Agent Garden's samples: the RAG sample Google's Agent
// Garden page documents, and the Agent Development Kit samples it is built
// from, each with its public source.
var gardenAgents = []GardenAgent{
	{ID: "retrieval-augmented-generation", Name: "Retrieval-Augmented Generation (RAG)", Kind: "Sample",
		Summary: "Answers questions grounded in your documents, for documentation-grounded support and research synthesis.",
		Tags:    []string{"RAG Engine", "Grounding"},
		Source:  adkSamples + "RAG", Language: "Python"},
	{ID: "academic-research", Name: "Academic Research", Kind: "Sample",
		Summary: "Finds and summarises recent work that cites a seminal paper.",
		Tags:    []string{"Research", "Google Search"}, Source: adkSamples + "academic-research", Language: "Python"},
	{ID: "customer-service", Name: "Customer Service", Kind: "Sample",
		Summary: "A retail customer-service agent with tools for orders, products and appointments.",
		Tags:    []string{"Customer service", "Tools"}, Source: adkSamples + "customer-service", Language: "Python"},
	{ID: "data-science", Name: "Data Science", Kind: "Sample",
		Summary: "A multi-agent system that analyses data in BigQuery and trains models with BigQuery ML.",
		Tags:    []string{"BigQuery", "Multi-agent"}, Source: adkSamples + "data-science", Language: "Python"},
	{ID: "financial-advisor", Name: "Financial Advisor", Kind: "Sample",
		Summary: "Coordinates sub-agents that analyse a market, a trading strategy and its risk.",
		Tags:    []string{"Finance", "Multi-agent"}, Source: adkSamples + "financial-advisor", Language: "Python"},
	{ID: "llm-auditor", Name: "LLM Auditor", Kind: "Sample",
		Summary: "Checks an answer's claims against web search and rewrites what it cannot verify.",
		Tags:    []string{"Evaluation", "Google Search"}, Source: adkSamples + "llm-auditor", Language: "Python"},
	{ID: "marketing-agency", Name: "Marketing Agency", Kind: "Sample",
		Summary: "Plans a website launch: domain names, the site, marketing material and a logo.",
		Tags:    []string{"Marketing", "Multi-agent"}, Source: adkSamples + "marketing-agency", Language: "Python"},
	{ID: "travel-concierge", Name: "Travel Concierge", Kind: "Sample",
		Summary: "Plans, books and accompanies a trip with specialised sub-agents.",
		Tags:    []string{"Travel", "Multi-agent"}, Source: adkSamples + "travel-concierge", Language: "Python"},
}
