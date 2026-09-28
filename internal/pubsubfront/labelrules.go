package pubsubfront

// Google's rules on labels (#962). Neither the emulator nor, before #962,
// the front checked a topic's or subscription's labels, on create or on
// update; Google does. Its Pub/Sub labels page
// (https://cloud.google.com/pubsub/docs/labels, "Requirements for labels")
// says:
//
//   - each resource can have up to 64 labels;
//   - keys have a minimum length of 1 character and a maximum length of 63
//     characters; values can be empty, and have a maximum length of 63
//     characters;
//   - keys and values can contain only lowercase letters, numeric
//     characters, underscores and dashes; all characters must use UTF-8
//     encoding, and international characters are allowed;
//   - keys must start with a lowercase letter or international character.
//
// "Lowercase letter" is read as Unicode category Ll, "international
// character" as a letter with no case (Lo: CJK, Arabic, Hebrew and so on),
// and "numeric character" as category N; lengths count characters, not
// bytes. An uppercase or titlecase letter, a space, a dot or any other
// character is refused. Uniqueness of keys holds by construction: labels
// are a map.
//
// A refusal is INVALID_ARGUMENT (HTTP 400 over REST), the code Google's
// Pub/Sub error-codes page gives for a request with an invalid argument
// (https://cloud.google.com/pubsub/docs/reference/error-codes). Google's
// message for a refused label is not measured here (no live call is made),
// so the message is the front's own, and names the rule that was broken.
//
// The front checks CreateTopic, CreateSubscription, and an UpdateTopic or
// UpdateSubscription whose mask names labels, over gRPC and REST alike.

import (
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// MaxLabels is the most labels a topic or subscription may have.
	MaxLabels = 64
	// MaxLabelLen is the most characters a label key or value may have.
	MaxLabelLen = 63
)

// LabelKeyPattern and LabelValuePattern are the rules above as regular
// expressions, valid in Go and in a browser's RegExp with the v flag, for
// the console's forms to check a label before it is sent. CheckLabels is the
// check the front makes; TestLabelPatternsAgreeWithCheckLabels holds the two
// to the same answers.
const (
	LabelKeyPattern   = `^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`
	LabelValuePattern = `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`
)

// CheckLabels refuses labels Google refuses, with INVALID_ARGUMENT naming
// the first broken rule. The console checks its forms with it too.
func CheckLabels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return status.Errorf(codes.InvalidArgument,
			"labels: a resource can have at most %d labels, got %d", MaxLabels, len(labels))
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the same label is reported first every time
	for _, k := range keys {
		if err := LabelKeyError(k); err != "" {
			return status.Errorf(codes.InvalidArgument, "labels: key %q %s", k, err)
		}
		if err := LabelValueError(labels[k]); err != "" {
			return status.Errorf(codes.InvalidArgument, "labels: value %q of key %q %s", labels[k], k, err)
		}
	}
	return nil
}

// LabelKeyError is why Google refuses k as a label key, or "" when it
// takes it.
func LabelKeyError(k string) string {
	if k == "" {
		return "is empty; a key must be 1 to 63 characters"
	}
	if msg := labelChars(k); msg != "" {
		return msg
	}
	if r, _ := utf8.DecodeRuneInString(k); !unicode.IsLower(r) && !unicode.Is(unicode.Lo, r) {
		return "must start with a lowercase letter or an international character"
	}
	return ""
}

// LabelValueError is why Google refuses v as a label value, or "" when it
// takes it.
func LabelValueError(v string) string {
	return labelChars(v)
}

// labelChars checks what keys and values share: the length and the
// characters.
func labelChars(s string) string {
	if !utf8.ValidString(s) {
		return "is not valid UTF-8"
	}
	if n := utf8.RuneCountInString(s); n > MaxLabelLen {
		return fmt.Sprintf("is %d characters; at most %d are allowed", n, MaxLabelLen)
	}
	for _, r := range s {
		if !labelRune(r) {
			return fmt.Sprintf("contains %q; only lowercase letters, international characters, digits, '_' and '-' are allowed", r)
		}
	}
	return ""
}

// labelRune reports whether r may appear in a label key or value.
func labelRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	case r < utf8.RuneSelf:
		return false
	}
	return unicode.IsLower(r) || unicode.Is(unicode.Lo, r) || unicode.IsNumber(r)
}
