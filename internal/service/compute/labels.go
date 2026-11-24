package compute

import (
	"unicode"
	"unicode/utf8"
)

// Labels (https://docs.cloud.google.com/compute/docs/labeling-resources,
// "Requirements for labels", read 2026-11-22): at most 64 labels a
// resource; a key 1 to 63 characters, a value 0 to 63, of lowercase
// letters, international characters, digits, underscores and dashes, a key
// starting with a lowercase letter or an international character. Every
// setLabels (and labels given at insert) is checked against them, under the
// resource's labelFingerprint (addresses.go's setLabels).

const maxLabels = 64

// labelChar is whether r may be in a label key or value.
func labelChar(r rune) bool {
	return r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r > unicode.MaxASCII && !unicode.IsUpper(r))
}

// checkLabels validates labels as Google does.
func checkLabels(field string, labels map[string]any) error {
	if len(labels) > maxLabels {
		return invalid("Invalid value for field '%s': Resource has too many labels: at most %d labels are allowed, got %d.", field, maxLabels, len(labels))
	}
	for k, v := range labels {
		if n := utf8.RuneCountInString(k); n == 0 || n > 63 {
			return invalid("Invalid value for field '%s': '%s'. Label key '%s' violates format constraints. The key can be at most 63 characters long and cannot be empty.", field, k, k)
		}
		first, _ := utf8.DecodeRuneInString(k)
		if !(first >= 'a' && first <= 'z') && !(first > unicode.MaxASCII && unicode.IsLetter(first) && !unicode.IsUpper(first)) {
			return invalid("Invalid value for field '%s': '%s'. Label key '%s' violates format constraints. The key must start with a lowercase character, can only contain lowercase letters, numeric characters, underscores and dashes. The key can be at most 63 characters long. International characters are allowed.", field, k, k)
		}
		for _, r := range k {
			if !labelChar(r) {
				return invalid("Invalid value for field '%s': '%s'. Label key '%s' violates format constraints. The key must start with a lowercase character, can only contain lowercase letters, numeric characters, underscores and dashes. The key can be at most 63 characters long. International characters are allowed.", field, k, k)
			}
		}
		s, ok := v.(string)
		if !ok {
			return invalid("Invalid value for field '%s.%s': a label value is a string.", field, k)
		}
		if utf8.RuneCountInString(s) > 63 {
			return invalid("Invalid value for field '%s': '%s'. Label value '%s' violates format constraints. The value can be at most 63 characters long.", field, s, s)
		}
		for _, r := range s {
			if !labelChar(r) {
				return invalid("Invalid value for field '%s': '%s'. Label value '%s' violates format constraints. The value can only contain lowercase letters, numeric characters, underscores and dashes. The value can be at most 63 characters long. International characters are allowed.", field, s, s)
			}
		}
	}
	return nil
}
