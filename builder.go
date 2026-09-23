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
	keyFn   func(*StepContext) (string, error)
	timeout time.Duration
}

func (op *waitEventOp) resolve(sc *StepContext) (key string, err error) {
	if op.keyFn != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("durablepg: event key resolver panicked: %v", recovered)
			}
		}()
		key, err = op.keyFn(sc)
		if err != nil {
			return "", err
		}
	} else {
		key = op.key
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("durablepg: resolved event key is required")
	}
	return key, nil
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

// WaitEventFunc resolves a run-specific event key from its input and prior steps.
// The resolver may be called again after a retry; it must be deterministic.
func (b *Builder) WaitEventFunc(keyFn func(*StepContext) (string, error), timeout time.Duration) {
	if b == nil {
		panic("durablepg: nil builder")
	}
	if keyFn == nil {
		panic("durablepg: event key resolver is required")
	}
	if timeout <= 0 {
		panic("durablepg: wait event timeout must be > 0")
	}
	b.ops = append(b.ops, operation{
		kind: opWaitEvent,
		wait: &waitEventOp{keyFn: keyFn, timeout: timeout},
	})
}

type compiledWorkflow struct {
	name    string
	version int
	ops     []operation
}

type workflowDefinition struct {
	name    string
	version int
	build   func(*Builder)
}

func (wf workflowDefinition) Name() string { return wf.name }

func (wf workflowDefinition) Version() int { return wf.version }

func (wf workflowDefinition) Build(b *Builder) {
	if wf.build != nil {
		wf.build(b)
	}
}

// DefineWorkflow creates version 1 of a workflow from a name and builder function.
func DefineWorkflow(name string, build func(*Builder)) Workflow {
	return DefineWorkflowVersion(name, 1, build)
}

// DefineWorkflowVersion creates a workflow definition with an immutable version.
// Keep earlier versions registered while their runs are still active.
func DefineWorkflowVersion(name string, version int, build func(*Builder)) Workflow {
	return workflowDefinition{name: name, version: version, build: build}
}

func compileWorkflow(wf Workflow) (*compiledWorkflow, error) {
	if wf == nil {
		return nil, errors.New("workflow is nil")
	}
	name := strings.TrimSpace(wf.Name())
	if name == "" {
		return nil, errors.New("workflow name is required")
	}
	version := 1
	if versioned, ok := wf.(interface{ Version() int }); ok {
		version = versioned.Version()
	}
	if version < 1 {
		return nil, fmt.Errorf("workflow %q version must be positive", name)
	}

	b := &Builder{stepNames: make(map[string]struct{})}
	wf.Build(b)
	if len(b.ops) == 0 {
		return nil, fmt.Errorf("workflow %q has no operations", name)
	}

	ops := make([]operation, len(b.ops))
	copy(ops, b.ops)
	return &compiledWorkflow{name: name, version: version, ops: ops}, nil
}
