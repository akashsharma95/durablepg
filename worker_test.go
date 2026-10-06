package durablepg

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestResolveWaitKeyUsesCurrentRunAndCompletedValues(t *testing.T) {
	wait := &waitEventOp{keyFn: func(sc *StepContext) (string, error) {
		var input struct {
			Order string `json:"order"`
		}
		if err := sc.DecodeInput(&input); err != nil {
			return "", err
		}
		var previous struct {
			Region string `json:"region"`
		}
		if _, err := sc.Value("lookup", &previous); err != nil {
			return "", err
		}
		// Mutating a publicly returned value must not change later steps.
		raw, _ := sc.RawValue("lookup")
		raw[0] = '!'
		return string(sc.RunID) + ":" + sc.Workflow + ":" + input.Order + ":" + previous.Region, nil
	}}
	wf := &compiledWorkflow{names: map[string]int{"lookup": 0, "wait": 1}}
	e := &Engine{}
	for _, tc := range []struct {
		run    claimedRun
		region string
		want   string
	}{
		{claimedRun{ID: "run-1", WorkflowName: "orders", Input: []byte(`{"order":"one"}`)}, "west", "run-1:orders:one:west"},
		{claimedRun{ID: "run-2", WorkflowName: "orders", Input: []byte(`{"order":"two"}`)}, "east", "run-2:orders:two:east"},
	} {
		values := []json.RawMessage{json.RawMessage(`{"region":"` + tc.region + `"}`), nil}
		key, err := wait.resolve(e.stepContext(tc.run, wf, values, 1, "wait"))
		if err != nil || key != tc.want {
			t.Fatalf("resolve(%s) = %q, %v; want %q", tc.run.ID, key, err, tc.want)
		}
		if !json.Valid(values[0]) {
			t.Fatalf("resolver mutated completed values for %s", tc.run.ID)
		}
	}
}

// A truncated error must stay valid UTF-8: PostgreSQL rejects invalid text,
// and the failure (attempt count and message) would be lost.
func TestTruncateErrorKeepsValidUTF8(t *testing.T) {
	msg := strings.Repeat("a", maxErrorLength-1) + "é" + "tail"
	got := truncateError(msg)
	if !utf8.ValidString(got) || len(got) != maxErrorLength-1 {
		t.Fatalf("truncateError: valid=%v len=%d", utf8.ValidString(got), len(got))
	}
	if short := "short"; truncateError(short) != short {
		t.Fatal("short message changed")
	}
}
