package durablepg

import (
	"encoding/json"
	"testing"
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
	for _, tc := range []struct {
		run    claimedRun
		region string
		want   string
	}{
		{claimedRun{ID: "run-1", WorkflowName: "orders", Input: []byte(`{"order":"one"}`)}, "west", "run-1:orders:one:west"},
		{claimedRun{ID: "run-2", WorkflowName: "orders", Input: []byte(`{"order":"two"}`)}, "east", "run-2:orders:two:east"},
	} {
		values := map[string]json.RawMessage{"lookup": json.RawMessage(`{"region":"` + tc.region + `"}`)}
		key, err := resolveWaitKey(tc.run, wait, values)
		if err != nil || key != tc.want {
			t.Fatalf("resolveWaitKey(%s) = %q, %v; want %q", tc.run.ID, key, err, tc.want)
		}
		if !json.Valid(values["lookup"]) {
			t.Fatalf("resolver mutated completed values for %s", tc.run.ID)
		}
	}
}
