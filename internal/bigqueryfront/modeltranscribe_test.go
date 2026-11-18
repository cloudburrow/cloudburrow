package bigqueryfront

import (
	"encoding/json"
	"testing"
)

func TestRecognitionConfigText(t *testing.T) {
	for in, want := range map[string]string{
		`( JSON '{"model": "chirp"}')`: `{"model": "chirp"}`,
		`JSON '{"a": 1}'`:              `{"a": 1}`,
		`'{"b": 2}'`:                   `{"b": 2}`,
		`((JSON '{"c": 3}'))`:          `{"c": 3}`,
	} {
		if got, ok := recognitionConfigText(in); !ok || got != want {
			t.Errorf("%s: %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{`JSON`, `1`, `(JSON 'a', 'b')`} {
		if _, ok := recognitionConfigText(bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestSnakeResult: ml_transcribe_result as the page's sample shows it.
func TestSnakeResult(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"metadata":{"totalBilledDuration":"56s"},"results":[{"alternatives":[{"transcript":"hi","confidence":0.7,`+
		`"words":[{"word":"hi","startOffset":"0.320s","endOffset":"1.500s"}]}],"resultEndOffset":"2s","languageCode":"en-us"}]}`), &v)
	b, _ := json.Marshal(snakeResult(v, ""))
	want := `{"metadata":{"total_billed_duration":{"seconds":56}},"results":[{"alternatives":[{"confidence":0.7,"transcript":"hi",` +
		`"words":[{"end_offset":{"nanos":500000000,"seconds":1},"start_offset":{"nanos":320000000,"seconds":0},"word":"hi"}]}],` +
		`"language_code":"en-us","result_end_offset":{"seconds":2}}]}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}
