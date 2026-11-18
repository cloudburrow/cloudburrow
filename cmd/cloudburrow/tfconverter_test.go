package main

import (
	"reflect"
	"testing"
)

// prefetch --tensorflow-converter is taken out of the flags config.Load
// reads, in each of the forms the flag package accepts.
func TestPrefetchTakesTheTensorFlowConverterFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		rest []string
		set  bool
	}{
		{[]string{"--name", "x"}, []string{"--name", "x"}, false},
		{[]string{"--tensorflow-converter", "--name", "x"}, []string{"--name", "x"}, true},
		{[]string{"-tensorflow-converter=true"}, nil, true},
		{[]string{"--tensorflow-converter=false", "--offline"}, []string{"--offline"}, false},
		{[]string{"--", "--tensorflow-converter"}, []string{"--", "--tensorflow-converter"}, false},
	} {
		rest, set := takeFlag(tc.args, "tensorflow-converter")
		if set != tc.set || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("takeFlag(%q) = %q, %v; want %q, %v", tc.args, rest, set, tc.rest, tc.set)
		}
	}
}
