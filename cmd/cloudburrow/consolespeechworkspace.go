package main

// Speech-to-Text's workspaces (#2084). Google's New Transcription keeps
// its transcriptions in a workspace, a Cloud Storage bucket chosen with
// Workspace > New Workspace (Browse, create or select a bucket, Create)
// (https://docs.cloud.google.com/speech-to-text/docs/models/chirp-3, read
// 2026-11-20); the console asks first-time users "where in Cloud Storage to
// store your configurations and transcriptions" and needs
// storage.buckets.get, list and update
// (https://docs.cloud.google.com/speech-to-text/docs/v1/transcribe-console).
//
// Here a workspace is a bucket of the instance's served Cloud Storage
// carrying the label speechWorkspaceLabel, which New Workspace sets (the
// buckets.update Google's console needs). Each transcription is a folder
// in it, written through the official storage client:
//
//	transcriptions/{id}/metadata.json    what made it (speechWorkspaceMeta)
//	transcriptions/{id}/{audio file}     a Local upload's audio, as uploaded
//	transcriptions/{id}/transcript.json  the RecognizeResponse, in JSON
//
// A Cloud storage file's audio stays where it is; its metadata names it.
// The Transcriptions page lists the workspaces' folders, and Transcription
// details plays the stored audio through the console's media route.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// speechWorkspaceLabel marks a bucket as a Speech-to-Text workspace.
const speechWorkspaceLabel = "speech-to-text-workspace"

// speechNewWorkspace is the Workspace drop-down's New Workspace.
const speechNewWorkspace = "New workspace"

// speechTranscriptionsPrefix is the folder transcriptions are written under.
const speechTranscriptionsPrefix = "transcriptions/"

// speechMetaLimit bounds a metadata.json or transcript.json read back.
const speechMetaLimit = 16 << 20

// speechBucketPattern is Cloud Storage's bucket naming
// (https://docs.cloud.google.com/storage/docs/buckets#naming), the
// dotless form.
var speechBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_\-]{1,61}[a-z0-9]$`)

// speechWorkspaceMeta is a transcription's metadata.json.
type speechWorkspaceMeta struct {
	ID         string    `json:"id"`
	Recognizer string    `json:"recognizer"`
	Model      string    `json:"model,omitempty"`
	Language   string    `json:"language,omitempty"`
	Source     string    `json:"source"`
	AudioFile  string    `json:"audioFile"`
	AudioURI   string    `json:"audioUri"`
	AudioType  string    `json:"audioType,omitempty"`
	Transcript string    `json:"transcriptUri,omitempty"`
	Output     string    `json:"outputUri,omitempty"`
	Created    time.Time `json:"createTime"`
	// Text is the transcript's text, for the list's preview.
	Text string `json:"text,omitempty"`
}

// speechWorkspaces is the served Cloud Storage the workspaces live in.
type speechWorkspaces struct{ storage storageProvider }

func (w speechWorkspaces) served() bool { return w.storage.endpoint != "" }

func (w speechWorkspaces) client(ctx context.Context) (*storage.Client, error) {
	if !w.served() {
		return nil, errors.New("Cloud Storage, which holds Speech-to-Text's workspaces, is not served by this instance")
	}
	return w.storage.storageClient(ctx)
}

// list is the project's workspaces, by bucket name.
func (w speechWorkspaces) list(ctx context.Context, project string) ([]string, error) {
	c, err := w.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []string
	it := c.Buckets(ctx, project)
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		if b.Labels[speechWorkspaceLabel] == "true" {
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ensure makes bucket a workspace: created in the project when absent,
// labelled when it is not yet one.
func (w speechWorkspaces) ensure(ctx context.Context, project, bucket string) error {
	if !speechBucketPattern.MatchString(bucket) {
		return fmt.Errorf("%q is not a bucket name: 3 to 63 lower-case letters, digits, hyphens and underscores", bucket)
	}
	c, err := w.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	b := c.Bucket(bucket)
	attrs, err := b.Attrs(ctx)
	if errors.Is(err, storage.ErrBucketNotExist) {
		return b.Create(ctx, project, &storage.BucketAttrs{Labels: map[string]string{speechWorkspaceLabel: "true"}})
	}
	if err != nil {
		return err
	}
	if attrs.Labels[speechWorkspaceLabel] == "true" {
		return nil
	}
	var up storage.BucketAttrsToUpdate
	up.SetLabel(speechWorkspaceLabel, "true")
	_, err = b.Update(ctx, up)
	return err
}

// chosen is the workspace New transcription's values name, made one when
// New workspace names a bucket.
func (w speechWorkspaces) chosen(ctx context.Context, project string, v map[string]string) (string, error) {
	ws := strings.TrimSpace(v["workspace"])
	if ws == speechNewWorkspace {
		ws = strings.TrimSpace(v["workspaceBucket"])
		if ws == "" {
			return "", errors.New("New workspace needs a bucket")
		}
		return ws, w.ensure(ctx, project, ws)
	}
	if ws == "" {
		return "", errors.New("choose a workspace: the Cloud Storage bucket the transcription is kept in")
	}
	all, err := w.list(ctx, project)
	if err != nil {
		return "", err
	}
	for _, b := range all {
		if b == ws {
			return ws, nil
		}
	}
	return "", fmt.Errorf("%s is not a workspace of this project; choose New workspace to make it one", ws)
}

// newTranscriptionID is a transcription's folder name: when it was made,
// and four random bytes.
func newTranscriptionID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "transcription-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// put writes one object.
func (w speechWorkspaces) put(ctx context.Context, c *storage.Client, bucket, name, contentType string, r io.Reader) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ow := c.Bucket(bucket).Object(name).NewWriter(wctx)
	ow.ContentType = contentType
	ow.ChunkSize = 0
	if _, err := io.Copy(ow, r); err != nil {
		cancel()
		_ = ow.Close()
		return err
	}
	if err := ow.Close(); err != nil {
		return fmt.Errorf("write gs://%s/%s: %w", bucket, name, err)
	}
	return nil
}

// save writes a transcription to the workspace: its audio when it was
// uploaded (audio non-nil), its transcript when it has one, and its
// metadata last, which is what lists it.
func (w speechWorkspaces) save(ctx context.Context, bucket string, meta *speechWorkspaceMeta, audio []byte,
	res *speechpb.RecognizeResponse) error {
	c, err := w.client(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	dir := speechTranscriptionsPrefix + meta.ID + "/"
	if audio != nil {
		name := dir + meta.AudioFile
		if err := w.put(ctx, c, bucket, name, meta.AudioType, bytes.NewReader(audio)); err != nil {
			return err
		}
		meta.AudioURI = "gs://" + bucket + "/" + name
	}
	if res != nil {
		js, err := protojson.MarshalOptions{Multiline: true}.Marshal(res)
		if err != nil {
			return err
		}
		if err := w.put(ctx, c, bucket, dir+"transcript.json", "application/json", bytes.NewReader(js)); err != nil {
			return err
		}
		meta.Transcript = "gs://" + bucket + "/" + dir + "transcript.json"
	}
	js, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return w.put(ctx, c, bucket, dir+"metadata.json", "application/json", bytes.NewReader(js))
}

func readSmall(ctx context.Context, c *storage.Client, bucket, name string) ([]byte, error) {
	r, err := c.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.LimitReader(r, speechMetaLimit))
}

// speechWorkspaceItem is a transcription read back from its workspace.
type speechWorkspaceItem struct {
	bucket string
	meta   speechWorkspaceMeta
}

// transcriptions are every workspace's transcriptions, newest first.
func (w speechWorkspaces) transcriptions(ctx context.Context, project string) ([]speechWorkspaceItem, error) {
	buckets, err := w.list(ctx, project)
	if err != nil {
		return nil, err
	}
	c, err := w.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []speechWorkspaceItem
	for _, b := range buckets {
		it := c.Bucket(b).Objects(ctx, &storage.Query{Prefix: speechTranscriptionsPrefix})
		for {
			o, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return nil, err
			}
			rest := strings.TrimPrefix(o.Name, speechTranscriptionsPrefix)
			if strings.Count(rest, "/") != 1 || path.Base(rest) != "metadata.json" {
				continue
			}
			body, err := readSmall(ctx, c, b, o.Name)
			if err != nil {
				return nil, err
			}
			var m speechWorkspaceMeta
			if json.Unmarshal(body, &m) != nil || m.ID != path.Dir(rest) {
				continue
			}
			out = append(out, speechWorkspaceItem{bucket: b, meta: m})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].meta.Created.After(out[j].meta.Created) })
	return out, nil
}

// transcript reads a transcription's transcript.json back.
func (w speechWorkspaces) transcript(ctx context.Context, bucket, id string) (*speechpb.RecognizeResponse, error) {
	c, err := w.client(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	body, err := readSmall(ctx, c, bucket, speechTranscriptionsPrefix+id+"/transcript.json")
	if err != nil {
		return nil, err
	}
	res := &speechpb.RecognizeResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, res); err != nil {
		return nil, fmt.Errorf("transcript.json of %s: %w", id, err)
	}
	return res, nil
}

// gsPath is a gs:// URI as an object path: the bucket, then the name's
// segments, as the console's object routes take it.
func gsPath(uri string) ([]string, bool) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	if !ok {
		return nil, false
	}
	bucket, name, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || name == "" {
		return nil, false
	}
	return append([]string{bucket}, strings.Split(name, "/")...), true
}

// workspaceDetail is Transcription details for a transcription kept in a
// workspace: what made it, the audio played from its object, and the
// transcript.
func (w speechWorkspaces) detail(ctx context.Context, project string, it speechWorkspaceItem) console.Detail {
	m := it.meta
	summary := []console.Property{
		{Label: "Recognizer", Value: m.Recognizer,
			Link: "/speech/recognizers/" + url.PathEscape(m.Recognizer) + "?project=" + url.QueryEscape(project)},
		{Label: "Model", Value: orDash(m.Model)}, {Label: "Spoken language", Value: orDash(m.Language)},
		{Label: "Workspace", Value: it.bucket, Link: "/storage/browser/" + url.PathEscape(it.bucket) + "?project=" + url.QueryEscape(project)},
		{Label: "Audio source", Value: m.Source}, {Label: "Audio file", Value: m.AudioURI, Copy: "Copy"},
	}
	if m.Transcript != "" {
		summary = append(summary, console.Property{Label: "Transcript file", Value: m.Transcript, Copy: "Copy"})
	}
	if m.Output != "" {
		summary = append(summary, console.Property{Label: "Output location", Value: m.Output, Copy: "Copy"})
	}
	summary = append(summary, console.Property{Label: "Created", Value: m.Created.Format(time.RFC3339)})
	player := console.Section{ID: "transcript", Label: "Transcript", Kind: console.KindMedia, Text: orDash(m.Text)}
	if p, ok := gsPath(m.AudioURI); ok {
		player.Media = &console.Media{Src: console.MediaURL("storage", project, p), Type: m.AudioType,
			Label: "Audio: " + path.Base(m.AudioURI)}
	}
	d := console.Detail{Heading: "Transcription details", Title: m.ID, Summary: summary, Sections: []console.Section{player}}
	if m.Transcript == "" {
		return d
	}
	res, err := w.transcript(ctx, it.bucket, m.ID)
	results := console.Section{ID: "results", Label: "Results"}
	if err != nil {
		results.Unavailable = consoleErr(err)
	} else {
		l := transcriptResult(res)
		l.Text = ""
		results.Listing = *l
	}
	d.Sections = append(d.Sections, results)
	return d
}

// workspaceFields are New transcription's Workspace drop-down, the
// project's workspaces and New workspace, whose bucket is created, or made
// a workspace, on Submit.
func workspaceFields(ws []string) []console.Field {
	opts := append(append([]string{}, ws...), speechNewWorkspace)
	return []console.Field{
		{Name: "workspace", Label: "Workspace", Type: "select", Options: opts, Default: opts[0], Required: true,
			Section: "Audio configuration", Help: "the Cloud Storage bucket the audio and transcript are kept in"},
		{Name: "workspaceBucket", Label: "Workspace bucket", Type: "text", Required: true, Section: "Audio configuration", Browse: "storage-bucket",
			VisibleWhen: map[string]string{"workspace": speechNewWorkspace}, Pattern: speechBucketPattern.String(),
			Help: "a bucket of this instance's Cloud Storage: created when it does not exist, then made a workspace"},
	}
}

// transcriptionHref is a transcription's Transcription details page.
func transcriptionHref(project, id string) string {
	return "/speech/transcriptions/" + url.PathEscape(id) + "?project=" + url.QueryEscape(project)
}

// listWorkspaces is the Transcriptions page read from the workspaces.
func (p speechTranscriptionsProvider) listWorkspaces(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Recognizer", "Model", "Language", "Audio file", "Workspace", "Transcript", "Created"},
		NameColumn: "Transcription", Noun: "transcriptions", RowsOpenable: true, AlwaysStatus: true,
		Note: "New transcription sends a Local upload to Recognize, or a Cloud storage file to BatchRecognize, with a recognizer's " +
			"default config and the Transcription options chosen over it, and keeps the audio and transcript in a workspace: a " +
			"Cloud Storage bucket, under transcriptions/."}
	items, err := p.ws.transcriptions(ctx, p.s.project(project))
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	for _, it := range items {
		m := it.meta
		preview := m.Text
		if r := []rune(preview); len(r) > 80 {
			preview = string(r[:80]) + "…"
		}
		out.Items = append(out.Items, console.Resource{Name: m.ID, Status: "Succeeded", Fields: map[string]string{
			"Recognizer": m.Recognizer, "Model": orDash(m.Model), "Language": orDash(m.Language), "Audio file": m.AudioFile,
			"Workspace": it.bucket, "Transcript": orDash(preview), "Created": m.Created.Format(time.RFC3339)}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// workspaceDetail finds a transcription in the workspaces.
func (p speechTranscriptionsProvider) workspaceDetail(ctx context.Context, project, id string) (console.Detail, error) {
	project = p.s.project(project)
	items, err := p.ws.transcriptions(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	for _, it := range items {
		if it.meta.ID == id {
			return p.ws.detail(ctx, project, it), nil
		}
	}
	return console.Detail{Unavailable: fmt.Sprintf("transcription %q is in none of this project's workspaces", id)}, nil
}

// transcribeToWorkspace is New transcription of a Local upload: the audio
// sent to Recognize, then it and its transcript written to the workspace.
// The answer opens its Transcription details.
func (p speechTranscriptionsProvider) transcribeToWorkspace(ctx context.Context, project string, r *speechpb.Recognizer,
	v map[string]string, f console.UploadedFile) (*console.Listing, error) {
	project = p.s.project(project)
	bucket, err := p.ws.chosen(ctx, project, v)
	if err != nil {
		return nil, err
	}
	audio, err := io.ReadAll(io.LimitReader(f, speechMaxAudio+1))
	if err != nil {
		return nil, err
	}
	cfg, mask := speechOverrides(v)
	res, err := p.s.recognizeWith(ctx, r.GetName(), bytes.NewReader(audio), cfg, mask)
	if err != nil {
		return nil, err
	}
	name := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "audio"
	}
	ct := console.MediaType(name, f.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	now := time.Now().UTC()
	meta := &speechWorkspaceMeta{ID: newTranscriptionID(now), Recognizer: lastSegment(r.GetName()), Source: speechSourceLocal,
		AudioFile: name, AudioType: ct, Created: now, Text: transcriptText(res)}
	meta.Model, meta.Language = effective(r, v)
	if err := p.ws.save(ctx, bucket, meta, audio, res); err != nil {
		return nil, err
	}
	out := transcriptResult(res)
	out.Open = transcriptionHref(project, meta.ID)
	return out, nil
}

// batchToWorkspace is New transcription of a Cloud storage file:
// BatchRecognize, then its transcript written to the workspace beside the
// metadata naming the file. With an Output location, the transcript is the
// BatchRecognizeResults written there, and the workspace names it.
func (p speechTranscriptionsProvider) batchToWorkspace(ctx context.Context, project string, r *speechpb.Recognizer,
	v map[string]string, uri, output string) (*console.Listing, error) {
	project = p.s.project(project)
	bucket, err := p.ws.chosen(ctx, project, v)
	if err != nil {
		return nil, err
	}
	cfg, mask := speechOverrides(v)
	fr, err := p.s.batchRecognize(ctx, r.GetName(), uri, output, cfg, mask)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	meta := &speechWorkspaceMeta{ID: newTranscriptionID(now), Recognizer: lastSegment(r.GetName()), Source: speechSourceGCS,
		AudioFile: path.Base(uri), AudioURI: uri, AudioType: console.MediaType(uri, ""), Created: now,
		Output: fr.GetCloudStorageResult().GetUri()}
	meta.Model, meta.Language = effective(r, v)
	var res *speechpb.RecognizeResponse
	if tr := fr.GetInlineResult().GetTranscript(); tr != nil {
		res = &speechpb.RecognizeResponse{Results: tr.GetResults(), Metadata: tr.GetMetadata()}
		meta.Text = transcriptText(res)
	} else if meta.Output != "" {
		meta.Text = "Written to " + meta.Output
	}
	if err := p.ws.save(ctx, bucket, meta, nil, res); err != nil {
		return nil, err
	}
	out := batchResult(fr)
	out.Open = transcriptionHref(project, meta.ID)
	return out, nil
}
