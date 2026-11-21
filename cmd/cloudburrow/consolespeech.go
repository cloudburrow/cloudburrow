package main

// Speech-to-Text's console (#1930): Transcriptions, where New transcription
// uploads audio and shows its transcript, or transcribes a Cloud Storage file
// with BatchRecognize; Recognizers, with Create recognizer, a recognizer's
// page, Edit, Delete, Undelete and Transcribe; and Custom classes and Phrase
// sets (consolespeechadaptation.go).
// Google's console opens on Transcriptions
// (https://docs.cloud.google.com/speech-to-text/docs/migration, read
// 2026-11-19: "navigate to the Transcriptions page", "New Transcription");
// its recognizers are Speech-to-Text v2's, which the local AI endpoint serves
// (#1799). Every read and change goes through the official
// cloud.google.com/go/speech/apiv2 client on that endpoint; nothing is sent
// to Google.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// speechRegions are the locations the pages list and create recognizers
// in: global and Speech-to-Text v2's documented regions
// (https://docs.cloud.google.com/speech-to-text/docs/speech-to-text-supported-languages).
var speechRegions = []string{"global", "us", "eu", "us-central1", "europe-west4", "asia-southeast1", "asia-northeast1"}

// speechMaxAudio is Recognize's limit on inline audio: 10 MB and one minute
// (https://docs.cloud.google.com/speech-to-text/docs/quotas).
const speechMaxAudio = 10 << 20

// speechSvc is what Speech-to-Text's pages share.
type speechSvc struct {
	defaultProject string
	addr           func() string
}

func (s speechSvc) project(project string) string {
	if project == "" {
		return s.defaultProject
	}
	return project
}

func (s speechSvc) withClient(ctx context.Context, fn func(*speech.Client) error) error {
	addr := ""
	if s.addr != nil {
		addr = s.addr()
	}
	if addr == "" {
		return errors.New("the local AI endpoint, which serves Speech-to-Text, is not running")
	}
	c, err := speech.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Speech-to-Text: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// recognizers are the project's recognizers in every listed region,
// deleted ones too.
func (s speechSvc) recognizers(ctx context.Context, project string) ([]*speechpb.Recognizer, error) {
	var out []*speechpb.Recognizer
	err := s.withClient(ctx, func(c *speech.Client) error {
		for _, loc := range speechRegions {
			it := c.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: "projects/" + s.project(project) + "/locations/" + loc,
				ShowDeleted: true})
			for {
				r, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// recognizer resolves a recognizer by its ID; one ID in two regions is
// refused, since the pages name recognizers by ID.
func (s speechSvc) recognizer(ctx context.Context, project, id string) (*speechpb.Recognizer, error) {
	all, err := s.recognizers(ctx, project)
	if err != nil {
		return nil, err
	}
	var match []*speechpb.Recognizer
	for _, r := range all {
		if lastSegment(r.GetName()) == id {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("recognizer %q not found", id)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("recognizer %q is in %d regions; use the API with its region", id, len(match))
}

// recognize transcribes audio with a recognizer, its default config.
func (s speechSvc) recognize(ctx context.Context, name string, f io.Reader) (*speechpb.RecognizeResponse, error) {
	return s.recognizeWith(ctx, name, f, nil, nil)
}

// speechOverrides are New transcription's Transcription options over the
// recognizer's default config (#1930): a Spoken language and a Model, each
// sent through config_mask only when chosen, as Google's console sends what
// its Transcription options page sets.
func speechOverrides(v map[string]string) (*speechpb.RecognitionConfig, *fieldmaskpb.FieldMask) {
	cfg, mask := &speechpb.RecognitionConfig{}, &fieldmaskpb.FieldMask{}
	if m := strings.TrimSpace(v["model"]); m != "" {
		cfg.Model = m
		mask.Paths = append(mask.Paths, "model")
	}
	if l := strings.TrimSpace(v["language"]); l != "" {
		for _, c := range strings.Split(l, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cfg.LanguageCodes = append(cfg.LanguageCodes, c)
			}
		}
		mask.Paths = append(mask.Paths, "language_codes")
	}
	if len(mask.Paths) == 0 {
		return nil, nil
	}
	return cfg, mask
}

// recognizeWith transcribes audio with a recognizer, cfg's masked fields
// over its default config.
func (s speechSvc) recognizeWith(ctx context.Context, name string, f io.Reader, cfg *speechpb.RecognitionConfig,
	mask *fieldmaskpb.FieldMask) (*speechpb.RecognizeResponse, error) {
	audio, err := io.ReadAll(io.LimitReader(f, speechMaxAudio+1))
	if err != nil {
		return nil, err
	}
	if len(audio) > speechMaxAudio {
		return nil, errors.New("the audio is larger than Recognize's 10 MB limit")
	}
	var res *speechpb.RecognizeResponse
	err = s.withClient(ctx, func(c *speech.Client) error {
		var e error
		res, e = c.Recognize(ctx, &speechpb.RecognizeRequest{Recognizer: name, Config: cfg, ConfigMask: mask,
			AudioSource: &speechpb.RecognizeRequest_Content{Content: audio}})
		return e
	})
	return res, err
}

// transcriptResult is a RecognizeResponse as the pages show it: the
// transcript, and a row per result.
func transcriptResult(res *speechpb.RecognizeResponse) *console.Listing {
	out := &console.Listing{Columns: []string{"Transcript", "Confidence", "Language", "End offset"}, NameColumn: "Result",
		Noun: "results", Text: transcriptText(res)}
	for i, r := range res.GetResults() {
		alt := r.GetAlternatives()
		tr, conf := "", ""
		if len(alt) > 0 {
			tr, conf = strings.TrimSpace(alt[0].GetTranscript()), strconv.FormatFloat(float64(alt[0].GetConfidence()), 'f', 2, 32)
		}
		out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{"Transcript": tr,
			"Confidence": conf, "Language": r.GetLanguageCode(), "End offset": r.GetResultEndOffset().AsDuration().String()}})
	}
	out.Total = len(out.Items)
	if d := res.GetMetadata().GetTotalBilledDuration(); d != nil {
		out.Note = "Audio duration " + d.AsDuration().String() + ", transcribed locally with whisper.cpp."
	}
	return out
}

func transcriptText(res *speechpb.RecognizeResponse) string {
	var parts []string
	for _, r := range res.GetResults() {
		if a := r.GetAlternatives(); len(a) > 0 && strings.TrimSpace(a[0].GetTranscript()) != "" {
			parts = append(parts, strings.TrimSpace(a[0].GetTranscript()))
		}
	}
	return strings.Join(parts, "\n")
}

// --- Recognizers --------------------------------------------------------------

// speechRecognizersProvider is the Recognizers page.
type speechRecognizersProvider struct{ s speechSvc }

func (speechRecognizersProvider) ID() string    { return "speech-recognizers" }
func (speechRecognizersProvider) Title() string { return "Recognizers" }

func (p speechRecognizersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Region", "Model", "Language", "Updated"}, NameColumn: "Recognizer ID",
		Noun: "recognizers", AlwaysStatus: true, RowsOpenable: true,
		Note: "Recognizers in " + strings.Join(speechRegions, ", ") + ". A deleted recognizer is listed until it expires, with Undelete. " +
			"Google's models " + strings.Join(speechconfig.Models(), ", ") + " are served, each on local whisper.cpp."}
	rs, err := p.s.recognizers(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	sort.SliceStable(rs, func(i, j int) bool { return lastSegment(rs[i].GetName()) < lastSegment(rs[j].GetName()) })
	for _, r := range rs {
		c := r.GetDefaultRecognitionConfig()
		out.Items = append(out.Items, console.Resource{Name: lastSegment(r.GetName()), Status: stateWord(r.GetState().String()),
			Fields: map[string]string{"Display name": orDash(r.GetDisplayName()), "Region": locationOf(r.GetName()),
				"Model": orDash(c.GetModel()), "Language": orDash(strings.Join(c.GetLanguageCodes(), ", ")),
				"Updated": stampAge(r.GetUpdateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// recognizerConfigFields are the default recognition config the console
// sets, from cur.
func recognizerConfigFields(cur *speechpb.Recognizer) []console.Field {
	c := cur.GetDefaultRecognitionConfig()
	lang := "en-US"
	if l := c.GetLanguageCodes(); len(l) > 0 {
		lang = l[0]
	}
	f := c.GetFeatures()
	model := c.GetModel()
	if model == "" {
		model = "chirp"
	}
	return []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: cur.GetDisplayName(), Pattern: "^.{0,63}$"},
		{Name: "model", Label: "Model", Type: "select", Options: speechconfig.Models(), Default: model,
			Help: "each runs on local whisper.cpp; the medical models are not available locally"},
		{Name: "language", Label: "Language", Type: "text", Required: true, Default: lang,
			Help: "a BCP-47 language code such as en-US, or auto to detect it", Pattern: "^([A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*|auto)$"},
		{Name: "wordTimeOffsets", Label: "Word timestamps", Type: "checkbox", Default: strconv.FormatBool(f.GetEnableWordTimeOffsets())},
		{Name: "wordConfidence", Label: "Word-level confidence", Type: "checkbox", Default: strconv.FormatBool(f.GetEnableWordConfidence())},
		{Name: "punctuation", Label: "Automatic punctuation", Type: "checkbox", Default: strconv.FormatBool(f.GetEnableAutomaticPunctuation())},
	}
}

// recognitionConfig is the config the form's values describe.
func recognitionConfig(v map[string]string) *speechpb.RecognitionConfig {
	model := strings.TrimSpace(v["model"])
	if model == "" {
		model = "chirp"
	}
	return &speechpb.RecognitionConfig{Model: model, LanguageCodes: []string{strings.TrimSpace(v["language"])},
		DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{}},
		Features: &speechpb.RecognitionFeatures{EnableWordTimeOffsets: v["wordTimeOffsets"] == "true",
			EnableWordConfidence: v["wordConfidence"] == "true", EnableAutomaticPunctuation: v["punctuation"] == "true"}}
}

func (p speechRecognizersProvider) CreateForm() (string, []console.Field) {
	return "Create recognizer", append([]console.Field{
		{Name: "name", Label: "Recognizer ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9\\-]{2,61}[a-z0-9]$",
			Help: "4 to 63 lower-case letters, digits and hyphens, starting with a letter"},
		{Name: "region", Label: "Region", Type: "select", Options: speechRegions, Default: "global"},
	}, recognizerConfigFields(&speechpb.Recognizer{})...)
}

func (p speechRecognizersProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	id := strings.TrimSpace(v["name"])
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	err := p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.CreateRecognizer(ctx, &speechpb.CreateRecognizerRequest{Parent: "projects/" + p.s.project(project) + "/locations/" + region,
			RecognizerId: id, Recognizer: &speechpb.Recognizer{DisplayName: strings.TrimSpace(v["displayName"]),
				DefaultRecognitionConfig: recognitionConfig(v)}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

func (p speechRecognizersProvider) Delete(ctx context.Context, project, id string) error {
	r, err := p.s.recognizer(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.DeleteRecognizer(ctx, &speechpb.DeleteRecognizerRequest{Name: r.GetName(), Etag: r.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

func (p speechRecognizersProvider) undelete(ctx context.Context, project, id string) error {
	r, err := p.s.recognizer(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.UndeleteRecognizer(ctx, &speechpb.UndeleteRecognizerRequest{Name: r.GetName(), Etag: r.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// Actions implements console.Actor: a deleted recognizer's Undelete.
func (p speechRecognizersProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "Deleted" {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return nil
}

func (p speechRecognizersProvider) Act(ctx context.Context, project, id, action string) error {
	if action != "undelete" {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.undelete(ctx, project, id)
}

// Detail implements console.Driller: a recognizer's page.
func (p speechRecognizersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	r, err := p.s.recognizer(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	c := r.GetDefaultRecognitionConfig()
	f := c.GetFeatures()
	onOff := func(b bool) string {
		if b {
			return "On"
		}
		return "Off"
	}
	decoding := "Auto-detect"
	if e := c.GetExplicitDecodingConfig(); e != nil {
		decoding = fmt.Sprintf("%s, %d Hz, %d channels", e.GetEncoding(), e.GetSampleRateHertz(), e.GetAudioChannelCount())
	}
	d := console.Detail{Summary: []console.Property{{Label: "Name", Value: r.GetName()},
		{Label: "Display name", Value: r.GetDisplayName()}, {Label: "State", Value: stateWord(r.GetState().String())},
		{Label: "Region", Value: locationOf(r.GetName())}, {Label: "UID", Value: r.GetUid()}, {Label: "Etag", Value: r.GetEtag()},
		{Label: "Created", Value: stampAge(r.GetCreateTime())}, {Label: "Updated", Value: stampAge(r.GetUpdateTime())}}}
	if r.GetState() == speechpb.Recognizer_DELETED {
		d.Summary = append(d.Summary, console.Property{Label: "Deleted", Value: stampAge(r.GetDeleteTime())},
			console.Property{Label: "Expires", Value: r.GetExpireTime().AsTime().Format(time.RFC3339)})
	}
	d.Sections = []console.Section{{ID: "config", Label: "Default recognition config", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: []console.Property{{Label: "Model", Value: orDash(c.GetModel())},
			{Label: "Language", Value: orDash(strings.Join(c.GetLanguageCodes(), ", "))}, {Label: "Decoding", Value: decoding},
			{Label: "Word timestamps", Value: onOff(f.GetEnableWordTimeOffsets())},
			{Label: "Word-level confidence", Value: onOff(f.GetEnableWordConfidence())},
			{Label: "Automatic punctuation", Value: onOff(f.GetEnableAutomaticPunctuation())}}}}}}
	return d, nil
}

// DetailActions implements console.PathActor: Transcribe, Edit and Delete
// on an active recognizer; Undelete on a deleted one.
func (p speechRecognizersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	r, err := p.s.recognizer(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if r.GetState() == speechpb.Recognizer_DELETED {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return []console.Action{
		{ID: "transcribe", Label: "Transcribe", Submit: "Transcribe", Fields: []console.Field{{Name: "file", Label: "Audio file",
			Type: console.FileFieldType, Required: true, Help: "WAV, FLAC, MP3, OGG Opus or WebM Opus, at most 10 MB and one minute"}}},
		{ID: "edit", Label: "Edit", Submit: "Save", Fields: append(recognizerConfigFields(r),
			console.Field{Name: "etag", Type: "hidden", Default: r.GetEtag()})},
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: "the recognizer: it can be undeleted until it expires"},
	}
}

// ListActions implements console.ListActor: Encryption settings, a
// location's Config and its customer-managed key (#1950), which encrypts
// the recognizers, custom classes and phrase sets created after it.
func (p speechRecognizersProvider) ListActions(ctx context.Context, project string) []console.Action {
	return []console.Action{{ID: "encryption", Label: "Encryption settings", Submit: "Save", Fields: []console.Field{
		{Name: "region", Label: "Region", Type: "select", Options: speechRegions, Default: "global"},
		{Name: "kmsKeyName", Label: "Customer-managed encryption key", Type: "text", Browse: "kms",
			Help: "A key of this instance's Cloud KMS in the region: the recognizers, custom classes and phrase sets created " +
				"after it are encrypted with it, and an existing one when it is next updated. Empty removes it. The Speech-to-Text " +
				"service agent needs Cloud KMS CryptoKey Encrypter/Decrypter on it."},
	}}}
}

// setEncryption is Encryption settings: UpdateConfig of the region's Config.
func (p speechRecognizersProvider) setEncryption(ctx context.Context, project string, v map[string]string) error {
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	return p.s.withClient(ctx, func(c *speech.Client) error {
		_, err := c.UpdateConfig(ctx, &speechpb.UpdateConfigRequest{Config: &speechpb.Config{
			Name: "projects/" + p.s.project(project) + "/locations/" + region + "/config", KmsKeyName: strings.TrimSpace(v["kmsKeyName"])}})
		return err
	})
}

func (p speechRecognizersProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) == 0 && action == "encryption" {
		return p.setEncryption(ctx, project, v)
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	switch action {
	case "undelete":
		return p.undelete(ctx, project, path[0])
	case "delete":
		return p.Delete(ctx, project, path[0])
	case "edit":
		r, err := p.s.recognizer(ctx, project, path[0])
		if err != nil {
			return err
		}
		return p.s.withClient(ctx, func(c *speech.Client) error {
			op, err := c.UpdateRecognizer(ctx, &speechpb.UpdateRecognizerRequest{Recognizer: &speechpb.Recognizer{Name: r.GetName(),
				Etag: v["etag"], DisplayName: strings.TrimSpace(v["displayName"]), DefaultRecognitionConfig: recognitionConfig(v)},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "default_recognition_config"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

// ActAtFile implements console.FileActor: Transcribe.
func (p speechRecognizersProvider) ActAtFile(ctx context.Context, project string, path []string, action string,
	_ map[string]string, f console.UploadedFile) (*console.Listing, error) {
	if len(path) != 1 || action != "transcribe" {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	r, err := p.s.recognizer(ctx, project, path[0])
	if err != nil {
		return nil, err
	}
	res, err := p.s.recognize(ctx, r.GetName(), f)
	if err != nil {
		return nil, err
	}
	return transcriptResult(res), nil
}

// --- Transcriptions -------------------------------------------------------------

// speechTranscription is one transcription New transcription made.
type speechTranscription struct {
	id, project, recognizer, file, text string
	// model and language are what transcribed it: the Transcription
	// options' choices, or the recognizer's default config's.
	model, language string
	when            time.Time
	result          *console.Listing
}

// speechSourceLocal and speechSourceGCS are New transcription's audio
// sources, Google's Local upload and Cloud storage.
const (
	speechSourceLocal = "Local upload"
	speechSourceGCS   = "Cloud storage"
)

// effective is the model and language a transcription with these options
// used: the chosen ones, or the recognizer's.
func effective(r *speechpb.Recognizer, v map[string]string) (model, language string) {
	c := r.GetDefaultRecognitionConfig()
	model, language = c.GetModel(), strings.Join(c.GetLanguageCodes(), ", ")
	if m := strings.TrimSpace(v["model"]); m != "" {
		model = m
	}
	if l := strings.TrimSpace(v["language"]); l != "" {
		language = l
	}
	return model, language
}

// speechTranscriptions are this console's transcriptions, held in memory:
// Recognize keeps nothing, and Google's list of stored transcriptions is
// batch recognition's (BatchRecognize to Cloud Storage), which is not
// served.
type speechTranscriptions struct {
	mu   sync.Mutex
	next int
	list []speechTranscription
}

// speechTranscriptionsProvider is the Transcriptions page.
type speechTranscriptionsProvider struct {
	s    speechSvc
	held *speechTranscriptions
}

func (speechTranscriptionsProvider) ID() string    { return "speech-transcriptions" }
func (speechTranscriptionsProvider) Title() string { return "Transcriptions" }

func (p speechTranscriptionsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Recognizer", "Model", "Language", "Audio file", "Transcript", "Created"},
		NameColumn: "Transcription", Noun: "transcriptions", RowsOpenable: true,
		Note: "New transcription sends a Local upload to Recognize, or a Cloud storage file to BatchRecognize (its transcript " +
			"shown here or written under a gs:// prefix), with a recognizer's default config and the Transcription options " +
			"chosen over it. The transcriptions made here are listed in this console's memory until the instance stops."}
	p.held.mu.Lock()
	defer p.held.mu.Unlock()
	project = p.s.project(project)
	for i := len(p.held.list) - 1; i >= 0; i-- {
		t := p.held.list[i]
		if t.project != project {
			continue
		}
		preview := t.text
		if r := []rune(preview); len(r) > 80 {
			preview = string(r[:80]) + "…"
		}
		out.Items = append(out.Items, console.Resource{Name: t.id, Status: "Succeeded", Fields: map[string]string{
			"Recognizer": t.recognizer, "Model": orDash(t.model), "Language": orDash(t.language), "Audio file": t.file,
			"Transcript": orDash(preview), "Created": t.when.Format(time.RFC3339)}})
	}
	out.AlwaysStatus = true
	out.Total = len(out.Items)
	return out, nil
}

// ListActions implements console.ListActor: New transcription.
func (p speechTranscriptionsProvider) ListActions(ctx context.Context, project string) []console.Action {
	rs, err := p.s.recognizers(ctx, project)
	if err != nil {
		return nil
	}
	var ids []string
	for _, r := range rs {
		if r.GetState() == speechpb.Recognizer_ACTIVE {
			ids = append(ids, lastSegment(r.GetName()))
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []console.Action{{ID: "create-recognizer", Label: "New transcription",
			Href: "/speech/recognizers?project=" + url.QueryEscape(p.s.project(project))}}
	}
	return []console.Action{newTranscriptionAction(ids)}
}

// newTranscriptionAction is Google's New Transcription (#1930), read from
// https://docs.cloud.google.com/speech-to-text/docs/models/chirp-3 and
// …/docs/migration (2026-11-20): its Audio configuration, a Local upload
// or a Cloud storage file, then Continue to its Transcription options, the
// Recognizer with a Spoken language and a Model over its default config,
// and Submit. A Cloud storage file may have its transcript written under a
// gs:// prefix. Google keeps the console's transcriptions in a workspace
// bucket; these are kept in the console's memory.
func newTranscriptionAction(ids []string) console.Action {
	local := map[string]string{"source": speechSourceLocal}
	gcs := map[string]string{"source": speechSourceGCS}
	models := append([]string{""}, speechconfig.Models()...)
	labels := map[string]string{"": "The recognizer's model"}
	return console.Action{ID: "transcribe", Label: "New transcription", Title: "New transcription", Submit: "Submit",
		Steps: "continue", Fields: []console.Field{
			{Name: "source", Label: "Audio source", Type: "radio", Section: "Audio configuration", Default: speechSourceLocal,
				Options: []string{speechSourceLocal, speechSourceGCS}},
			{Name: "file", Label: "Audio file", Type: console.FileFieldType, Required: true, Section: "Audio configuration",
				VisibleWhen: local, Help: "WAV, FLAC, MP3, OGG Opus or WebM Opus, at most 10 MB and one minute, sent to Recognize"},
			{Name: "uri", Label: "Audio file in Cloud Storage", Type: "text", Required: true, Section: "Audio configuration",
				VisibleWhen: gcs, Pattern: `^gs://[^\/]+\/.+$`, Help: "gs://bucket/object, up to 30 minutes of audio, sent to BatchRecognize"},
			{Name: "output", Label: "Output location", Type: "text", Section: "Audio configuration", VisibleWhen: gcs,
				Pattern: `^(gs://[^\/]+(\/.*)?)?$`, Help: "optional: a gs:// prefix the transcript is written under; empty to show it here"},
			{Name: "recognizer", Label: "Recognizer", Type: "select", Options: ids, Default: ids[0], Required: true,
				Section: "Transcription options", Help: "its default recognition config transcribes the audio"},
			{Name: "language", Label: "Spoken language", Type: "text", Section: "Transcription options", Pattern: `^[A-Za-z0-9\-, ]*$`,
				Help: "optional: a BCP-47 code such as en-US, or several separated by commas; empty for the recognizer's"},
			{Name: "model", Label: "Model", Type: "select", Section: "Transcription options", Options: models, OptionLabels: labels},
		}}
}

func (p speechTranscriptionsProvider) DetailActions(context.Context, string, []string) []console.Action {
	return nil
}

// ActAt implements console.PathActor: New transcription of a Cloud
// storage file; a Local upload is ActAtFile's.
func (p speechTranscriptionsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, v)
	return err
}

// ActAtFile implements console.FileActor: New transcription.
func (p speechTranscriptionsProvider) ActAtFile(ctx context.Context, project string, path []string, action string,
	v map[string]string, f console.UploadedFile) (*console.Listing, error) {
	if len(path) != 0 || action != "transcribe" {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	r, err := p.s.recognizer(ctx, project, strings.TrimSpace(v["recognizer"]))
	if err != nil {
		return nil, err
	}
	cfg, mask := speechOverrides(v)
	res, err := p.s.recognizeWith(ctx, r.GetName(), f, cfg, mask)
	if err != nil {
		return nil, err
	}
	out := transcriptResult(res)
	model, lang := effective(r, v)
	p.held.mu.Lock()
	p.held.next++
	t := speechTranscription{id: "transcription-" + strconv.Itoa(p.held.next), project: p.s.project(project),
		recognizer: lastSegment(r.GetName()), file: f.Name, text: out.Text, model: model, language: lang,
		when: time.Now().UTC(), result: out}
	p.held.list = append(p.held.list, t)
	p.held.mu.Unlock()
	return out, nil
}

// Detail implements console.Driller: a transcription's page, its
// transcript and results.
func (p speechTranscriptionsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	p.held.mu.Lock()
	defer p.held.mu.Unlock()
	for _, t := range p.held.list {
		if t.id != path[0] || t.project != p.s.project(project) {
			continue
		}
		res := *t.result
		res.Text = ""
		// Google's Transcription details page: the transcript, with what
		// made it (#1930).
		return console.Detail{Heading: "Transcription details", Title: t.id, Summary: []console.Property{{Label: "Recognizer", Value: t.recognizer,
			Link: "/speech/recognizers/" + url.PathEscape(t.recognizer) + "?project=" + url.QueryEscape(t.project)},
			{Label: "Model", Value: orDash(t.model)}, {Label: "Spoken language", Value: orDash(t.language)},
			{Label: "Audio file", Value: t.file}, {Label: "Created", Value: t.when.Format(time.RFC3339)}},
			Sections: []console.Section{{ID: "transcript", Label: "Transcript", Kind: console.KindText, Text: orDash(t.text)},
				{ID: "results", Label: "Results", Listing: res}}}, nil
	}
	return console.Detail{Unavailable: fmt.Sprintf("transcription %q not found: transcriptions are kept until the instance stops", path[0])}, nil
}

var (
	_ console.Creator   = speechRecognizersProvider{}
	_ console.Deleter   = speechRecognizersProvider{}
	_ console.Driller   = speechRecognizersProvider{}
	_ console.Actor     = speechRecognizersProvider{}
	_ console.FileActor = speechRecognizersProvider{}
	_ console.ListActor = speechTranscriptionsProvider{}
	_ console.FileActor = speechTranscriptionsProvider{}
	_ console.Driller   = speechTranscriptionsProvider{}
)
