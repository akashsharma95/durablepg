package durablepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkflowID uniquely identifies a workflow run.
type WorkflowID string

var ErrStepResultNotFound = errors.New("durablepg: step result not found")

// ErrRunNotFound reports that no run has the requested ID.
var ErrRunNotFound = errors.New("durablepg: run not found")

// RunState is the lifecycle state of a run.
type RunState string

const (
	RunReady        RunState = "ready"
	RunLeased       RunState = "leased"
	RunWaitingEvent RunState = "waiting_event"
	RunCompleted    RunState = "completed"
	RunFailed       RunState = "failed"
	RunCancelled    RunState = "cancelled"
)

// Terminal reports whether the run can no longer change state.
func (s RunState) Terminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled
}

// RunStatus is a snapshot of a run's progress.
type RunStatus struct {
	ID              WorkflowID
	WorkflowName    string
	WorkflowVersion int
	Queue           string
	State           RunState
	StepIndex       int
	Attempt         int
	MaxAttempts     int
	LeaseFailures   int
	NextRunAt       time.Time
	WaitingEventKey *string
	WaitingDeadline *time.Time
	LastError       *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CancelOutcome reports whether Cancel cancelled the run, or its existing terminal State.
type CancelOutcome struct {
	Cancelled bool
	State     RunState
}

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
	StepKey  string

	values []json.RawMessage
	names  map[string]int
}

func (sc *StepContext) raw(name string) (json.RawMessage, bool) {
	index, ok := sc.names[name]
	if !ok || index >= len(sc.values) || sc.values[index] == nil {
		return nil, false
	}
	return sc.values[index], true
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
	raw, ok := sc.raw(step)
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

// Event decodes a prior wait's payload into dst; false when the wait timed out.
func (sc *StepContext) Event(wait string, dst any) (bool, error) {
	if sc == nil {
		return false, errors.New("nil step context")
	}
	raw, ok := sc.raw(wait)
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrStepResultNotFound, wait)
	}
	var outcome struct {
		Received json.RawMessage `json:"received"`
	}
	if string(raw) == `"timed_out"` {
		return false, nil
	}
	if err := json.Unmarshal(raw, &outcome); err != nil || outcome.Received == nil {
		return false, fmt.Errorf("decode wait outcome %q: not an event outcome", wait)
	}
	if dst == nil {
		return true, nil
	}
	if err := json.Unmarshal(outcome.Received, dst); err != nil {
		return true, fmt.Errorf("decode event payload %q: %w", wait, err)
	}
	return true, nil
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
	raw, ok := sc.raw(step)
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, true
}

// IdempotencyKey is stable across retries for this run and step.
func (sc *StepContext) IdempotencyKey() string {
	if sc == nil || sc.RunID == "" || sc.StepKey == "" {
		return ""
	}
	return string(sc.RunID) + ":" + sc.StepKey
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
	Logger            *slog.Logger
}

// EnqueueOption configures Enqueue.
type EnqueueOption func(*enqueueOptions)

// RunOption is the preferred name for options passed to Run.
type RunOption = EnqueueOption

type enqueueOptions struct {
	runID           WorkflowID
	workflowVersion int
	queue           string
	maxAttempts     int
	idempotencyKey  string
	scheduledAt     *time.Time
}

// WithWorkflowVersion selects a registered definition instead of the latest.
func WithWorkflowVersion(version int) EnqueueOption {
	return func(o *enqueueOptions) {
		o.workflowVersion = version
	}
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
