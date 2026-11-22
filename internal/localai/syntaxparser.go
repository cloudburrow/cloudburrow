package localai

// Parsey McParseface, the English part-of-speech tagger and dependency
// parser Google published with SyntaxNet (Apache-2.0), which Natural
// Language's analyzeSyntax runs locally (internal/syntaxnet, #1799). Like
// the MediaPipe models it is a file download, not an API call: fetched
// once, the first time a text is analysed, into <state-dir>/models and
// checked against the SHA-256 pinned here, measured on the files
// downloaded on 2026-11-21 from tensorflow/models at tag v1.13.0.

// ModalitySyntax tags and parses text.
const ModalitySyntax Modality = "syntax"

// ParseyMcParsefaceRepo is the model's directory in Google's
// tensorflow/models repository.
const ParseyMcParsefaceRepo = "tensorflow/models/research/syntaxnet/syntaxnet/models/parsey_mcparseface"

// parseyCommit is tensorflow/models' v1.13.0 tag, the last release that
// carries SyntaxNet's models.
const parseyCommit = "57e075203f8fba8d85e6b74f17f63d0a07da233a"

// ParseyMcParseface is SyntaxNet's English tagger and parser.
var ParseyMcParseface = Model{
	ID:        "syntaxnet-parsey-mcparseface-2016-05-17",
	Repo:      ParseyMcParsefaceRepo,
	Publisher: PublisherGoogle,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalitySyntax,
	Artifact:  "parser-params",
	Runtime:   "syntaxnet",
	Official:  true,
	Notes:     "SyntaxNet's Parsey McParseface (Apache-2.0, Google), fetched on demand for Natural Language analyzeSyntax.",
	Pin: &Pin{
		Revision: parseyCommit,
		BaseURL:  "https://raw.githubusercontent.com/tensorflow/models/" + parseyCommit + "/research/syntaxnet/syntaxnet/models/parsey_mcparseface",
		Files: []PinnedFile{
			{Path: "parser-params", Bytes: 22369891, SHA256: "adf58fde76b6c2817eb287e83a4d8547b660219edae00bcd047e89781c5db229"},
			{Path: "tagger-params", Bytes: 19188055, SHA256: "54886dd542ff7bf0b5919ef1cd53401220d0b696a93c044daa7beb7e0dead2a2"},
			{Path: "word-map", Bytes: 678188, SHA256: "94d226a236f80f6f5f19688ebc198c3a9654ecb48584725f5fbbc89da8d66e1f"},
			{Path: "tag-map", Bytes: 447, SHA256: "fc3b16f6f39506d82a9ffcfa12f420ddfe3d7ceb33800296e21332e43bed6f10"},
			{Path: "label-map", Bytes: 549, SHA256: "16b55e50b55926c10ad758e95f45178929c248ba0e636ddaa90beb44ac70dfa6"},
			{Path: "fine-to-universal.map", Bytes: 369, SHA256: "ef1dc014c0ad22f2dabcb7eba67fe1b9e49fafa666bc04b1b7d2baf9474130dd"},
			{Path: "prefix-table", Bytes: 125727, SHA256: "2b86495844713db511a277c13dbbe5322ec1cbd7aa5b3f2bceebecc094f92ba6"},
			{Path: "suffix-table", Bytes: 105284, SHA256: "8592e760feab676191fefd80fad55e75ee9bc2212c46752f95074dc61b4dece9"},
		},
	},
}
