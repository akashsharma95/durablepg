package durablepg

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type testWorkflow struct {
	name  string
	build func(*Builder)
}

func (tw testWorkflow) Name() string { return tw.name }
func (tw testWorkflow) Build(b *Builder) {
	tw.build(b)
}

func TestCompileWorkflow(t *testing.T) {
	wf := testWorkflow{
		name: "sample",
		build: func(b *Builder) {
			b.Step("a", func(context.Context, *StepContext) (any, error) { return "ok", nil })
			b.Sleep(2 * time.Second)
			b.WaitEvent("evt", 10*time.Second)
		},
	}
	compiled, err := compileWorkflow(wf)
	if err != nil {
		t.Fatalf("compileWorkflow() error = %v", err)
	}
	if len(compiled.ops) != 3 {
		t.Fatalf("ops len = %d, want 3", len(compiled.ops))
	}
}

func TestCompileWorkflowNoOps(t *testing.T) {
	wf := testWorkflow{name: "empty", build: func(*Builder) {}}
	if _, err := compileWorkflow(wf); err == nil {
		t.Fatal("expected error for workflow with no operations")
	}
}

func TestDefineWorkflow(t *testing.T) {
	wf := DefineWorkflow("defined", func(b *Builder) {
		b.Step("a", func(context.Context, *StepContext) (any, error) { return "ok", nil })
	})
	compiled, err := compileWorkflow(wf)
	if err != nil {
		t.Fatalf("compileWorkflow() error = %v", err)
	}
	if compiled.name != "defined" {
		t.Fatalf("compiled workflow name = %q, want defined", compiled.name)
	}
}

func TestBuilderDuplicateStepPanics(t *testing.T) {
	b := &Builder{}
	b.Step("x", func(context.Context, *StepContext) (any, error) { return nil, nil })

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for duplicate step")
		}
	}()
	b.Step("x", func(context.Context, *StepContext) (any, error) { return nil, nil })
}

func TestStepContextValue(t *testing.T) {
	sc := &StepContext{
		values: map[string]json.RawMessage{
			"step_a": json.RawMessage(`{"n":42}`),
		},
	}
	var out struct {
		N int `json:"n"`
	}
	ok, err := sc.Value("step_a", &out)
	if err != nil {
		t.Fatalf("Value() error = %v", err)
	}
	if !ok || out.N != 42 {
		t.Fatalf("Value() got ok=%v out=%+v", ok, out)
	}
}

func TestStepContextStepResult(t *testing.T) {
	sc := &StepContext{
		RunID:    "run-123",
		Workflow: "signup",
		values: map[string]json.RawMessage{
			"step_a": json.RawMessage(`{"n":42}`),
		},
	}
	var out struct {
		N int `json:"n"`
	}
	if err := sc.StepResult("step_a", &out); err != nil {
		t.Fatalf("StepResult() error = %v", err)
	}
	if out.N != 42 {
		t.Fatalf("StepResult() decoded %+v, want N=42", out)
	}
	if sc.WorkflowID() != "run-123" {
		t.Fatalf("WorkflowID() = %q, want run-123", sc.WorkflowID())
	}
	if sc.WorkflowName() != "signup" {
		t.Fatalf("WorkflowName() = %q, want signup", sc.WorkflowName())
	}
}

func TestStepContextStepResultMissing(t *testing.T) {
	sc := &StepContext{}
	var out struct{}
	err := sc.StepResult("missing", &out)
	if err == nil {
		t.Fatal("expected missing step result error")
	}
	if !errors.Is(err, ErrStepResultNotFound) {
		t.Fatalf("StepResult() error = %v, want ErrStepResultNotFound", err)
	}
}

func TestBackoffDuration(t *testing.T) {
	if got := backoffDuration(1); got != 250*time.Millisecond {
		t.Fatalf("attempt 1 backoff = %v, want 250ms", got)
	}
	if got := backoffDuration(100); got != time.Minute {
		t.Fatalf("attempt 100 backoff = %v, want 1m", got)
	}
}
