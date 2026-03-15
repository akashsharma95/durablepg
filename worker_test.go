package durablepg

import (
	"context"
	"strings"
	"testing"
)

func TestExecuteStepSafelyPanic(t *testing.T) {
	_, err := executeStepSafely(context.Background(), "panic_step", func(context.Context, *StepContext) (any, error) {
		panic("boom")
	}, &StepContext{})
	if err == nil {
		t.Fatal("expected panic error")
	}
	if !strings.Contains(err.Error(), "panic_step") {
		t.Fatalf("error does not include step name: %v", err)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("error does not include panic marker: %v", err)
	}
}

func TestExecuteStepSafelyValue(t *testing.T) {
	out, err := executeStepSafely(context.Background(), "ok_step", func(context.Context, *StepContext) (any, error) {
		return map[string]any{"ok": true}, nil
	}, &StepContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("unexpected output type: %T", out)
	}
	if v, ok := m["ok"].(bool); !ok || !v {
		t.Fatalf("unexpected output value: %#v", out)
	}
}
