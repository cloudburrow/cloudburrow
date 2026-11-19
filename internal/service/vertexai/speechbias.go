package vertexai

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/speech"
)

// A RecognitionConfig's adaptation (#1930), applied to local whisper.cpp:
// https://docs.cloud.google.com/speech-to-text/docs/adaptation-model and
// .../model-adaptation (read 2026-11-19) describe phrase sets (stored, by
// name, or inline), custom classes (stored or inline) referenced from a
// phrase as ${name}, and boost.
//
//   - Each phrase, with each ${class} reference expanded into one phrase per
//     item of the class, biases whisper's decoder: the logit of the token
//     that would start the phrase, or continue it where the text decoded
//     so far ends with a prefix of it, is raised by biasPerBoost times the
//     phrase's boost (its own, else its phrase set's). A phrase without a
//     boost gets defaultBoost, as Google applies a phrase set without one.
//   - Every phrase is also put in whisper's initial prompt (highest boost
//     last, as whisper keeps the prompt's last tokens), which conditions
//     the model on the spelling, as a hint.
//   - Limits (https://docs.cloud.google.com/speech-to-text/docs/quotas):
//     20 phrase sets and 20 custom classes per adaptation, 1,200 phrases a
//     set, 5,000 phrases and 100,000 characters a request, 100 characters a
//     phrase, 500 items a class and 500 characters an item, boost at most 20
//     and never negative.
//   - Google's prebuilt classes ($ADDRESSNUM, $OOV_CLASS_DIGIT_SEQUENCE,
//     ...) are UNIMPLEMENTED: whisper has no class grammar.
//
// UNVERIFIED: how strongly a boost biases (Google's is model-internal; the
// scale here was measured on whisper base, see speechbias_test.go), the
// default boost, and the texts of the errors.

const (
	biasPerBoost        = 1.0
	defaultBoost        = 5
	adaptMaxSets        = 20
	adaptMaxClasses     = 20
	adaptMaxSetPhrases  = 1200
	adaptMaxPhrases     = 5000
	adaptMaxPhraseChars = 100
	adaptMaxTotalChars  = 100000
	adaptMaxItems       = 500
	adaptMaxItemChars   = 500
	adaptPromptChars    = 600
)

var classRef = regexp.MustCompile(`\$\{([^}]+)\}|\$([A-Z][A-Z0-9_]*)`)

// adaptationOptions resolves a config's adaptation into whisper's phrases
// and prompt.
func (s *Server) adaptationOptions(raw json.RawMessage) ([]speech.Phrase, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	a := &speechpb.SpeechAdaptation{}
	if err := protojson.Unmarshal(raw, a); err != nil {
		return nil, "", apierror.InvalidArgument("Invalid RecognitionConfig.adaptation: %v", err)
	}
	if n := len(a.GetPhraseSets()); n > adaptMaxSets {
		return nil, "", apierror.InvalidArgument("adaptation has %d phrase sets: at most %d are allowed", n, adaptMaxSets)
	}
	if n := len(a.GetCustomClasses()); n > adaptMaxClasses {
		return nil, "", apierror.InvalidArgument("adaptation has %d custom classes: at most %d are allowed", n, adaptMaxClasses)
	}
	classes := map[string]*speechpb.CustomClass{}
	for i, c := range a.GetCustomClasses() {
		if c.GetName() == "" {
			return nil, "", apierror.InvalidArgument("adaptation.custom_classes[%d].name is required: phrases name the class as ${name}", i)
		}
		if err := checkCustomClass(c); err != nil {
			return nil, "", err
		}
		classes[c.GetName()] = c
	}
	type weighted struct {
		text  string
		boost float32
	}
	var phrases []weighted
	chars := 0
	s.mu.Lock()
	st := s.speechRecognizers()
	lookupClass := func(name string) (*speechpb.CustomClass, error) {
		if c, ok := classes[name]; ok {
			return c, nil
		}
		if c, ok := st.classes[name]; ok && c.GetState() != speechpb.CustomClass_DELETED {
			return c, nil
		}
		if !strings.Contains(name, "/customClasses/") && strings.ToUpper(name) == name {
			return nil, apierror.Unimplemented("Not implemented here: the prebuilt class $%s: CloudBurrow's local whisper.cpp has no class grammar, "+
				"and CloudBurrow never calls Google; name a custom class instead", name)
		}
		return nil, apierror.InvalidArgument("Unable to find CustomClass %s referenced by a phrase", name)
	}
	var sets []*speechpb.PhraseSet
	var err error
	for i, aps := range a.GetPhraseSets() {
		switch {
		case aps.GetInlinePhraseSet() != nil:
			sets = append(sets, aps.GetInlinePhraseSet())
		case aps.GetPhraseSet() != "":
			ps, ok := st.phraseSets[aps.GetPhraseSet()]
			if !ok || ps.GetState() == speechpb.PhraseSet_DELETED {
				err = apierror.InvalidArgument("Unable to find PhraseSet %s from storage", aps.GetPhraseSet())
			}
			sets = append(sets, ps)
		default:
			err = apierror.InvalidArgument("adaptation.phrase_sets[%d] names no phrase set: set phrase_set or inline_phrase_set", i)
		}
		if err != nil {
			break
		}
	}
	if err == nil {
	sets:
		for i, ps := range sets {
			if err = checkPhraseSet(ps); err != nil {
				break
			}
			if n := len(ps.GetPhrases()); n > adaptMaxSetPhrases {
				err = apierror.InvalidArgument("phrase_sets[%d] has %d phrases: at most %d are allowed", i, n, adaptMaxSetPhrases)
				break
			}
			for _, ph := range ps.GetPhrases() {
				v := strings.TrimSpace(ph.GetValue())
				if utf8.RuneCountInString(v) > adaptMaxPhraseChars {
					err = apierror.InvalidArgument("Phrase %q is longer than %d characters", v, adaptMaxPhraseChars)
					break sets
				}
				boost := ph.GetBoost()
				if boost == 0 {
					boost = ps.GetBoost()
				}
				if boost == 0 {
					boost = defaultBoost
				}
				var texts []string
				if texts, err = expandPhrase(v, lookupClass); err != nil {
					break sets
				}
				for _, t := range texts {
					chars += utf8.RuneCountInString(t)
					phrases = append(phrases, weighted{t, boost})
				}
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	if len(phrases) > adaptMaxPhrases {
		return nil, "", apierror.InvalidArgument("adaptation has %d phrases: at most %d are allowed in a request", len(phrases), adaptMaxPhrases)
	}
	if chars > adaptMaxTotalChars {
		return nil, "", apierror.InvalidArgument("adaptation's phrases total %d characters: at most %d are allowed in a request", chars, adaptMaxTotalChars)
	}
	sort.SliceStable(phrases, func(i, j int) bool { return phrases[i].boost < phrases[j].boost })
	out := make([]speech.Phrase, 0, len(phrases))
	var prompt []string
	n := 0
	for i := len(phrases) - 1; i >= 0; i-- {
		p := phrases[i]
		out = append(out, speech.Phrase{Text: p.text, Bias: biasPerBoost * float64(p.boost)})
		if n+len(p.text)+2 <= adaptPromptChars {
			prompt = append([]string{p.text}, prompt...)
			n += len(p.text) + 2
		}
	}
	return out, strings.Join(prompt, ", "), nil
}

// expandPhrase is a phrase with each class reference replaced by each of
// the class's items.
func expandPhrase(v string, lookup func(string) (*speechpb.CustomClass, error)) ([]string, error) {
	loc := classRef.FindStringSubmatchIndex(v)
	if loc == nil {
		return []string{v}, nil
	}
	var name string
	if loc[2] >= 0 {
		name = v[loc[2]:loc[3]]
	} else {
		name = v[loc[4]:loc[5]]
	}
	c, err := lookup(name)
	if err != nil {
		return nil, err
	}
	if n := len(c.GetItems()); n > adaptMaxItems {
		return nil, apierror.InvalidArgument("CustomClass %s has %d items: at most %d are allowed", name, n, adaptMaxItems)
	}
	var out []string
	for _, it := range c.GetItems() {
		if utf8.RuneCountInString(it.GetValue()) > adaptMaxItemChars {
			return nil, apierror.InvalidArgument("An item of CustomClass %s is longer than %d characters", name, adaptMaxItemChars)
		}
		rest, err := expandPhrase(v[loc[1]:], lookup)
		if err != nil {
			return nil, err
		}
		for _, r := range rest {
			out = append(out, strings.TrimSpace(v[:loc[0]]+it.GetValue()+r))
		}
		if len(out) > adaptMaxPhrases {
			return nil, apierror.InvalidArgument("phrase %q expands to more than %d phrases", v, adaptMaxPhrases)
		}
	}
	return out, nil
}
