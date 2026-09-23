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
	notifyChannel            = "durable_wakeup"
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

	versionMu      sync.Mutex
	postgresMajor  int
	checkedVersion bool

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
	if !identPattern.MatchString(schema) {
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

	if strings.TrimSpace(o.queue) == "" {
		o.queue = e.queue
	}
	if o.maxAttempts <= 0 {
		o.maxAttempts = defaultMaxAttempts
	}
	if o.runID == "" {
		o.runID = WorkflowID(e.nextRunID(ctx))
	}

	nextRunAt := time.Now().UTC()
	if o.scheduledAt != nil {
		nextRunAt = o.scheduledAt.UTC()
	}

	var runID string
	if o.idempotencyKey != "" {
		query := fmt.Sprintf(`
INSERT INTO %s (id, workflow_name, workflow_version, queue, state, step_index, attempt, max_attempts, next_run_at, input_json, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'ready', 0, 0, $5, $6, $7::jsonb, $8, now(), now())
ON CONFLICT (workflow_name, idempotency_key)
WHERE idempotency_key IS NOT NULL
DO UPDATE SET updated_at = now()
RETURNING id;
`, e.table("workflow_runs"))
		if err := e.db.QueryRow(ctx, query, string(o.runID), wf.name, wf.version, o.queue, o.maxAttempts, nextRunAt, rawInput, o.idempotencyKey).Scan(&runID); err != nil {
			return "", fmt.Errorf("durablepg: enqueue upsert: %w", err)
		}
	} else {
		query := fmt.Sprintf(`
INSERT INTO %s (id, workflow_name, workflow_version, queue, state, step_index, attempt, max_attempts, next_run_at, input_json, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'ready', 0, 0, $5, $6, $7::jsonb, now(), now())
RETURNING id;
`, e.table("workflow_runs"))
		if err := e.db.QueryRow(ctx, query, string(o.runID), wf.name, wf.version, o.queue, o.maxAttempts, nextRunAt, rawInput).Scan(&runID); err != nil {
			return "", fmt.Errorf("durablepg: enqueue insert: %w", err)
		}
	}

	e.notifyWakeup(ctx, o.queue)
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

// EmitEvent records an event and wakes waiting workflows.
func (e *Engine) EmitEvent(ctx context.Context, key string, payload any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("durablepg: event key is required")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("durablepg: marshal event payload: %w", err)
	}

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("durablepg: begin emit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	insertEvent := fmt.Sprintf(`
INSERT INTO %s (event_key, payload_json, created_at, expires_at)
VALUES ($1, $2::jsonb, now(), now() + ($3::bigint * INTERVAL '1 millisecond'));
`, e.table("event_log"))
	if _, err := tx.Exec(ctx, insertEvent, key, raw, e.eventTTL.Milliseconds()); err != nil {
		return fmt.Errorf("durablepg: insert event: %w", err)
	}

	wakeQuery := fmt.Sprintf(`
WITH awakened AS (
	UPDATE %s wr
	SET state = 'ready',
		next_run_at = now(),
		waiting_event_key = NULL,
		waiting_deadline = NULL,
		lease_owner = NULL,
		lease_until = NULL,
		last_error = NULL,
		updated_at = now()
	WHERE wr.state = 'waiting_event'
	  AND wr.waiting_event_key = $1
	  AND wr.id IN (
		SELECT w.run_id
		FROM %s w
		WHERE w.event_key = $1
		  AND w.run_id = wr.id
		  AND (w.deadline IS NULL OR w.deadline > now())
	)
	RETURNING wr.id
)
DELETE FROM %s w
USING awakened a
WHERE w.run_id = a.id AND w.event_key = $1;
`, e.table("workflow_runs"), e.table("waiters"), e.table("waiters"))
	if _, err := tx.Exec(ctx, wakeQuery, key); err != nil {
		return fmt.Errorf("durablepg: wake waiters: %w", err)
	}

	cleanupWaiters := fmt.Sprintf(`
DELETE FROM %s
WHERE event_key = $1
  AND deadline IS NOT NULL
  AND deadline <= now();
`, e.table("waiters"))
	if _, err := tx.Exec(ctx, cleanupWaiters, key); err != nil {
		return fmt.Errorf("durablepg: cleanup waiters: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("durablepg: commit emit tx: %w", err)
	}

	e.notifyWakeup(ctx, e.queue)
	return nil
}

func (e *Engine) notifyWakeup(ctx context.Context, payload string) {
	const query = "SELECT pg_notify($1, $2);"
	_, _ = e.db.Exec(ctx, query, notifyChannel, payload)
}

func (e *Engine) table(name string) string {
	return e.qSchema + "." + quoteIdentifier(name)
}

func quoteIdentifier(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

func (e *Engine) nextRunID(ctx context.Context) string {
	if e.postgresVersion(ctx) >= 18 {
		var id string
		if err := e.db.QueryRow(ctx, "SELECT uuidv7()::text;").Scan(&id); err == nil && id != "" {
			return id
		}
	}
	return newUUID()
}

func (e *Engine) postgresVersion(ctx context.Context) int {
	e.versionMu.Lock()
	if e.checkedVersion {
		v := e.postgresMajor
		e.versionMu.Unlock()
		return v
	}
	e.versionMu.Unlock()

	var num int
	if err := e.db.QueryRow(ctx, "SHOW server_version_num;").Scan(&num); err != nil {
		return 0
	}

	major := num / 10000
	e.versionMu.Lock()
	e.postgresMajor = major
	e.checkedVersion = true
	e.versionMu.Unlock()
	return major
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

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}

	b[6] = (b[6] & 0x0f) | 0x40
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
