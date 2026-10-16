package bigqueryfront

import "testing"

func TestTableReads(t *testing.T) {
	reads, _, ok := tableReads("SELECT n FROM `ds.t$20240101` AS x FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()", "p", "")
	if !ok || len(reads) != 1 || reads[0].dataset != "ds" || reads[0].table != "t" || reads[0].decorator != "20240101" {
		t.Fatalf("%+v %v", reads, ok)
	}
}

func TestPartitionFilter(t *testing.T) {
	day := partitionMeta{Schema: tableSchema{Fields: []field{{Name: "d", Type: "DATE"}}}}
	day.TimePartitioning = &struct {
		Type  string `json:"type"`
		Field string `json:"field"`
	}{"DAY", "d"}
	if got, code, _ := partitionFilter(day, "20240101", "x"); code != 0 ||
		got != "(TIMESTAMP(`d`, 'UTC') >= TIMESTAMP '2024-01-01 00:00:00+00' AND TIMESTAMP(`d`, 'UTC') < TIMESTAMP '2024-01-02 00:00:00+00')" {
		t.Errorf("day: %q %d", got, code)
	}
	if _, code, _ := partitionFilter(day, "2024", "x"); code != 400 {
		t.Errorf("bad decorator: %d", code)
	}
	if _, code, msg := partitionFilter(partitionMeta{}, "20240101", "p:d.t$20240101"); code != 400 || msg == "" {
		t.Errorf("unpartitioned: %d %q", code, msg)
	}
	if end := systemTimeEnd(mustLex(t, "x TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR) WHERE a"), 1); end != 11 {
		t.Errorf("systemTimeEnd %d", end)
	}
	if got := lineCol("SELECT\n  _P", 9); got != "[2:3]" {
		t.Errorf("lineCol %s", got)
	}
}

func mustLex(t *testing.T, s string) []token {
	toks, ok := lex(s)
	if !ok {
		t.Fatal("lex")
	}
	return toks
}

func TestDecorated(t *testing.T) {
	for _, tc := range []struct {
		id, table, dec string
		ok             bool
	}{
		{"t$20240101", "t", "20240101", true},
		{"t$__NULL__", "t", "__NULL__", true},
		{"t", "t", "", false},
		{"$20240101", "$20240101", "", false},
	} {
		table, dec, ok := decorated(tc.id)
		if table != tc.table || dec != tc.dec || ok != tc.ok {
			t.Errorf("decorated(%q) = %q, %q, %v", tc.id, table, dec, ok)
		}
	}
}
