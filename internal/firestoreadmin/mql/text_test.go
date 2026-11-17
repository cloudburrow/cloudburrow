package mql

import "testing"

func TestTextSearch(t *testing.T) {
	ix := &TextIndex{Fields: []string{"name", "food"}}
	docs := []string{
		`{"_id": 1, "name": "Paris", "food": "the best french bread is here"}`,
		`{"_id": 2, "name": "Lyon", "food": "french cheeses"}`,
		`{"_id": 3, "name": "Rome", "food": "best pizza and breads"}`,
		`{"_id": 4, "name": "Café", "food": ["crêpes", "coffee"]}`,
	}
	for _, tc := range []struct {
		search string
		want   []int
	}{
		{"french bread", []int{1, 2, 3}},
		{`"best french bread"`, []int{1}},
		{`"best" "french" bread is`, []int{1}},
		{"best bread -french", []int{3}},
		{"cheese", []int{2}},
		{"cafe crepe", []int{4}},
		{"nothing", nil},
	} {
		q, err := ParseText(doc(t, `{"$search": `+quote(tc.search)+`}`))
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for i, d := range docs {
			if m, s := q.Match(doc(t, d), ix); m {
				if s <= 0 {
					t.Errorf("%q: document %d matched with score %v", tc.search, i+1, s)
				}
				got = append(got, i+1)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q matched %v, want %v", tc.search, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q matched %v, want %v", tc.search, got, tc.want)
			}
		}
	}
	if _, _, err := FindText(doc(t, `{"$text": {"$search": "a"}, "$or": [{"$text": {"$search": "b"}}]}`)); err == nil {
		t.Error("two $text were accepted")
	}
	if _, _, err := FindText(doc(t, `{"$nor": [{"$text": {"$search": "b"}}]}`)); err == nil {
		t.Error("$text in $nor was accepted")
	}
}

func quote(s string) string {
	out := `"`
	for _, r := range s {
		if r == '"' {
			out += `\"`
		} else {
			out += string(r)
		}
	}
	return out + `"`
}
