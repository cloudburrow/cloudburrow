package bigqueryfront

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The naming rules below are BigQuery's documented ones. The emulator
// checks none of them (measured against the pinned image, #861): it
// created a dataset "d-1" and a table "t!".

// maxIDLength is the longest dataset ID in characters, and the longest table
// ID in UTF-8 bytes.
const maxIDLength = 1024

// datasetIDPattern: a dataset name can contain "Up to 1,024 characters" and
// "Letters (uppercase or lowercase), numbers, and underscores"; no spaces,
// hyphens or other special characters.
// https://cloud.google.com/bigquery/docs/datasets
//
// The length is checked apart, because Go's regexp refuses a repeat count
// over 1,000.
var datasetIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// tableIDPattern: a table name must "Contain characters with a total of up
// to 1,024 UTF-8 bytes" and "Contain Unicode characters in category L
// (letter), M (mark), N (number), Pc (connector, including underscore), Pd
// (dash), Zs (space)."
// https://cloud.google.com/bigquery/docs/tables
var tableIDPattern = regexp.MustCompile(`^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`)

// maxColumnLength is the longest column name, in characters: "The maximum
// column name length is 300 characters."
// https://cloud.google.com/bigquery/docs/schemas
const maxColumnLength = 300

// columnNamePattern is BigQuery's flexible column name rule, the one a table
// created through the API is held to: "any letter in any language" (\p{L}),
// "any numeric character in any language" (\p{N}), "any connector
// punctuation character, including underscores" (\p{Pc}), "a hyphen or
// dash" (\p{Pd}), "any mark intended to accompany another character"
// (\p{M}), and the special characters & % = + : ' < > # | and whitespace.
// Everything else is refused, notably ! " $ ( ) * , . / ; ? @ [ \ ] ^ ` { } ~.
// https://cloud.google.com/bigquery/docs/schemas
//
// It is wider than the classic rule (letters, digits and underscores,
// starting with a letter or underscore), which BigQuery still documents: a
// space in a column name is accepted by BigQuery today, so it is accepted
// here, and the emulator was measured to store and query such a column.
var columnNamePattern = regexp.MustCompile(`^[\p{L}\p{N}\p{Pc}\p{Pd}\p{M}&%=+:'<>#|\s]+$`)

// reservedColumnPrefixes: "Column names cannot use any of the following
// prefixes". Column names are case-insensitive, so the prefixes are
// matched without case.
// https://cloud.google.com/bigquery/docs/schemas
var reservedColumnPrefixes = []string{
	"_TABLE_", "_FILE_", "_PARTITION", "_ROW_TIMESTAMP", "__ROOT__", "_COLIDENTIFIER",
	"_CHANGE_SEQUENCE_NUMBER", "_CHANGE_TYPE", "_CHANGE_TIMESTAMP",
}

// checkDatasetID returns why id is not a valid dataset ID, or "".
func checkDatasetID(id string) string {
	if id == "" {
		return "Dataset ID is required"
	}
	if utf8.RuneCountInString(id) > maxIDLength || !datasetIDPattern.MatchString(id) {
		return fmt.Sprintf("Invalid dataset ID %q. Dataset IDs must be alphanumeric (plus underscores) and must be at most %d characters long.", id, maxIDLength)
	}
	return ""
}

// checkTableID returns why id is not a valid table ID, or "".
func checkTableID(id string) string {
	if id == "" {
		return "Table ID is required"
	}
	if len(id) > maxIDLength || !utf8.ValidString(id) || !tableIDPattern.MatchString(id) {
		return fmt.Sprintf("Invalid table ID %q. Table IDs must be Unicode letters, marks, numbers, connectors, dashes or spaces, at most %d UTF-8 bytes long.", id, maxIDLength)
	}
	return ""
}

// checkColumnName returns why name is not a valid column name, or "".
func checkColumnName(name string) string {
	if name == "" {
		return "Empty field name"
	}
	if utf8.RuneCountInString(name) > maxColumnLength {
		return fmt.Sprintf("Invalid field name %q. Fields must be at most %d characters long.", name, maxColumnLength)
	}
	if !utf8.ValidString(name) || !columnNamePattern.MatchString(name) {
		return fmt.Sprintf("Invalid field name %q. Fields may contain letters, numbers, marks, underscores, dashes, spaces and & %% = + : ' < > # |.", name)
	}
	upper := strings.ToUpper(name)
	for _, p := range reservedColumnPrefixes {
		if strings.HasPrefix(upper, p) {
			return fmt.Sprintf("Invalid field name %q. Field names cannot begin with the reserved prefix %s.", name, p)
		}
	}
	return ""
}
