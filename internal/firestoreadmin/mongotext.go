package firestoreadmin

// Firestore with MongoDB compatibility (#1738): text indexes and $text,
// as Google documents them ("Create a text index", "Use text searches";
// mql/text.go has the matching). A text index is a composite index of the
// Admin API with the MONGODB_COMPATIBLE_API scope whose fields have a
// search config (a tokenized, match-globally text spec), as `gcloud
// firestore indexes composite create --field-config=...search-config=...`
// makes it, with its default language and language override in
// search_index_options; a partition field (firestoreOptions'
// customPartitionFields) is an ascending field before them. One text index
// a collection, as Google allows.

import (
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

// textLanguages are the languages Google lists for a text index, by code
// and by name ("Expand for a list of supported languages").
var textLanguages = func() map[string]string {
	pairs := []string{"und", "Autodetect", "af", "Afrikaans", "ak", "Akan", "sq", "Albanian", "am", "Amharic", "ar", "Arabic",
		"hy", "Armenian", "az", "Azerbaijani", "eu", "Basque", "be", "Belarusian", "bn", "Bangla", "bs", "Bosnian", "bg", "Bulgarian",
		"my", "Burmese", "ca", "Catalan", "ceb", "Cebuano", "chr", "Cherokee", "zh", "Chinese", "zh-Hant", "Chinese_Traditional",
		"hr", "Croatian", "cs", "Czech", "da", "Danish", "nl", "Dutch", "en", "English", "eo", "Esperanto", "et", "Estonian",
		"fil", "Filipino", "fi", "Finnish", "fr", "French", "gl", "Galician", "ka", "Georgian", "de", "German", "el", "Greek",
		"gu", "Gujarati", "ht", "Haitian_Creole", "ha", "Hausa", "haw", "Hawaiian", "iw", "Hebrew", "hi", "Hindi", "hmn", "Hmong",
		"hu", "Hungarian", "is", "Icelandic", "ig", "Igbo", "id", "Indonesian", "ga", "Irish", "it", "Italian", "ja", "Japanese",
		"jv", "Javanese", "kn", "Kannada", "kk", "Kazakh", "km", "Khmer", "ko", "Korean", "lo", "Lao", "la", "Latin", "lv", "Latvian",
		"lt", "Lithuanian", "lb", "Luxembourgish", "mk", "Macedonian", "mg", "Malagasy", "ms", "Malay", "ml", "Malayalam",
		"mt", "Maltese", "mi", "Maori", "mr", "Marathi", "mfe", "Morisyen", "mn", "Mongolian", "sr-ME", "Serbian_Montenegro",
		"ne", "Nepali", "no", "Norwegian", "ny", "Nyanja", "or", "Odia", "fa", "Persian", "pl", "Polish", "pt-BR", "Portuguese_Brazil",
		"pt-PT", "Portuguese_Portugal", "pa", "Punjabi", "ro", "Romanian", "ru", "Russian", "gd", "Scottish Gaelic", "sr", "Serbian",
		"st", "Southern Sotho", "si", "Sinhala", "sk", "Slovak", "sl", "Slovenian", "so", "Somali", "es", "Spanish", "su", "Sundanese",
		"sw", "Swahili", "sv", "Swedish", "tg", "Tajik", "ta", "Tamil", "te", "Telugu", "th", "Thai", "tr", "Turkish", "uk", "Ukrainian",
		"ur", "Urdu", "uz", "Uzbek", "vi", "Vietnamese", "cy", "Welsh", "yi", "Yiddish", "yo", "Yoruba", "zu", "Zulu"}
	m := map[string]string{}
	for i := 0; i < len(pairs); i += 2 {
		m[strings.ToLower(pairs[i])] = pairs[i]
		m[strings.ToLower(pairs[i+1])] = pairs[i]
	}
	return m
}()

// textLanguageCode is lang's code, and whether Google lists it.
func textLanguageCode(lang string) (string, bool) {
	c, ok := textLanguages[strings.ToLower(lang)]
	return c, ok
}

// isTextIndex is whether x is a text index.
func isTextIndex(x *adminpb.Index) bool {
	for _, f := range x.GetFields() {
		if f.GetSearchConfig().GetTextSpec() != nil {
			return true
		}
	}
	return false
}

// textIndexSpec is a text index of a createIndexes spec, nil when keys
// name no text field.
func textIndexSpec(keys bson.D, spec bson.D) (*adminpb.Index, error) {
	text := 0
	for _, k := range keys {
		if k.Value == "text" {
			text++
		}
	}
	if text == 0 {
		return nil, nil
	}
	if text != len(keys) {
		return nil, mql.Errorf(67, "CannotCreateIndex", "A text index's keys must all be \"text\".")
	}
	in := &adminpb.Index{QueryScope: adminpb.Index_COLLECTION_GROUP, ApiScope: adminpb.Index_MONGODB_COMPATIBLE_API,
		State: adminpb.Index_READY, Density: adminpb.Index_DENSE, SearchIndexOptions: &adminpb.Index_SearchIndexOptions{}}
	if mql.Truthy(mql.Lookup(spec, "unique")) {
		return nil, mql.Errorf(67, "CannotCreateIndex", "A text index cannot be unique.")
	}
	if v, ok := mql.LookupE(spec, "default_language"); ok {
		s, _ := v.(string)
		switch {
		case s == "\x00" || s == "none":
			// No language: no field overrides it.
			in.SearchIndexOptions.TextLanguage = "none"
		default:
			code, ok := textLanguageCode(s)
			if !ok {
				return nil, mql.Errorf(17262, "Location17262", "default_language: unknown language %v", v)
			}
			in.SearchIndexOptions.TextLanguage = code
		}
	}
	if v, ok := mql.LookupE(spec, "language_override"); ok {
		s, _ := v.(string)
		if s == "" || strings.Contains(s, ".") {
			return nil, mql.Errorf(67, "CannotCreateIndex", "language_override must be a top-level field, not %v", v)
		}
		in.SearchIndexOptions.TextLanguageOverrideFieldPath = s
	}
	if fo, ok := mql.Lookup(spec, "firestoreOptions").(bson.D); ok {
		for _, e := range fo {
			if e.Key != "customPartitionFields" {
				return nil, mql.Errorf(67, "CannotCreateIndex", "Unknown firestoreOptions field %s.", e.Key)
			}
			parts, _ := e.Value.(bson.A)
			if len(parts) != 1 {
				return nil, mql.Errorf(67, "CannotCreateIndex", "The partition must be only one field.")
			}
			p, _ := parts[0].(string)
			if p == "" || strings.Contains(p, ".") {
				return nil, mql.Errorf(67, "CannotCreateIndex", "The partition field must be a top-level field, not %v", parts[0])
			}
			in.Fields = append(in.Fields, &adminpb.Index_IndexField{FieldPath: p,
				ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}})
		}
	}
	for _, k := range keys {
		in.Fields = append(in.Fields, &adminpb.Index_IndexField{FieldPath: k.Key, ValueMode: &adminpb.Index_IndexField_SearchConfig_{
			SearchConfig: &adminpb.Index_IndexField_SearchConfig{TextSpec: &adminpb.Index_IndexField_SearchConfig_SearchTextSpec{
				IndexSpecs: []*adminpb.Index_IndexField_SearchConfig_SearchTextIndexSpec{{
					IndexType: adminpb.Index_IndexField_SearchConfig_TOKENIZED,
					MatchType: adminpb.Index_IndexField_SearchConfig_MATCH_GLOBALLY}}}}}})
	}
	return in, nil
}

// textOf is the mql text index of x.
func textOf(x *adminpb.Index) *mql.TextIndex {
	ti := &mql.TextIndex{Language: x.GetSearchIndexOptions().GetTextLanguage(),
		LanguageOverride: x.GetSearchIndexOptions().GetTextLanguageOverrideFieldPath()}
	if ti.Language == "none" {
		ti.LanguageOverride = "\x00"
	}
	for _, f := range x.GetFields() {
		switch {
		case f.GetSearchConfig().GetTextSpec() != nil:
			ti.Fields = append(ti.Fields, f.GetFieldPath())
		case f.GetFieldPath() != "__name__" && f.GetOrder() != adminpb.Index_IndexField_ORDER_UNSPECIFIED:
			ti.Partition = f.GetFieldPath()
		}
	}
	return ti
}

// textIndex is coll's text index, nil for none.
func (s *MongoServer) textIndex(mc *mongoConn, coll string) *mql.TextIndex {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	for _, x := range s.a.mongoIndexes(mc.db.name, coll) {
		if isTextIndex(x) {
			return textOf(x)
		}
	}
	// An index the Admin API made with any API scope searches too.
	prefix := mc.db.name + "/collectionGroups/" + coll + "/indexes/"
	for n, x := range s.a.indexes {
		if strings.HasPrefix(n, prefix) && isTextIndex(x) {
			return textOf(x)
		}
	}
	return nil
}

// textMatcher is how filter's $text matches a document of coll, nil when
// it has none: the collection's text index must exist, and a partitioned
// one needs its partition field's equality.
func (s *MongoServer) textMatcher(mc *mongoConn, coll string, filter bson.D, sortV any) (func(bson.D) (bool, float64), error) {
	spec, ok, err := mql.FindText(filter)
	if err != nil || !ok {
		return nil, err
	}
	if sd, ok := sortV.(bson.D); ok {
		for _, e := range sd {
			if e.Key == "$natural" {
				return nil, mql.BadValue("$natural sort cannot be set with a $text query.")
			}
		}
	}
	q, err := mql.ParseText(spec)
	if err != nil {
		return nil, err
	}
	ti := s.textIndex(mc, coll)
	if ti == nil {
		return nil, mql.Errorf(27, "IndexNotFound", "text index required for $text query")
	}
	if ti.Partition != "" {
		if _, ok := mql.PartitionValue(filter, ti.Partition); !ok {
			return nil, mql.BadValue("The text index of %s is partitioned by %s: a $text query must filter on %s's value, a string, joined by $and.", coll, ti.Partition, ti.Partition)
		}
	}
	return func(d bson.D) (bool, float64) { return q.Match(d, ti) }, nil
}

// textStage is an aggregation's input after its first stage, a $match with
// a $text, and the rest of its pipeline; docs and pipeline as they are
// when it has none. A $text in any later stage is refused.
func (s *MongoServer) textStage(mc *mongoConn, coll string, docs []bson.D, pipeline bson.A) ([]bson.D, bson.A, error) {
	for i, st := range pipeline {
		sd, ok := st.(bson.D)
		if !ok || len(sd) != 1 || sd[0].Key != "$match" {
			continue
		}
		f, _ := sd[0].Value.(bson.D)
		_, isText, err := mql.FindText(f)
		if err != nil {
			return nil, nil, err
		}
		if !isText {
			continue
		}
		if i != 0 {
			return nil, nil, mql.Errorf(17313, "Location17313", "$match with $text is only allowed as the first pipeline stage")
		}
		text, err := s.textMatcher(mc, coll, f, nil)
		if err != nil {
			return nil, nil, err
		}
		var out []bson.D
		for _, d := range docs {
			tm, score := text(d)
			m, err := mql.Matches(d, mql.ReplaceText(f, tm))
			if err != nil {
				return nil, nil, err
			}
			if m {
				out = append(out, append(append(bson.D{}, d...), bson.E{Key: mql.TextScoreKey, Value: score}))
			}
		}
		docs, pipeline = out, pipeline[1:]
	}
	return docs, pipeline, nil
}
