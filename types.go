package durablepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkflowID uniquely identifies a workflow run.
type WorkflowID string

var ErrStepResultNotFound = errors.New("durablepg: step result not found")

// StepFunc runs one durable step. It must be idempotent.
type StepFunc func(ctx context.Context, sc *StepContext) (any, error)

// Workflow defines a code-first durable workflow.
type Workflow interface {
	Name() string
	Build(*Builder)
}

// StepContext exposes workflow input and completed step values.
type StepContext struct {
	RunID    WorkflowID
	Workflow string
	Input    json.RawMessage

	values map[string]json.RawMessage
}

// DecodeInput decodes workflow input into dst.
func (sc *StepContext) DecodeInput(dst any) error {
	if sc == nil {
		return errors.New("nil step context")
	}
	if len(sc.Input) == 0 {
		return nil
	}
	return json.Unmarshal(sc.Input, dst)
}

// Value decodes a prior step value by step name.
func (sc *StepContext) Value(step string, dst any) (bool, error) {
	if sc == nil {
		return false, errors.New("nil step context")
	}
	raw, ok := sc.values[step]
	if !ok {
		return false, nil
	}
	if dst == nil {
		return true, errors.New("dst cannot be nil")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, fmt.Errorf("decode step value %q: %w", step, err)
	}
	return true, nil
}

// StepResult decodes a prior step value and returns an error when it is absent.
func (sc *StepContext) StepResult(step string, dst any) error {
	ok, err := sc.Value(step, dst)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrStepResultNotFound, step)
	}
	return nil
}

// WorkflowID returns the current workflow run ID.
func (sc *StepContext) WorkflowID() WorkflowID {
	if sc == nil {
		return ""
	}
	return sc.RunID
}

// WorkflowName returns the workflow definition name.
func (sc *StepContext) WorkflowName() string {
	if sc == nil {
		return ""
	}
	return sc.Workflow
}

// RawValue returns the raw JSON for a prior step.
func (sc *StepContext) RawValue(step string) (json.RawMessage, bool) {
	if sc == nil {
		return nil, false
	}
	raw, ok := sc.values[step]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, true
}

// Config configures Engine.
type Config struct {
	DB                *pgxpool.Pool
	Schema            string
	Queue             string
	MaxConcurrency    int
	PollInterval      time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
}

// EnqueueOption configures Enqueue.
type EnqueueOption func(*enqueueOptions)

// RunOption is the preferred name for options passed to Run.
type RunOption = EnqueueOption

type enqueueOptions struct {
	runID          WorkflowID
	queue          string
	maxAttempts    int
	idempotencyKey string
	scheduledAt    *time.Time
}

// WithRunID provides a deterministic run ID.
func WithRunID(id WorkflowID) EnqueueOption {
	return func(o *enqueueOptions) {
		o.runID = id
	}
}

// WithWorkflowID provides a deterministic workflow run ID.
func WithWorkflowID(id WorkflowID) RunOption {
	return WithRunID(id)
}

// WithQueue routes the run to a specific queue.
func WithQueue(queue string) EnqueueOption {
	return func(o *enqueueOptions) {
		o.queue = queue
	}
}

// WithMaxAttempts sets max run attempts.
func WithMaxAttempts(n int) EnqueueOption {
	return func(o *enqueueOptions) {
		o.maxAttempts = n
	}
}

// WithIdempotencyKey deduplicates enqueues per workflow name.
func WithIdempotencyKey(key string) EnqueueOption {
	return func(o *enqueueOptions) {
		o.idempotencyKey = key
	}
}

// WithDeduplicationKey is the preferred alias for WithIdempotencyKey.
func WithDeduplicationKey(key string) RunOption {
	return WithIdempotencyKey(key)
}

// WithScheduledAt sets the first execution timestamp.
func WithScheduledAt(at time.Time) EnqueueOption {
	return func(o *enqueueOptions) {
		o.scheduledAt = &at
	}
}

// StepOption configures one workflow step.
type StepOption func(*stepOptions)

type stepOptions struct {
	timeout time.Duration
}

// WithStepTimeout bounds step execution time.
func WithStepTimeout(timeout time.Duration) StepOption {
	return func(o *stepOptions) {
		o.timeout = timeout
	}
}
