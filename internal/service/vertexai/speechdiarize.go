package vertexai

import (
	"context"
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speaker"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// Speaker diarization (#2131): features.diarizationConfig (v2) and
// diarization_config.enable_speaker_diarization (v1), where Google's
// supported languages tables mark it (chirp_3 in v2; v1's models), are
// answered by internal/speaker, 3D-Speaker's CAM++ speaker embeddings
// clustered over whisper's words, in the `-tags onnx` build; without it,
// speechconfig's Unserved answers 501 first.
//
// The labels are where Google's docs put them
// (https://docs.cloud.google.com/speech-to-text/docs/multiple-voices, read
// 2026-11-21): v2's words[].speakerLabel ("1", "2", ...) on each result's
// top alternative, which the request's diarizationConfig alone asks for,
// as the page's REST example answers words without enableWordTimeOffsets;
// v1's WordInfo.speaker_tag and speaker_label, in one more, last result
// whose single alternative holds every word of the audio ("Speaker Tags
// are only included in the last result object, which has only one
// alternative"), the other results' words untagged.
//
// minSpeakerCount and maxSpeakerCount: v1's bound the speakers found
// ("If not set, the default value is 2" and 6); v2's are accepted and
// ignored, the clusterer choosing the count, as the v2 reference says of
// both: "The system automatically determines the number of speakers. This
// value is not currently used" (read 2026-11-21).
//
// v1 StreamingRecognize diarizes too: diarization_config's reference says
// "When this is enabled, we send all the words from the beginning of the
// audio for the top alternative in every consecutive STREAMING responses",
// so each final result's alternative holds every word said so far, all
// labelled again over the audio so far. With
// SEPARATE_RECOGNITION_PER_CHANNEL (v2), which no Google page restricts,
// each channel is diarized on its own.
//
// UNVERIFIED (re-read 2026-11-21): how Google segments and numbers
// speakers; v1's last result's transcript (empty here) and resultEndTime;
// whether v1's interim streaming results carry the words too (here only
// the final ones do); how speakers are numbered across channels (here from
// 1 in each).

// diarizeSegments labels each word of segs with its speaker when cfg asks
// for diarization.
func diarizeSegments(ctx context.Context, samples []float32, segs []speech.Segment, cfg speechconfig.Config) error {
	if !cfg.Diarization {
		return nil
	}
	var spans []speaker.Span
	for _, seg := range segs {
		for _, w := range seg.Words {
			spans = append(spans, speaker.Span{Start: w.Start, End: w.End})
		}
	}
	labels, err := speaker.Diarize(ctx, samples, spans, speaker.Options{Min: cfg.MinSpeakers, Max: cfg.MaxSpeakers})
	switch {
	case errors.Is(err, speaker.ErrNotCompiled):
		return apierror.Unimplemented("Not implemented here: features.diarizationConfig: %v", err)
	case err != nil:
		return apierror.Internal(err, "speaker diarization: %v", err)
	}
	k := 0
	for i := range segs {
		for j := range segs[i].Words {
			segs[i].Words[j].Speaker = labels[k]
			k++
		}
	}
	return nil
}
