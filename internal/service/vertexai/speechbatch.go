package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
)

// Speech-to-Text v2's BatchRecognize (#1930), and Recognize's uri, on the
// local AI endpoint, gRPC and JSON, as
// https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers/batchRecognize
// and https://docs.cloud.google.com/speech-to-text/docs/batch-recognize
// document them (read 2026-11-19). The audio is read from, and the
// transcripts written to, this instance's Cloud Storage; nothing is sent
// to Google.
//
//   - files: 1 to 15 ("The maximum number of files allowed to be specified
//     is 15"), each a gs:// uri, each with an optional config merged over
//     the request's config, which is merged over the recognizer's
//     default_recognition_config. The merged configs are checked when the request is made:
//     an invalid one fails the call, not the operation.
//   - recognition_output_config: gcs_output_config writes each file's
//     BatchRecognizeResults, in JSON, under its uri prefix, and the
//     response's cloud_storage_result names the object;
//     inline_response_config ("only supported when calling BatchRecognize
//     with just one audio file") answers the transcript in the response.
//     output_format_config's native, vtt and srt are written (vtt and srt
//     from whisper's segment times), beside the JSON or inline.
//   - processing_strategy DYNAMIC_BATCHING is accepted and processed at
//     once ("fulfilled within 24 hours").
//   - The operation is returned at once and runs here in the background,
//     its OperationMetadata carrying batch_recognize_request and
//     batch_recognize_metadata (each file's progress_percent, its output
//     uri and any error); a file that fails (missing object, undecodable
//     audio) has its error in its BatchRecognizeFileResult and the others
//     still run, as the per-file error field allows.
//   - Audio is transcribed with the recognizer's model on local whisper.cpp
//     (speech.go), up to Google's 480 minutes a file, in pieces of at most
//     speech.ChunkSeconds cut at the quietest point near each piece's end.
//     MP3 and AAC stay limited by their decoders (internal/embedding's
//     MaxAudioSeconds); WAV, FLAC and headerless audio reach 480 minutes,
//     within batchMaxAudioBytes (1 GiB) an object.
//   - config_mask, on the request and on each file, is applied as
//     Recognize's (speechrecognizers.go's effectiveConfig).
//
// Recognize's uri reads a gs:// object of at most 10 MB from this
// instance's Cloud Storage, as content would be sent.
//
// UNVERIFIED (re-read 2026-11-19: the batchRecognize reference above names the prefix, not the
// objects): the
// result objects' names, {prefix}/{audio object's base name}_transcript_{id}.json
// (and .vtt, .srt); the texts of the errors.

const (
	batchMaxFiles       = 15
	recognizeMaxContent = 10 << 20
	batchMaxAudioBytes  = 1 << 30
	// batchMaxSeconds is BatchRecognize's limit: "Each file can be up to 8
	// hours long" (quotas, read 2026-11-19). Audio longer than
	// speech.ChunkSeconds is transcribed in pieces.
	batchMaxSeconds = 480 * 60
)

// readAudioURI reads a gs:// audio object.
func (s *Server) readAudioURI(ctx context.Context, uri string, limit int64) ([]byte, error) {
	bucket, object, ok := splitGCS(uri, false)
	if !ok {
		return nil, apierror.InvalidArgument("Invalid audio uri %q: expected gs://bucket/object", uri)
	}
	s.mu.RLock()
	objs := s.objects
	s.mu.RUnlock()
	if objs == nil {
		return nil, apierror.FailedPrecondition("%s needs Cloud Storage, which this instance does not serve", uri)
	}
	data, _, err := objs.Read(ctx, bucket, object, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, apierror.InvalidArgument("Audio %s exceeds the limit of %d bytes", uri, limit)
	}
	return data, nil
}

// configJSON is a RecognitionConfig as JSON, nil for none.
func configJSON(c *speechpb.RecognitionConfig) json.RawMessage {
	if c == nil {
		return nil
	}
	b, _ := protojson.Marshal(c)
	return b
}

// overConfig merges over's fields over base's, both JSON configs.
func overConfig(base, over json.RawMessage) json.RawMessage {
	if len(over) == 0 {
		return base
	}
	if len(base) == 0 {
		return over
	}
	var b, o map[string]any
	_ = json.Unmarshal(base, &b)
	_ = json.Unmarshal(over, &o)
	if b == nil {
		b = map[string]any{}
	}
	mergeJSON(b, normaliseKeys(o))
	out, _ := json.Marshal(b)
	return out
}

type batchFile struct {
	uri    string
	config json.RawMessage
	out    string // the native output object's gs:// uri
}

func (v *speechService) BatchRecognize(ctx context.Context, req *speechpb.BatchRecognizeRequest) (*longrunningpb.Operation, error) {
	op, err := v.s.batchRecognize(req)
	return op, grpcError(err)
}

func (s *Server) batchRecognize(req *speechpb.BatchRecognizeRequest) (*longrunningpb.Operation, error) {
	if _, _, _, err := parseChild("recognizer", "recognizers", req.GetRecognizer()); err != nil {
		return nil, err
	}
	n := len(req.GetFiles())
	switch {
	case n == 0:
		return nil, apierror.InvalidArgument("files is required: name at least one audio file")
	case n > batchMaxFiles:
		return nil, apierror.InvalidArgument("The maximum number of files allowed to be specified is %d; got %d", batchMaxFiles, n)
	}
	oc := req.GetRecognitionOutputConfig()
	gcs, inline := oc.GetGcsOutputConfig(), oc.GetInlineResponseConfig()
	switch {
	case gcs == nil && inline == nil:
		return nil, apierror.InvalidArgument("recognition_output_config is required: set gcs_output_config or inline_response_config")
	case inline != nil && n > 1:
		return nil, apierror.InvalidArgument("inline_response_config is only supported when calling BatchRecognize with just one audio file")
	}
	ofc := oc.GetOutputFormatConfig()
	native := ofc == nil || ofc.GetNative() != nil || ofc.GetVtt() == nil && ofc.GetSrt() == nil
	vtt, srt := ofc.GetVtt() != nil, ofc.GetSrt() != nil
	var outBucket, outPrefix string
	if gcs != nil {
		b, p, ok := splitGCS(gcs.GetUri(), true)
		if !ok {
			return nil, apierror.InvalidArgument("Invalid gcs_output_config.uri %q: expected gs://bucket/prefix", gcs.GetUri())
		}
		outBucket, outPrefix = b, p
		if outPrefix != "" && !strings.HasSuffix(outPrefix, "/") {
			outPrefix += "/"
		}
	}
	if _, _, err := s.storageRW("BatchRecognize"); err != nil {
		return nil, err
	}
	reqConfig, err := s.effectiveConfig(req.GetRecognizer(), configJSON(req.GetConfig()), req.GetConfigMask().GetPaths())
	if err != nil {
		return nil, err
	}
	recognizer := defaultRecognizer(req.GetRecognizer())
	var files []batchFile
	seen := map[string]bool{}
	for i, f := range req.GetFiles() {
		uri := f.GetUri()
		if _, _, ok := splitGCS(uri, false); !ok {
			return nil, apierror.InvalidArgument("Invalid files[%d].uri %q: expected gs://bucket/object", i, uri)
		}
		if seen[uri] {
			return nil, apierror.InvalidArgument("files[%d].uri %q is named twice", i, uri)
		}
		seen[uri] = true
		cfg := overConfig(reqConfig, configJSON(f.GetConfig()))
		if paths := f.GetConfigMask().GetPaths(); len(paths) > 0 {
			var base map[string]any
			_ = json.Unmarshal(reqConfig, &base)
			if base == nil {
				base = map[string]any{}
			}
			if cfg, err = applyConfigMask(base, configJSON(f.GetConfig()), paths); err != nil {
				return nil, err
			}
		}
		if _, err := s.checkedConfig(recognizer, cfg); err != nil {
			return nil, err
		}
		bf := batchFile{uri: uri, config: cfg}
		if gcs != nil {
			base := path.Base(uri)
			if ext := path.Ext(base); ext != "" {
				base = strings.TrimSuffix(base, ext)
			}
			bf.out = "gs://" + outBucket + "/" + outPrefix + base + "_transcript_" + newEtag()
		}
		files = append(files, bf)
	}

	s.mu.Lock()
	s.speechRecognizers() // the operations store
	s.mu.Unlock()
	p := strings.Split(req.GetRecognizer(), "/")
	op := s.newOp(strings.Join(p[:4], "/"))
	start := timestamppb.Now()
	meta := func(progress int32, live map[string]*speechpb.BatchRecognizeTranscriptionMetadata) *speechpb.OperationMetadata {
		// The stored metadata is a copy: the run goes on changing live
		// while a client reads the operation.
		tm := make(map[string]*speechpb.BatchRecognizeTranscriptionMetadata, len(live))
		for k, v := range live {
			tm[k] = proto.Clone(v).(*speechpb.BatchRecognizeTranscriptionMetadata)
		}
		return &speechpb.OperationMetadata{CreateTime: start, UpdateTime: timestamppb.Now(), Resource: req.GetRecognizer(),
			Method: "google.cloud.speech.v2.Speech.BatchRecognize", ProgressPercent: progress,
			Request:  &speechpb.OperationMetadata_BatchRecognizeRequest{BatchRecognizeRequest: req},
			Metadata: &speechpb.OperationMetadata_BatchRecognizeMetadata{BatchRecognizeMetadata: &speechpb.BatchRecognizeMetadata{TranscriptionMetadata: tm}}}
	}
	tm := map[string]*speechpb.BatchRecognizeTranscriptionMetadata{}
	for _, f := range files {
		tm[f.uri] = &speechpb.BatchRecognizeTranscriptionMetadata{Uri: nativeURI(f.out)}
	}
	_ = s.ops.SetMetadata(op, meta(0, tm))
	go s.runSpeechBatch(op, recognizer, files, native, vtt, srt, inline != nil, tm, meta)
	got, _ := s.ops.Get(op)
	return opProto(got)
}

func nativeURI(out string) string {
	if out == "" {
		return ""
	}
	return out + ".json"
}

// runSpeechBatch transcribes each file and finishes the operation.
func (s *Server) runSpeechBatch(op, recognizer string, files []batchFile, native, vtt, srt, inline bool,
	tm map[string]*speechpb.BatchRecognizeTranscriptionMetadata,
	meta func(int32, map[string]*speechpb.BatchRecognizeTranscriptionMetadata) *speechpb.OperationMetadata) {
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	resp := &speechpb.BatchRecognizeResponse{Results: map[string]*speechpb.BatchRecognizeFileResult{}}
	var billed time.Duration
	for i, f := range files {
		fr, d, err := s.speechBatchOne(ctx, recognizer, f, native, vtt, srt, inline)
		if err != nil {
			st := apierror.From(err).GRPCStatus().Proto()
			fr = &speechpb.BatchRecognizeFileResult{Error: st}
			tm[f.uri].Error = st
		}
		billed += d
		resp.Results[f.uri] = fr
		tm[f.uri].ProgressPercent = 100
		_ = s.ops.SetMetadata(op, meta(int32(100*(i+1)/len(files)), tm))
	}
	resp.TotalBilledDuration = durationpb.New(billed)
	_ = s.ops.Succeed(op, resp)
}

// speechBatchOne transcribes one file and writes or answers its result.
func (s *Server) speechBatchOne(ctx context.Context, recognizer string, f batchFile, native, vtt, srt, inline bool) (*speechpb.BatchRecognizeFileResult, time.Duration, error) {
	data, err := s.readAudioURI(ctx, f.uri, batchMaxAudioBytes)
	if err != nil {
		return nil, 0, err
	}
	res, samples, cfg, err := s.transcribeAudioMax(ctx, recognizer, f.config, data, batchMaxSeconds)
	if err != nil {
		return nil, 0, err
	}
	b, _ := json.Marshal(recognizeResponse(res, samples, cfg))
	rr := &speechpb.RecognizeResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, rr); err != nil {
		return nil, 0, apierror.Internal(err, "encoding the result: %v", err)
	}
	results := &speechpb.BatchRecognizeResults{Results: rr.GetResults(), Metadata: rr.GetMetadata()}
	billed := rr.GetMetadata().GetTotalBilledDuration().AsDuration()
	fr := &speechpb.BatchRecognizeFileResult{Metadata: rr.GetMetadata()}
	var vttText, srtText string
	if vtt {
		vttText = captions(res, true)
	}
	if srt {
		srtText = captions(res, false)
	}
	if inline {
		ir := &speechpb.InlineResult{VttCaptions: vttText, SrtCaptions: srtText}
		if native {
			ir.Transcript = results
		}
		fr.Result = &speechpb.BatchRecognizeFileResult_InlineResult{InlineResult: ir}
		return fr, billed, nil
	}
	_, w, err := s.storageRW("BatchRecognize")
	if err != nil {
		return nil, 0, err
	}
	csr := &speechpb.CloudStorageResult{}
	write := func(uri, contentType string, body []byte) error {
		bucket, object, _ := splitGCS(uri, false)
		return w.Write(ctx, bucket, object, contentType, body)
	}
	if native {
		csr.Uri = nativeURI(f.out)
		body, _ := protojson.Marshal(results)
		if err := write(csr.Uri, "application/json", body); err != nil {
			return nil, 0, err
		}
	}
	if vtt {
		csr.VttFormatUri = f.out + ".vtt"
		if err := write(csr.VttFormatUri, "text/vtt", []byte(vttText)); err != nil {
			return nil, 0, err
		}
	}
	if srt {
		csr.SrtFormatUri = f.out + ".srt"
		if err := write(csr.SrtFormatUri, "application/x-subrip", []byte(srtText)); err != nil {
			return nil, 0, err
		}
	}
	fr.Result = &speechpb.BatchRecognizeFileResult_CloudStorageResult{CloudStorageResult: csr}
	return fr, billed, nil
}

// captions writes whisper's segments as WebVTT, or as SubRip.
func captions(res speech.Result, vtt bool) string {
	var b strings.Builder
	if vtt {
		b.WriteString("WEBVTT\n\n")
	}
	stamp := func(d time.Duration) string {
		ms := d.Milliseconds()
		sep := ","
		if vtt {
			sep = "."
		}
		return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
	}
	for i, seg := range res.Segments {
		if !vtt {
			fmt.Fprintf(&b, "%d\n", i+1)
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(seg.Start), stamp(seg.End), strings.TrimSpace(seg.Text))
	}
	return b.String()
}
