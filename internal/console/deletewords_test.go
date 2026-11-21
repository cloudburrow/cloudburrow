package console

import (
	"encoding/json"
	"testing"
)

// wordingProvider names its delete's header button and its prompt's
// button, as Cloud Tasks' Delete queue and Confirm (#1848).
type wordingProvider struct{ mutableProvider }

func (wordingProvider) DeleteWords() DeleteWords {
	return DeleteWords{Selected: "Delete queue", Submit: "Confirm"}
}

// A DeleteWorder's words reach the client as deleteSelected and
// deleteSubmit; a provider that names none sends neither, so its list
// keeps "Delete".
func TestDeleteWordsReachTheClient(t *testing.T) {
	srv := serve(t,
		&wordingProvider{mutableProvider{fakeProvider: fakeProvider{id: "worded", title: "Worded"}}},
		&mutableProvider{fakeProvider: fakeProvider{id: "plain", title: "Plain"}},
	)
	_, body := get(t, srv, "/api/services", nil)
	var got struct {
		Services []struct {
			ID             string `json:"id"`
			DeleteSelected string `json:"deleteSelected"`
			DeleteSubmit   string `json:"deleteSubmit"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	seen := 0
	for _, s := range got.Services {
		switch s.ID {
		case "worded":
			seen++
			if s.DeleteSelected != "Delete queue" || s.DeleteSubmit != "Confirm" {
				t.Errorf("worded: %+v; want Delete queue and Confirm", s)
			}
		case "plain":
			seen++
			if s.DeleteSelected != "" || s.DeleteSubmit != "" {
				t.Errorf("plain: %+v; want no words", s)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 providers: %s", seen, body)
	}
}
