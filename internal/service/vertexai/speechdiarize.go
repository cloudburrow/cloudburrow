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
// minSpeakerCount and maxSpeakerCount bound the speakers found (v1's
// defaults, 2 and 6, apply to v1; v2 leaves them free when unset).
// UNVERIFIED (re-read 2026-11-21: the v2 reference says of both "The
// system automatically determines the number of speakers. This value is not
// currently used"; CloudBurrow honours them as v1 documents): that v2
// ignores them; how Google segments and numbers speakers; v1's last
// result's transcript (empty here) and resultEndTime.

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
