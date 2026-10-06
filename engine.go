package durablepg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultSchema            = "durable"
	defaultQueue             = "default"
	defaultPollInterval      = 250 * time.Millisecond
	defaultLeaseTTL          = 30 * time.Second
	defaultHeartbeatInterval = 10 * time.Second
	defaultMaxAttempts       = 25
	defaultEventTTL          = 24 * time.Hour
	// Leaves room for the "durablepg_" channel prefix within PostgreSQL's
	// 63-byte identifier limit.
	maxSchemaLength = 48
)

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type workflowKey struct {
	name    string
	version int
}

// Engine executes durable workflows using PostgreSQL.
type Engine struct {
	db *pgxpool.Pool

	schema  string
	qSchema string
	queue   string
	// channel carries NOTIFY wakeups for this schema; the payload is a queue.
	channel string
	sql     queries

	maxConcurrency int
	pollInterval   time.Duration
	leaseTTL       time.Duration
	heartbeatEvery time.Duration
	eventTTL       time.Duration
	workerID       string

	mu        sync.RWMutex
	workflows map[workflowKey]*compiledWorkflow
	logger    *slog.Logger

	// Claims can outlive StartWorker's bounded drain if user steps ignore context.
	activeClaims sync.WaitGroup

	workerMu      sync.Mutex
	workerRunning bool
}

// New creates a workflow engine.
func New(cfg Config) (*Engine, error) {
	if cfg.DB == nil {
		return nil, errors.New("durablepg: cfg.DB is required")
	}

	schema := cfg.Schema
	if schema == "" {
		schema = defaultSchema
	}
	if !identPattern.MatchString(schema) || len(schema) > maxSchemaLength {
		return nil, fmt.Errorf("durablepg: invalid schema %q", schema)
	}

	queue := strings.TrimSpace(cfg.Queue)
	if queue == "" {
		queue = defaultQueue
	}

	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = runtime.GOMAXPROCS(0)
		if maxConcurrency <= 0 {
			maxConcurrency = 1
		}
	}

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}

	leaseTTL := cfg.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}

	heartbeat := cfg.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeatInterval
	}
	if heartbeat >= leaseTTL {
		heartbeat = leaseTTL / 2
		if heartbeat <= 0 {
			heartbeat = time.Second
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	e := &Engine{
		db:             cfg.DB,
		schema:         schema,
		qSchema:        quoteIdentifier(schema),
		queue:          queue,
		channel:        "durablepg_" + schema,
		sql:            newQueries(quoteIdentifier(schema)),
		maxConcurrency: maxConcurrency,
		pollInterval:   pollInterval,
		leaseTTL:       leaseTTL,
		heartbeatEvery: heartbeat,
		eventTTL:       defaultEventTTL,
		workerID:       newUUID(),
		workflows:      make(map[workflowKey]*compiledWorkflow),
		logger:         logger,
	}
	return e, nil
}

// Register compiles and stores a workflow definition.
func (e *Engine) Register(wf Workflow) {
	compiled, err := compileWorkflow(wf)
	if err != nil {
		panic(err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	key := workflowKey{name: compiled.name, version: compiled.version}
	if _, exists := e.workflows[key]; exists {
		panic(fmt.Sprintf("durablepg: workflow %q version %d already registered", compiled.name, compiled.version))
	}
	e.workflows[key] = compiled
}

// RegisterWorkflow registers version 1 of a workflow.
func (e *Engine) RegisterWorkflow(name string, build func(*Builder)) {
	e.Register(DefineWorkflow(name, build))
}

// RegisterWorkflowVersion registers an immutable version of a workflow.
func (e *Engine) RegisterWorkflowVersion(name string, version int, build func(*Builder)) {
	e.Register(DefineWorkflowVersion(name, version, build))
}

func (e *Engine) workflow(name string, version int) (*compiledWorkflow, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	wf, ok := e.workflows[workflowKey{name: name, version: version}]
	return wf, ok
}

func (e *Engine) latestWorkflow(name string) (*compiledWorkflow, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var latest *compiledWorkflow
	for key, wf := range e.workflows {
		if key.name == name && (latest == nil || key.version > latest.version) {
			latest = wf
		}
	}
	return latest, latest != nil
}

func (e *Engine) supportedWorkflows() ([]string, []int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.workflows))
	versions := make([]int, 0, len(e.workflows))
	for key := range e.workflows {
		names = append(names, key.name)
		versions = append(versions, key.version)
	}
	return names, versions
}

// Enqueue schedules a workflow run.
func (e *Engine) Enqueue(ctx context.Context, name string, input any, opts ...EnqueueOption) (WorkflowID, error) {

	rawInput, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("durablepg: marshal input: %w", err)
	}

	o := enqueueOptions{
		queue:       e.queue,
		maxAttempts: defaultMaxAttempts,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.workflowVersion < 0 {
		return "", fmt.Errorf("durablepg: workflow version must be positive")
	}
	var wf *compiledWorkflow
	var ok bool
	if o.workflowVersion == 0 {
		wf, ok = e.latestWorkflow(name)
	} else {
		wf, ok = e.workflow(name, o.workflowVersion)
	}
	if !ok {
		return "", fmt.Errorf("durablepg: workflow %q version %d is not registered", name, o.workflowVersion)
	}

	// Workers match queues exactly; an untrimmed name would never be claimed.
	o.queue = strings.TrimSpace(o.queue)
	if o.queue == "" {
		return "", errors.New("durablepg: queue cannot be empty")
	}
	if o.maxAttempts <= 0 {
		o.maxAttempts = defaultMaxAttempts
	}
	if o.runID == "" {
		o.runID = WorkflowID(newUUID())
	}
	var dedupKey *string
	if o.idempotencyKey != "" {
		dedupKey = &o.idempotencyKey
	}

	var runID string
	var notified int64
	if err := e.db.QueryRow(ctx, e.sql.insertRun, string(o.runID), wf.name, wf.version, o.queue,
		o.maxAttempts, o.scheduledAt, rawInput, dedupKey, e.channel).Scan(&runID, &notified); err != nil {
		return "", fmt.Errorf("durablepg: enqueue: %w", err)
	}
	return WorkflowID(runID), nil
}

// Run starts a workflow run and returns its workflow ID.
func (e *Engine) Run(ctx context.Context, name string, input any, opts ...RunOption) (WorkflowID, error) {
	return e.Enqueue(ctx, name, input, opts...)
}

// RunWorkflow is a compatibility alias for Run.
func (e *Engine) RunWorkflow(ctx context.Context, name string, input any, opts ...RunOption) (WorkflowID, error) {
	return e.Run(ctx, name, input, opts...)
}

// EmitEvent records an event and wakes runs waiting on key, returning how
// many woke. Events stay available to later waits for 24 hours.
func (e *Engine) EmitEvent(ctx context.Context, key string, payload any) (int, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, errors.New("durablepg: event key is required")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("durablepg: marshal event payload: %w", err)
	}

	tx, err := e.beginEventTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("durablepg: begin emit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serializes with waiter registration for this key. The wake statement
	// below starts after the lock, so its snapshot sees any waiter that
	// committed first; a waiter that registers later sees this event.
	if _, err := tx.Exec(ctx, e.sql.lockEventKey, e.schema, key); err != nil {
		return 0, fmt.Errorf("durablepg: lock event key: %w", err)
	}
	var woken, notified int64
	if err := tx.QueryRow(ctx, e.sql.emitEvent, key, raw, e.eventTTL.Milliseconds(), e.channel).Scan(&woken, &notified); err != nil {
		return 0, fmt.Errorf("durablepg: emit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("durablepg: commit emit tx: %w", err)
	}
	return int(woken), nil
}

// beginEventTx starts a transaction for the event-key protocol. Each statement
// after the key lock must see rows committed before it, which READ COMMITTED
// guarantees and a REPEATABLE READ server default would not.
func (e *Engine) beginEventTx(ctx context.Context) (pgx.Tx, error) {
	return e.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}

// RunStatus returns a snapshot of a run, or ErrRunNotFound.
func (e *Engine) RunStatus(ctx context.Context, id WorkflowID) (RunStatus, error) {
	var st RunStatus
	var runID, state string
	err := e.db.QueryRow(ctx, e.sql.runStatus, string(id)).Scan(&runID, &st.WorkflowName, &st.WorkflowVersion,
		&st.Queue, &state, &st.StepIndex, &st.Attempt, &st.MaxAttempts, &st.LeaseFailures, &st.NextRunAt,
		&st.WaitingEventKey, &st.WaitingDeadline, &st.LastError, &st.CreatedAt, &st.UpdatedAt)
	if isNoRows(err) {
		return RunStatus{}, ErrRunNotFound
	}
	if err != nil {
		return RunStatus{}, fmt.Errorf("durablepg: run status: %w", err)
	}
	st.ID, st.State = WorkflowID(runID), RunState(state)
	return st, nil
}

// RunOutput decodes a completed run's output, the result of its last step or
// wait, into dst. It reports false until the run has completed.
func (e *Engine) RunOutput(ctx context.Context, id WorkflowID, dst any) (bool, error) {
	var state string
	var output []byte
	err := e.db.QueryRow(ctx, e.sql.runOutput, string(id)).Scan(&state, &output)
	if isNoRows(err) {
		return false, ErrRunNotFound
	}
	if err != nil {
		return false, fmt.Errorf("durablepg: run output: %w", err)
	}
	if RunState(state) != RunCompleted {
		return false, nil
	}
	if output == nil {
		output = []byte("null")
	}
	if err := json.Unmarshal(output, dst); err != nil {
		return true, fmt.Errorf("durablepg: decode run output: %w", err)
	}
	return true, nil
}

// Cancel moves an unfinished run to cancelled. A worker executing it loses its
// lease, and its step context is canceled at the next lease renewal.
func (e *Engine) Cancel(ctx context.Context, id WorkflowID) (CancelOutcome, error) {
	tag, err := e.db.Exec(ctx, e.sql.cancelRun, string(id))
	if err != nil {
		return CancelOutcome{}, fmt.Errorf("durablepg: cancel run: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return CancelOutcome{Cancelled: true, State: RunCancelled}, nil
	}
	// A separate statement: if the UPDATE waited on a worker that just
	// finished the run, its own snapshot still shows the old state.
	var state string
	err = e.db.QueryRow(ctx, e.sql.runState, string(id)).Scan(&state)
	if isNoRows(err) {
		return CancelOutcome{}, ErrRunNotFound
	}
	if err != nil {
		return CancelOutcome{}, fmt.Errorf("durablepg: read cancelled run: %w", err)
	}
	return CancelOutcome{State: RunState(state)}, nil
}

func (e *Engine) table(name string) string {
	return e.qSchema + "." + quoteIdentifier(name)
}

func quoteIdentifier(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

func (e *Engine) beginWorker() error {
	e.workerMu.Lock()
	defer e.workerMu.Unlock()
	if e.workerRunning {
		return errors.New("durablepg: worker is already running on this engine")
	}
	e.workerRunning = true
	return nil
}

func (e *Engine) endWorker() {
	e.workerMu.Lock()
	e.workerRunning = false
	e.workerMu.Unlock()
}

// WaitForIdle waits for claims that outlived StartWorker's bounded shutdown.
// Call it after StartWorker returns and before closing the database pool.
// A step that ignores cancellation can keep this method waiting indefinitely.
func (e *Engine) WaitForIdle(ctx context.Context) error {
	e.workerMu.Lock()
	defer e.workerMu.Unlock() // Prevent a new worker from adding claims during Wait.
	if e.workerRunning {
		return errors.New("durablepg: stop the worker before waiting for idle")
	}
	done := make(chan struct{})
	go func() {
		e.activeClaims.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newUUID returns a time-ordered UUIDv7, which keeps primary-key inserts
// clustered at the end of the index.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("durablepg: read random bytes: %v", err))
	}
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
