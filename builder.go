package durablepg

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type opKind uint8

const (
	opStep opKind = iota + 1
	opSleep
	opWaitEvent
)

type operation struct {
	kind  opKind
	step  *stepOp
	sleep time.Duration
	wait  *waitEventOp
}

type stepOp struct {
	name string
	fn   StepFunc
	opts stepOptions
}

type waitEventOp struct {
	key     string
	timeout time.Duration
}

// Builder records a workflow definition.
type Builder struct {
	ops       []operation
	stepNames map[string]struct{}
}

// Step appends a durable function step.
func (b *Builder) Step(name string, fn StepFunc, opts ...StepOption) {
	if b == nil {
		panic("durablepg: nil builder")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		panic("durablepg: step name is required")
	}
	if fn == nil {
		panic(fmt.Sprintf("durablepg: step %q has nil function", name))
	}
	if b.stepNames == nil {
		b.stepNames = make(map[string]struct{})
	}
	if _, exists := b.stepNames[name]; exists {
		panic(fmt.Sprintf("durablepg: duplicate step name %q", name))
	}
	b.stepNames[name] = struct{}{}

	o := stepOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.timeout < 0 {
		panic(fmt.Sprintf("durablepg: step %q timeout cannot be negative", name))
	}

	b.ops = append(b.ops, operation{
		kind: opStep,
		step: &stepOp{name: name, fn: fn, opts: o},
	})
}

// Sleep pauses execution until duration elapses.
func (b *Builder) Sleep(d time.Duration) {
	if b == nil {
		panic("durablepg: nil builder")
	}
	if d <= 0 {
		panic("durablepg: sleep duration must be > 0")
	}
	b.ops = append(b.ops, operation{kind: opSleep, sleep: d})
}

// WaitEvent pauses execution until key is emitted or timeout is reached.
func (b *Builder) WaitEvent(key string, timeout time.Duration) {
	if b == nil {
		panic("durablepg: nil builder")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		panic("durablepg: wait event key is required")
	}
	if timeout <= 0 {
		panic("durablepg: wait event timeout must be > 0")
	}
	b.ops = append(b.ops, operation{
		kind: opWaitEvent,
		wait: &waitEventOp{key: key, timeout: timeout},
	})
}

type compiledWorkflow struct {
	name string
	ops  []operation
}

type workflowDefinition struct {
	name  string
	build func(*Builder)
}

func (wf workflowDefinition) Name() string { return wf.name }

func (wf workflowDefinition) Build(b *Builder) {
	if wf.build != nil {
		wf.build(b)
	}
}

// DefineWorkflow creates a workflow from a name and builder function.
func DefineWorkflow(name string, build func(*Builder)) Workflow {
	return workflowDefinition{name: name, build: build}
}

func compileWorkflow(wf Workflow) (*compiledWorkflow, error) {
	if wf == nil {
		return nil, errors.New("workflow is nil")
	}
	name := strings.TrimSpace(wf.Name())
	if name == "" {
		return nil, errors.New("workflow name is required")
	}

	b := &Builder{stepNames: make(map[string]struct{})}
	wf.Build(b)
	if len(b.ops) == 0 {
		return nil, fmt.Errorf("workflow %q has no operations", name)
	}

	ops := make([]operation, len(b.ops))
	copy(ops, b.ops)
	return &compiledWorkflow{name: name, ops: ops}, nil
}
