package main

// Speech-to-Text's model adaptation pages (#1930): Custom classes and
// Phrase sets, after Recognizers, each with its list (deleted ones too, with
// Undelete), Create, its page, Edit, Delete and Undelete; and the
// Transcriptions page's batch transcription from Cloud Storage
// (BatchRecognize). Every read and change goes through the official
// cloud.google.com/go/speech/apiv2 client on the local AI endpoint, as the
// recognizers' pages do (consolespeech.go).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// speechAdaptIDField is the ID field Create shows.
func speechAdaptIDField(label string) console.Field {
	return console.Field{Name: "name", Label: label, Type: "text", Required: true, Pattern: "^[a-z][a-z0-9\\-]{2,61}[a-z0-9]$",
		Help: "4 to 63 lower-case letters, digits and hyphens, starting with a letter"}
}

// eachRegion runs fn on each listed region's parent.
func (s speechSvc) eachRegion(project string, fn func(parent string) error) error {
	for _, loc := range speechRegions {
		if err := fn("projects/" + s.project(project) + "/locations/" + loc); err != nil {
			return err
		}
	}
	return nil
}

// byID picks the one name whose last segment is id.
func byID[T interface{ GetName() string }](all []T, noun, id string) (T, error) {
	var match []T
	for _, x := range all {
		if lastSegment(x.GetName()) == id {
			match = append(match, x)
		}
	}
	var zero T
	switch len(match) {
	case 0:
		return zero, fmt.Errorf("%s %q not found", noun, id)
	case 1:
		return match[0], nil
	}
	return zero, fmt.Errorf("%s %q is in %d regions; use the API with its region", noun, id, len(match))
}

func (s speechSvc) customClasses(ctx context.Context, project string) ([]*speechpb.CustomClass, error) {
	var out []*speechpb.CustomClass
	err := s.withClient(ctx, func(c *speech.Client) error {
		return s.eachRegion(project, func(parent string) error {
			it := c.ListCustomClasses(ctx, &speechpb.ListCustomClassesRequest{Parent: parent, ShowDeleted: true, PageSize: 100})
			for {
				x, err := it.Next()
				if err == iterator.Done {
					return nil
				}
				if err != nil {
					return err
				}
				out = append(out, x)
			}
		})
	})
	sort.SliceStable(out, func(i, j int) bool { return lastSegment(out[i].GetName()) < lastSegment(out[j].GetName()) })
	return out, err
}

func (s speechSvc) phraseSets(ctx context.Context, project string) ([]*speechpb.PhraseSet, error) {
	var out []*speechpb.PhraseSet
	err := s.withClient(ctx, func(c *speech.Client) error {
		return s.eachRegion(project, func(parent string) error {
			it := c.ListPhraseSets(ctx, &speechpb.ListPhraseSetsRequest{Parent: parent, ShowDeleted: true, PageSize: 100})
			for {
				x, err := it.Next()
				if err == iterator.Done {
					return nil
				}
				if err != nil {
					return err
				}
				out = append(out, x)
			}
		})
	})
	sort.SliceStable(out, func(i, j int) bool { return lastSegment(out[i].GetName()) < lastSegment(out[j].GetName()) })
	return out, err
}

func (s speechSvc) customClass(ctx context.Context, project, id string) (*speechpb.CustomClass, error) {
	all, err := s.customClasses(ctx, project)
	if err != nil {
		return nil, err
	}
	return byID(all, "custom class", id)
}

func (s speechSvc) phraseSet(ctx context.Context, project, id string) (*speechpb.PhraseSet, error) {
	all, err := s.phraseSets(ctx, project)
	if err != nil {
		return nil, err
	}
	return byID(all, "phrase set", id)
}

// lines are a textarea's non-blank lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func classItems(v string) []*speechpb.CustomClass_ClassItem {
	var out []*speechpb.CustomClass_ClassItem
	for _, l := range lines(v) {
		out = append(out, &speechpb.CustomClass_ClassItem{Value: l})
	}
	return out
}

func classItemsText(c *speechpb.CustomClass) string {
	var out []string
	for _, it := range c.GetItems() {
		out = append(out, it.GetValue())
	}
	return strings.Join(out, "\n")
}

// phrases reads "phrase" or "phrase | boost" lines.
func phrases(v string) ([]*speechpb.PhraseSet_Phrase, error) {
	var out []*speechpb.PhraseSet_Phrase
	for _, l := range lines(v) {
		p := &speechpb.PhraseSet_Phrase{Value: l}
		if val, b, ok := strings.Cut(l, "|"); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(b), 32)
			if err != nil {
				return nil, fmt.Errorf("phrase %q: the boost after | is not a number", l)
			}
			p.Value, p.Boost = strings.TrimSpace(val), float32(f)
		}
		out = append(out, p)
	}
	return out, nil
}

func phrasesText(p *speechpb.PhraseSet) string {
	var out []string
	for _, ph := range p.GetPhrases() {
		if ph.GetBoost() != 0 {
			out = append(out, ph.GetValue()+" | "+strconv.FormatFloat(float64(ph.GetBoost()), 'g', -1, 32))
		} else {
			out = append(out, ph.GetValue())
		}
	}
	return strings.Join(out, "\n")
}

func boostText(b float32) string {
	if b == 0 {
		return "-"
	}
	return strconv.FormatFloat(float64(b), 'g', -1, 32)
}

const adaptNote = "Model adaptation is stored and served through the API; local whisper.cpp does not apply it, so a recognition " +
	"config that names custom classes or phrase sets is refused as not available locally."

// --- Custom classes ---------------------------------------------------------

// speechCustomClassesProvider is the Custom classes page.
type speechCustomClassesProvider struct{ s speechSvc }

func (speechCustomClassesProvider) ID() string    { return "speech-custom-classes" }
func (speechCustomClassesProvider) Title() string { return "Custom classes" }

func (p speechCustomClassesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Region", "Items", "Updated"}, NameColumn: "Custom class ID",
		Noun: "custom classes", AlwaysStatus: true, RowsOpenable: true,
		Note: "Custom classes in " + strings.Join(speechRegions, ", ") + ". A deleted class is listed until it expires, with Undelete. " + adaptNote}
	cs, err := p.s.customClasses(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	for _, c := range cs {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(c.GetName()), Status: stateWord(c.GetState().String()),
			Fields: map[string]string{"Display name": orDash(c.GetDisplayName()), "Region": locationOf(c.GetName()),
				"Items": strconv.Itoa(len(c.GetItems())), "Updated": stampAge(c.GetUpdateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func customClassFields(c *speechpb.CustomClass) []console.Field {
	return []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: c.GetDisplayName(), Pattern: "^.{0,63}$"},
		{Name: "items", Label: "Items", Type: "textarea", Required: true, Default: classItemsText(c), Help: "one item a line"},
	}
}

func (p speechCustomClassesProvider) CreateForm() (string, []console.Field) {
	return "Create custom class", append([]console.Field{speechAdaptIDField("Custom class ID"),
		{Name: "region", Label: "Region", Type: "select", Options: speechRegions, Default: "global"}},
		customClassFields(&speechpb.CustomClass{})...)
}

func (p speechCustomClassesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	id, region := strings.TrimSpace(v["name"]), strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	err := p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.CreateCustomClass(ctx, &speechpb.CreateCustomClassRequest{Parent: "projects/" + p.s.project(project) + "/locations/" + region,
			CustomClassId: id, CustomClass: &speechpb.CustomClass{DisplayName: strings.TrimSpace(v["displayName"]), Items: classItems(v["items"])}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

func (p speechCustomClassesProvider) Delete(ctx context.Context, project, id string) error {
	c, err := p.s.customClass(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(cl *speech.Client) error {
		op, err := cl.DeleteCustomClass(ctx, &speechpb.DeleteCustomClassRequest{Name: c.GetName(), Etag: c.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

func (p speechCustomClassesProvider) undelete(ctx context.Context, project, id string) error {
	c, err := p.s.customClass(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(cl *speech.Client) error {
		op, err := cl.UndeleteCustomClass(ctx, &speechpb.UndeleteCustomClassRequest{Name: c.GetName(), Etag: c.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// Actions implements console.Actor: a deleted class's Undelete.
func (p speechCustomClassesProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "Deleted" {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return nil
}

func (p speechCustomClassesProvider) Act(ctx context.Context, project, id, action string) error {
	if action != "undelete" {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.undelete(ctx, project, id)
}

func (p speechCustomClassesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.s.customClass(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	d := console.Detail{Summary: adaptSummary(c.GetName(), c.GetDisplayName(), c.GetState().String(), c.GetUid(), c.GetEtag(),
		c.GetCreateTime(), c.GetUpdateTime(), c.GetDeleteTime(), c.GetExpireTime())}
	items := console.Listing{Columns: []string{"Value"}, NameColumn: "Item", Noun: "items"}
	for i, it := range c.GetItems() {
		items.Items = append(items.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{"Value": it.GetValue()}})
	}
	items.Total = len(items.Items)
	d.Sections = []console.Section{{ID: "items", Label: "Items", Listing: items}}
	return d, nil
}

func (p speechCustomClassesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	c, err := p.s.customClass(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if c.GetState() == speechpb.CustomClass_DELETED {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return []console.Action{
		{ID: "edit", Label: "Edit", Submit: "Save", Fields: append(customClassFields(c), console.Field{Name: "etag", Type: "hidden", Default: c.GetEtag()})},
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: "the custom class: it can be undeleted until it expires"},
	}
}

func (p speechCustomClassesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	switch action {
	case "undelete":
		return p.undelete(ctx, project, path[0])
	case "delete":
		return p.Delete(ctx, project, path[0])
	case "edit":
		c, err := p.s.customClass(ctx, project, path[0])
		if err != nil {
			return err
		}
		return p.s.withClient(ctx, func(cl *speech.Client) error {
			op, err := cl.UpdateCustomClass(ctx, &speechpb.UpdateCustomClassRequest{CustomClass: &speechpb.CustomClass{Name: c.GetName(),
				Etag: v["etag"], DisplayName: strings.TrimSpace(v["displayName"]), Items: classItems(v["items"])},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "items"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

// adaptSummary is a custom class's or phrase set's summary.
func adaptSummary(name, display, state, uid, etag string, created, updated, deleted, expires *timestamppb.Timestamp) []console.Property {
	out := []console.Property{{Label: "Name", Value: name}, {Label: "Display name", Value: orDash(display)},
		{Label: "State", Value: stateWord(state)}, {Label: "Region", Value: locationOf(name)}, {Label: "UID", Value: uid},
		{Label: "Etag", Value: etag}}
	stamp := func(t *timestamppb.Timestamp) string {
		if t == nil || !t.IsValid() {
			return "-"
		}
		return t.AsTime().UTC().Format(time.RFC3339)
	}
	out = append(out, console.Property{Label: "Created", Value: stamp(created)}, console.Property{Label: "Updated", Value: stamp(updated)})
	if state == "DELETED" {
		out = append(out, console.Property{Label: "Deleted", Value: stamp(deleted)}, console.Property{Label: "Expires", Value: stamp(expires)})
	}
	return out
}

// --- Phrase sets ------------------------------------------------------------

// speechPhraseSetsProvider is the Phrase sets page.
type speechPhraseSetsProvider struct{ s speechSvc }

func (speechPhraseSetsProvider) ID() string    { return "speech-phrase-sets" }
func (speechPhraseSetsProvider) Title() string { return "Phrase sets" }

func (p speechPhraseSetsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Region", "Phrases", "Boost", "Updated"}, NameColumn: "Phrase set ID",
		Noun: "phrase sets", AlwaysStatus: true, RowsOpenable: true,
		Note: "Phrase sets in " + strings.Join(speechRegions, ", ") + ". A deleted set is listed until it expires, with Undelete. " + adaptNote}
	ps, err := p.s.phraseSets(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	for _, x := range ps {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(x.GetName()), Status: stateWord(x.GetState().String()),
			Fields: map[string]string{"Display name": orDash(x.GetDisplayName()), "Region": locationOf(x.GetName()),
				"Phrases": strconv.Itoa(len(x.GetPhrases())), "Boost": boostText(x.GetBoost()), "Updated": stampAge(x.GetUpdateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func phraseSetFields(x *speechpb.PhraseSet) []console.Field {
	boost := ""
	if x.GetBoost() != 0 {
		boost = boostText(x.GetBoost())
	}
	return []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: x.GetDisplayName(), Pattern: "^.{0,63}$"},
		{Name: "phrases", Label: "Phrases", Type: "textarea", Required: true, Default: phrasesText(x),
			Help: "one phrase a line; a line \"phrase | 10\" gives that phrase its own boost, 0 to 20"},
		{Name: "boost", Label: "Boost", Type: "text", Default: boost, Pattern: "^([0-9]+(\\.[0-9]+)?)?$",
			Help: "the set's boost, above 0 and at most 20; empty for none"},
	}
}

// phraseSetOf is the phrase set the form describes.
func phraseSetOf(v map[string]string) (*speechpb.PhraseSet, error) {
	ph, err := phrases(v["phrases"])
	if err != nil {
		return nil, err
	}
	x := &speechpb.PhraseSet{DisplayName: strings.TrimSpace(v["displayName"]), Phrases: ph}
	if b := strings.TrimSpace(v["boost"]); b != "" {
		f, err := strconv.ParseFloat(b, 32)
		if err != nil {
			return nil, fmt.Errorf("boost %q is not a number", b)
		}
		x.Boost = float32(f)
	}
	return x, nil
}

func (p speechPhraseSetsProvider) CreateForm() (string, []console.Field) {
	return "Create phrase set", append([]console.Field{speechAdaptIDField("Phrase set ID"),
		{Name: "region", Label: "Region", Type: "select", Options: speechRegions, Default: "global"}},
		phraseSetFields(&speechpb.PhraseSet{})...)
}

func (p speechPhraseSetsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	id, region := strings.TrimSpace(v["name"]), strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	x, err := phraseSetOf(v)
	if err != nil {
		return "", err
	}
	err = p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.CreatePhraseSet(ctx, &speechpb.CreatePhraseSetRequest{Parent: "projects/" + p.s.project(project) + "/locations/" + region,
			PhraseSetId: id, PhraseSet: x})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

func (p speechPhraseSetsProvider) Delete(ctx context.Context, project, id string) error {
	x, err := p.s.phraseSet(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.DeletePhraseSet(ctx, &speechpb.DeletePhraseSetRequest{Name: x.GetName(), Etag: x.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

func (p speechPhraseSetsProvider) undelete(ctx context.Context, project, id string) error {
	x, err := p.s.phraseSet(ctx, project, id)
	if err != nil {
		return err
	}
	return p.s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.UndeletePhraseSet(ctx, &speechpb.UndeletePhraseSetRequest{Name: x.GetName(), Etag: x.GetEtag()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// Actions implements console.Actor: a deleted set's Undelete.
func (p speechPhraseSetsProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "Deleted" {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return nil
}

func (p speechPhraseSetsProvider) Act(ctx context.Context, project, id, action string) error {
	if action != "undelete" {
		return fmt.Errorf("unknown action %q", action)
	}
	return p.undelete(ctx, project, id)
}

func (p speechPhraseSetsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	x, err := p.s.phraseSet(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	d := console.Detail{Summary: append(adaptSummary(x.GetName(), x.GetDisplayName(), x.GetState().String(), x.GetUid(), x.GetEtag(),
		x.GetCreateTime(), x.GetUpdateTime(), x.GetDeleteTime(), x.GetExpireTime()), console.Property{Label: "Boost", Value: boostText(x.GetBoost())})}
	l := console.Listing{Columns: []string{"Phrase", "Boost"}, NameColumn: "#", Noun: "phrases"}
	for i, ph := range x.GetPhrases() {
		l.Items = append(l.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{"Phrase": ph.GetValue(), "Boost": boostText(ph.GetBoost())}})
	}
	l.Total = len(l.Items)
	d.Sections = []console.Section{{ID: "phrases", Label: "Phrases", Listing: l}}
	return d, nil
}

func (p speechPhraseSetsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	x, err := p.s.phraseSet(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if x.GetState() == speechpb.PhraseSet_DELETED {
		return []console.Action{{ID: "undelete", Label: "Undelete"}}
	}
	return []console.Action{
		{ID: "edit", Label: "Edit", Submit: "Save", Fields: append(phraseSetFields(x), console.Field{Name: "etag", Type: "hidden", Default: x.GetEtag()})},
		{ID: "delete", Label: "Delete", Destructive: true, Confirm: "the phrase set: it can be undeleted until it expires"},
	}
}

func (p speechPhraseSetsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	switch action {
	case "undelete":
		return p.undelete(ctx, project, path[0])
	case "delete":
		return p.Delete(ctx, project, path[0])
	case "edit":
		cur, err := p.s.phraseSet(ctx, project, path[0])
		if err != nil {
			return err
		}
		x, err := phraseSetOf(v)
		if err != nil {
			return err
		}
		x.Name, x.Etag = cur.GetName(), v["etag"]
		return p.s.withClient(ctx, func(c *speech.Client) error {
			op, err := c.UpdatePhraseSet(ctx, &speechpb.UpdatePhraseSetRequest{PhraseSet: x,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "phrases", "boost"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

// --- Batch transcription ----------------------------------------------------

// batchRecognize sends one Cloud Storage file to BatchRecognize with a
// recognizer's default config, its transcript inline or written under
// output, and waits for it.
func (s speechSvc) batchRecognize(ctx context.Context, recognizer, uri, output string) (*speechpb.BatchRecognizeFileResult, error) {
	oc := &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_InlineResponseConfig{
		InlineResponseConfig: &speechpb.InlineOutputConfig{}}}
	if output != "" {
		oc = &speechpb.RecognitionOutputConfig{Output: &speechpb.RecognitionOutputConfig_GcsOutputConfig{
			GcsOutputConfig: &speechpb.GcsOutputConfig{Uri: output}}}
	}
	var res *speechpb.BatchRecognizeResponse
	err := s.withClient(ctx, func(c *speech.Client) error {
		op, err := c.BatchRecognize(ctx, &speechpb.BatchRecognizeRequest{Recognizer: recognizer, RecognitionOutputConfig: oc,
			Files: []*speechpb.BatchRecognizeFileMetadata{{AudioSource: &speechpb.BatchRecognizeFileMetadata_Uri{Uri: uri}}}})
		if err != nil {
			return err
		}
		res, err = op.Wait(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	fr := res.GetResults()[uri]
	if e := fr.GetError(); e != nil && e.GetCode() != 0 {
		return nil, fmt.Errorf("%s: %s", uri, e.GetMessage())
	}
	return fr, nil
}

// batchResult is a batch transcription's file result as the pages show it.
func batchResult(fr *speechpb.BatchRecognizeFileResult) *console.Listing {
	if tr := fr.GetInlineResult().GetTranscript(); tr != nil {
		return transcriptResult(&speechpb.RecognizeResponse{Results: tr.GetResults(), Metadata: tr.GetMetadata()})
	}
	out := &console.Listing{Columns: []string{"Output"}, NameColumn: "Result", Noun: "results"}
	if u := fr.GetCloudStorageResult().GetUri(); u != "" {
		out.Items = append(out.Items, console.Resource{Name: "1", Fields: map[string]string{"Output": u}})
		out.Note = "The transcript was written to " + u + ", a BatchRecognizeResults in JSON."
	}
	out.Total = len(out.Items)
	return out
}

// speechTranscriptionsProvider's batch transcription.
func (p speechTranscriptionsProvider) batch(ctx context.Context, project string, v map[string]string) (*console.Listing, error) {
	r, err := p.s.recognizer(ctx, project, strings.TrimSpace(v["recognizer"]))
	if err != nil {
		return nil, err
	}
	uri, output := strings.TrimSpace(v["uri"]), strings.TrimSpace(v["output"])
	fr, err := p.s.batchRecognize(ctx, r.GetName(), uri, output)
	if err != nil {
		return nil, err
	}
	out := batchResult(fr)
	p.held.mu.Lock()
	p.held.next++
	t := speechTranscription{id: "transcription-" + strconv.Itoa(p.held.next), project: p.s.project(project),
		recognizer: lastSegment(r.GetName()), file: uri, text: out.Text, when: time.Now().UTC(), result: out}
	if out.Text == "" && len(out.Items) > 0 {
		t.text = "Written to " + out.Items[0].Fields["Output"]
	}
	p.held.list = append(p.held.list, t)
	p.held.mu.Unlock()
	return out, nil
}

// ActAtResult implements console.ResultActor: the batch transcription.
func (p speechTranscriptionsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if len(path) != 0 || action != "batch" {
		return nil, p.ActAt(ctx, project, path, action, v)
	}
	return p.batch(ctx, project, v)
}

// MutationBudget implements console.MutationBudgeter: a batch
// transcription runs local whisper.cpp over up to 30 minutes of audio.
func (speechTranscriptionsProvider) MutationBudget() time.Duration { return 30 * time.Minute }

var (
	_ console.Creator     = speechCustomClassesProvider{}
	_ console.Deleter     = speechCustomClassesProvider{}
	_ console.Driller     = speechCustomClassesProvider{}
	_ console.Actor       = speechCustomClassesProvider{}
	_ console.PathActor   = speechCustomClassesProvider{}
	_ console.Creator     = speechPhraseSetsProvider{}
	_ console.Deleter     = speechPhraseSetsProvider{}
	_ console.Driller     = speechPhraseSetsProvider{}
	_ console.Actor       = speechPhraseSetsProvider{}
	_ console.PathActor   = speechPhraseSetsProvider{}
	_ console.ResultActor = speechTranscriptionsProvider{}
)
