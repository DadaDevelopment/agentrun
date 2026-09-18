package cliapp

import (
	"reflect"
	"testing"
)

// TestExtraEvalArgs covers the shape CI actually uses: a YAML folded block of
// flags, some carrying quoted values with spaces.
func TestExtraEvalArgs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"blank", "   \n  ", nil},
		{"plain", "--environment ci --fail-below 0.1",
			[]string{"--environment", "ci", "--fail-below", "0.1"}},
		{"quoted value", `--label "nightly run" --environment ci`,
			[]string{"--label", "nightly run", "--environment", "ci"}},
		{"single quotes", `--run-name 'ci-42'`, []string{"--run-name", "ci-42"}},
		{"folded whitespace", "--environment ci\n  --label x\t--max-concurrency 3",
			[]string{"--environment", "ci", "--label", "x", "--max-concurrency", "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DDC_EVAL_ARGS", tc.raw)
			got := extraEvalArgs()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("extraEvalArgs() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
