package localai

// SpeakerModelRepo is where the CAM++ speaker model's ONNX export is
// published: sherpa-onnx's (k2-fsa, Apache-2.0) speaker-recognition models
// release on GitHub.
const SpeakerModelRepo = "k2-fsa/sherpa-onnx"

// SpeakerModelFile is the ONNX export of 3D-Speaker's CAM++.
const SpeakerModelFile = "3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx"

// SpeakerCAMPlusPlus is 3D-Speaker's CAM++ speaker-embedding network
// (Alibaba DAMO Academy's iic/speech_campplus_sv_zh_en_16k-common_advanced,
// Apache-2.0, trained on 200,000 Chinese and English speakers), which
// Speech-to-Text's speaker diarization runs locally (internal/speaker,
// #2131). Google publishes no speaker model. 3D-Speaker publishes the
// weights as PyTorch only (ModelScope); this is the ONNX export sherpa-onnx's
// authors publish, ungated, on their GitHub release (its metadata names the
// ModelScope model, framework 3d-speaker, 192 dimensions, 16 kHz, fbank
// input with global-mean normalisation). It runs on ONNX Runtime, in the
// `-tags onnx` build only. It is fetched once, the first time audio is
// diarized, into <state-dir>/models, and checked against the SHA-256 pinned
// here: GitHub's asset digest, which matched the file downloaded on
// 2026-11-21. It is not a catalogue entry: no Local models screen manages
// it, since diarization fetches what it needs.
var SpeakerCAMPlusPlus = Model{
	ID:        "3dspeaker-campplus-zh-en-common-advanced-onnx",
	Repo:      SpeakerModelRepo,
	Publisher: PublisherCommunity,
	Access:    AccessOpen,
	License:   "apache-2.0",
	Modality:  ModalitySpeakerEmbedding,
	Artifact:  SpeakerModelFile,
	Runtime:   RuntimeONNX,
	Official:  false,
	Notes: "3D-Speaker's CAM++ speaker embeddings (Apache-2.0) in sherpa-onnx's ONNX export (Apache-2.0), " +
		"fetched on demand for Speech-to-Text speaker diarization in the onnx build.",
	Pin: &Pin{
		Revision: "speaker-recongition-models",
		BaseURL:  "https://github.com/" + SpeakerModelRepo + "/releases/download/speaker-recongition-models",
		Files: []PinnedFile{
			{Path: SpeakerModelFile, Bytes: 28281165,
				SHA256: "53cab20ba8f2eb2f6e2d89ecc18e88ef3222169b61b4419cbc13666744a4574b"},
		},
	},
}
