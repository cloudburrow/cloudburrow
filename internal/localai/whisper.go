package localai

// WhisperRepo is the official whisper.cpp model repository on Hugging Face.
const WhisperRepo = "ggerganov/whisper.cpp"

// WhisperModelFile is the ggml model internal/speech transcribes with.
const WhisperModelFile = "ggml-base-q8_0.bin"

// Whisper is OpenAI's multilingual Whisper base model (MIT), converted to
// ggml and quantised to 8 bits by whisper.cpp's authors, from the official
// whisper.cpp model repository (#1799). BigQuery's ML.TRANSCRIBE reads
// audio with it. It is fetched the first time audio is transcribed, into
// <state-dir>/models, and checked against the SHA-256 pinned here (Hugging
// Face's LFS object ID, which matched the file downloaded from the
// revision on 2026-11-18). It is not a catalogue entry: no Local models
// screen manages it, since ML.TRANSCRIBE fetches what it needs.
var Whisper = Model{
	ID:        "whisper-base-q8_0-ggml",
	Repo:      WhisperRepo,
	Publisher: PublisherCommunity,
	Access:    AccessOpen,
	License:   "mit",
	Modality:  ModalitySpeech,
	Official:  false,
	Notes:     "OpenAI Whisper base (multilingual, MIT) in ggml q8_0 from whisper.cpp's model repository, fetched on demand for ML.TRANSCRIBE.",
	Pin: &Pin{
		Revision: "5359861c739e955e79d9a303bcbc70fb988958b1",
		Files: []PinnedFile{
			{Path: WhisperModelFile, Bytes: 81768585,
				SHA256: "c577b9a86e7e048a0b7eada054f4dd79a56bbfa911fbdacf900ac5b567cbb7d9"},
		},
	},
}
