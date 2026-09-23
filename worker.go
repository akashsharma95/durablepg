package durablepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

var errLostLease = errors.New("durablepg: lease lost")

type claimedRun struct {
	ID            WorkflowID
	WorkflowName  string
	Version       int
	StepIndex     int
	Input         json.RawMessage
	Attempt       int
	MaxAttempts   int
	LeaseOwner    string
	leaseDeadline time.Time
}

// StartWorker polls and claims runs until ctx is canceled, then drains active
// claims for at least five seconds and at most max(leaseTTL, five seconds).
// After that, their contexts are canceled; Go cannot forcibly stop a step
// that ignores its context, but claim fencing prevents its later DB writes.
func (e *Engine) StartWorker(ctx context.Context) error {
	if err := e.beginWorker(); err != nil {
		return err
	}
	defer e.endWorker()

	wake := make(chan struct{}, 1)
	select {
	case wake <- struct{}{}:
	default:
	}

	g, dispatchCtx := errgroup.WithContext(ctx)
	// Claimed work and its heartbeat outlive the dispatcher's cancellation while
	// shutdown drains. A bounded drain handles steps that ignore cancellation.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	if e.db.Config().MaxConns > 1 {
		g.Go(func() error { return e.listenLoop(dispatchCtx, wake) })
	}
	g.Go(func() error { return e.maintenanceLoop(dispatchCtx) })
	g.Go(func() error { return e.dispatchLoop(dispatchCtx, workCtx, cancelWork, wake) })
	return g.Wait()
}

func (e *Engine) listenLoop(ctx context.Context, wake chan<- struct{}) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		conn, err := e.db.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e.logger.Error("acquire notification listener", "error", err)
			time.Sleep(time.Second)
			continue
		}

		_, err = conn.Exec(ctx, "LISTEN "+notifyChannel)
		if err != nil {
			conn.Release()
			if ctx.Err() != nil {
				return nil
			}
			e.logger.Error("listen for workflow notifications", "error", err)
			time.Sleep(time.Second)
			continue
		}

		for {
			if ctx.Err() != nil {
				conn.Release()
				return nil
			}

			_, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				break
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}

		conn.Release()
		if ctx.Err() != nil {
			return nil
		}
		e.logger.Error("workflow notification listener disconnected", "error", err)
		time.Sleep(time.Second)
	}
}

// maintenanceLoop keeps recovery and retention off the latency-sensitive
// dispatcher. Both paths use bounded database batches.
func (e *Engine) maintenanceLoop(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(time.Minute)
	defer pruneTicker.Stop()
	maintain := func() {
		if err := e.recoverExpiredLeases(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("recover expired workflow leases", "error", err)
		}
		if err := e.promoteTimedOutWaiters(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("promote timed-out workflow waiters", "error", err)
		}
	}
	maintain()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			maintain()
		case <-pruneTicker.C:
			if err := e.pruneExpiredEvents(ctx); err != nil && ctx.Err() == nil {
				e.logger.Error("prune expired workflow events", "error", err)
			}
		}
	}
}

func (e *Engine) dispatchLoop(ctx, workCtx context.Context, cancelWork context.CancelFunc, wake chan struct{}) error {
	var wg sync.WaitGroup
	var inFlight atomic.Int64

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	dispatch := func() {
		if ctx.Err() != nil {
			return
		}
		available := e.maxConcurrency - int(inFlight.Load())
		if available <= 0 {
			return
		}
		runs, err := e.claimReadyRuns(ctx, available)
		if err != nil {
			if ctx.Err() == nil {
				e.logger.Error("claim ready workflow runs", "error", err)
			}
			return
		}
		for i := range runs {
			inFlight.Add(1)
			e.activeClaims.Add(1)
			run := runs[i]
			wg.Go(func() {
				defer func() {
					inFlight.Add(-1)
					select {
					case wake <- struct{}{}:
					default:
					}
					e.activeClaims.Done()
				}()
				if err := e.executeClaim(workCtx, run); err != nil && !errors.Is(err, errLostLease) && workCtx.Err() == nil {
					e.logger.Error("execute workflow claim", "run_id", run.ID, "workflow", run.WorkflowName, "version", run.Version, "error", err)
				}
			})
		}
	}

	dispatch()
	for {
		select {
		case <-ctx.Done():
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			drainTimeout := e.leaseTTL
			if drainTimeout < 5*time.Second {
				drainTimeout = 5 * time.Second
			}
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				// Noncooperative user code cannot be killed. Cancel the heartbeat
				// and fence all writes; the run is recovered when the lease expires.
				cancelWork()
			}
			return nil
		case <-ticker.C:
			dispatch()
		case <-wake:
			dispatch()
		}
	}
}

func (e *Engine) claimReadyRuns(ctx context.Context, limit int) ([]claimedRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	names, versions := e.supportedWorkflows()
	if len(names) == 0 {
		return nil, nil
	}
	// A single supported definition is common in isolated queues. Equality
	// predicates let PostgreSQL use workflow_runs_selective_ready_idx directly.
	predicate := `EXISTS (SELECT 1 FROM unnest($5::text[], $6::integer[]) AS supported(name, version)
	              WHERE supported.name = workflow_name AND supported.version = workflow_version)`
	var nameArg, versionArg any = names, versions
	if len(names) == 1 {
		predicate = "workflow_name = $5 AND workflow_version = $6"
		nameArg, versionArg = names[0], versions[0]
	}
	query := fmt.Sprintf(`
WITH picked AS (
	SELECT id
	FROM %s
	WHERE queue = $1
	  AND %s
	  AND state = 'ready'
	  AND next_run_at <= now()
	  AND attempt < max_attempts
	ORDER BY next_run_at, id
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
UPDATE %s wr
SET state = 'leased',
	lease_owner = $3 || ':' || md5(random()::text || clock_timestamp()::text || picked.id),
	lease_until = clock_timestamp() + ($4::bigint * INTERVAL '1 millisecond'),
	updated_at = now()
FROM picked
WHERE wr.id = picked.id
RETURNING wr.id, wr.workflow_name, wr.workflow_version, wr.step_index, wr.input_json, wr.attempt, wr.max_attempts, wr.lease_owner;
`, e.table("workflow_runs"), predicate, e.table("workflow_runs"))

	started := time.Now()
	rows, err := e.db.Query(ctx, query, e.queue, limit, e.workerID, e.leaseTTL.Milliseconds(), nameArg, versionArg)
	if err != nil {
		return nil, fmt.Errorf("durablepg: claim runs: %w", err)
	}
	defer rows.Close()

	runs := make([]claimedRun, 0, limit)
	for rows.Next() {
		var run claimedRun
		var runID string
		var input []byte
		if err := rows.Scan(&runID, &run.WorkflowName, &run.Version, &run.StepIndex, &input, &run.Attempt, &run.MaxAttempts, &run.LeaseOwner); err != nil {
			return nil, fmt.Errorf("durablepg: scan claimed run: %w", err)
		}
		run.ID = WorkflowID(runID)
		run.Input = append([]byte(nil), input...)
		run.leaseDeadline = started.Add(time.Duration(e.leaseTTL.Milliseconds()) * time.Millisecond)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durablepg: iterate claimed runs: %w", err)
	}
	return runs, nil
}

func (e *Engine) executeClaim(ctx context.Context, run claimedRun) error {
	wf, ok := e.workflow(run.WorkflowName, run.Version)
	if !ok || wf == nil {
		// A definition may have been unregistered after the claim's registry snapshot.
		return errLostLease
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lostLease atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		e.heartbeatLoop(runCtx, run, &lostLease, cancel)
	}()
	defer func() {
		cancel()
		<-hbDone
	}()

	values, err := e.loadStepValues(runCtx, run.ID)
	if err != nil {
		if runCtx.Err() != nil {
			return nil
		}
		return e.failOrRetry(runCtx, run, err)
	}

	index := run.StepIndex
	for index < len(wf.ops) {
		if runCtx.Err() != nil {
			if lostLease.Load() || ctx.Err() != nil {
				return nil
			}
			return runCtx.Err()
		}

		op := wf.ops[index]
		switch op.kind {
		case opStep:
			err := e.runStep(runCtx, run, index, op.step, values)
			if err != nil {
				if errors.Is(err, errLostLease) || lostLease.Load() || runCtx.Err() != nil {
					return nil
				}
				return e.failOrRetry(runCtx, run, err)
			}
			index++
		case opSleep:
			err := e.parkForSleep(runCtx, run, index+1, op.sleep)
			if err != nil && !errors.Is(err, errLostLease) && runCtx.Err() == nil {
				return e.failOrRetry(runCtx, run, err)
			}
			return nil
		case opWaitEvent:
			key, err := resolveWaitKey(run, op.wait, values)
			if err != nil {
				if runCtx.Err() != nil {
					return nil
				}
				return e.failOrRetry(runCtx, run, err)
			}
			waiting, err := e.parkForEvent(runCtx, run, index+1, key, op.wait.timeout)
			if err != nil {
				if errors.Is(err, errLostLease) || lostLease.Load() || runCtx.Err() != nil {
					return nil
				}
				return e.failOrRetry(runCtx, run, err)
			}
			if waiting {
				return nil
			}
			index++
		default:
			return e.failOrRetry(runCtx, run, fmt.Errorf("unsupported operation kind %d", op.kind))
		}
	}

	if lostLease.Load() || runCtx.Err() != nil {
		return nil
	}
	return e.completeRun(runCtx, run, values)
}

func resolveWaitKey(run claimedRun, wait *waitEventOp, values map[string]json.RawMessage) (string, error) {
	if wait == nil {
		return "", errors.New("durablepg: nil wait operation")
	}
	return wait.resolve(&StepContext{
		RunID: run.ID, Workflow: run.WorkflowName,
		Input: run.Input, values: cloneValues(values),
	})
}

func (e *Engine) heartbeatLoop(ctx context.Context, run claimedRun, lost *atomic.Bool, cancel context.CancelFunc) {
	ticker := time.NewTicker(e.heartbeatEvery)
	defer ticker.Stop()
	deadline := run.leaseDeadline
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	query := fmt.Sprintf(`
UPDATE %s
SET lease_until = clock_timestamp() + ($2::bigint * INTERVAL '1 millisecond'),
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			lost.Store(true)
			cancel()
			return
		case <-ticker.C:
			started := time.Now()
			// Never wait for a renewal past the last confirmed lease deadline.
			renewCtx, stopRenew := context.WithDeadline(ctx, deadline)
			tag, err := e.db.Exec(renewCtx, query, string(run.ID), e.leaseTTL.Milliseconds(), run.LeaseOwner)
			stopRenew()
			if ctx.Err() != nil {
				return
			}
			if !time.Now().Before(deadline) {
				lost.Store(true)
				cancel()
				return
			}
			if err != nil {
				// Transient uncertainty is safe only until the confirmed deadline.
				continue
			}
			if tag.RowsAffected() == 0 {
				lost.Store(true)
				cancel()
				return
			}
			deadline = started.Add(time.Duration(e.leaseTTL.Milliseconds()) * time.Millisecond)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(deadline))
		}
	}
}

func (e *Engine) runStep(ctx context.Context, run claimedRun, index int, step *stepOp, values map[string]json.RawMessage) error {
	if step == nil {
		return fmt.Errorf("durablepg: nil step at index %d", index)
	}
	if _, ok := values[step.name]; ok {
		return e.advanceStep(ctx, run, index+1)
	}

	stepKey := formatStepKey(index, step.name)

	sc := &StepContext{
		RunID:    run.ID,
		Workflow: run.WorkflowName,
		StepKey:  stepKey,
		Input:    run.Input,
		values:   cloneValues(values),
	}

	execCtx := ctx
	cancel := func() {}
	if step.opts.timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, step.opts.timeout)
	}
	defer cancel()

	out, err := executeStepSafely(execCtx, step.name, step.fn, sc)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("durablepg: marshal step %q output: %w", step.name, err)
	}
	raw, err = e.persistCheckpoint(execCtx, run, stepKey, raw)
	if err != nil {
		return err
	}
	values[step.name] = raw
	return e.advanceStep(ctx, run, index+1)
}

func (e *Engine) advanceStep(ctx context.Context, run claimedRun, nextIndex int) error {
	query := fmt.Sprintf(`
UPDATE %s
SET step_index = $2,
	updated_at = now(),
	last_error = NULL,
	lease_failures = 0
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(run.ID), nextIndex, run.LeaseOwner)
	if err != nil {
		return fmt.Errorf("durablepg: advance step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLostLease
	}
	return nil
}

func (e *Engine) parkForSleep(ctx context.Context, run claimedRun, nextIndex int, d time.Duration) error {
	query := fmt.Sprintf(`
UPDATE %s
SET state = 'ready',
	step_index = $2,
	next_run_at = now() + ($3::bigint * INTERVAL '1 millisecond'),
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now(),
	last_error = NULL,
	lease_failures = 0
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $4
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(run.ID), nextIndex, d.Milliseconds(), run.LeaseOwner)
	if err != nil {
		return fmt.Errorf("durablepg: park for sleep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLostLease
	}
	e.notifyWakeup(ctx, e.queue)
	return nil
}

func (e *Engine) parkForEvent(ctx context.Context, run claimedRun, nextIndex int, key string, timeout time.Duration) (bool, error) {
	exists, err := e.eventExists(ctx, key)
	if err != nil {
		return false, err
	}
	if exists {
		if err := e.advanceStep(ctx, run, nextIndex); err != nil {
			return false, err
		}
		return false, nil
	}

	deadline := time.Now().UTC().Add(timeout)
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("durablepg: begin wait tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	insertWaiter := fmt.Sprintf(`
INSERT INTO %s (event_key, run_id, deadline, created_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (event_key, run_id)
DO UPDATE SET deadline = EXCLUDED.deadline;
`, e.table("waiters"))
	if _, err := tx.Exec(ctx, insertWaiter, key, string(run.ID), deadline); err != nil {
		return false, fmt.Errorf("durablepg: insert waiter: %w", err)
	}

	updateRun := fmt.Sprintf(`
UPDATE %s
SET state = 'waiting_event',
	step_index = $2,
	waiting_event_key = $3,
	waiting_deadline = $4,
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now(),
	lease_failures = 0
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $5
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	tag, err := tx.Exec(ctx, updateRun, string(run.ID), nextIndex, key, deadline, run.LeaseOwner)
	if err != nil {
		return false, fmt.Errorf("durablepg: set waiting_event state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, errLostLease
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("durablepg: commit wait tx: %w", err)
	}

	// Close registration races where the event committed during wait setup.
	// Once the lease is released, the heartbeat can cancel the execution
	// context. The race-closing wakeup must still finish independently.
	wakeCtx, cancelWake := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelWake()
	_, err = e.wakeWaitingRunIfEventExists(wakeCtx, run.ID, key)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) wakeWaitingRunIfEventExists(ctx context.Context, runID WorkflowID, key string) (bool, error) {
	updateQuery := fmt.Sprintf(`
UPDATE %s
SET state = 'ready',
	next_run_at = now(),
	waiting_event_key = NULL,
	waiting_deadline = NULL,
	updated_at = now()
WHERE id = $1
  AND state = 'waiting_event'
  AND waiting_event_key = $2
  AND EXISTS (
	  SELECT 1
	  FROM %s
	  WHERE event_key = $2
	    AND (expires_at IS NULL OR expires_at > now())
	  ORDER BY created_at DESC
	  LIMIT 1
  )
RETURNING id;
`, e.table("workflow_runs"), e.table("event_log"))
	var awakenedID string
	err := e.db.QueryRow(ctx, updateQuery, string(runID), key).Scan(&awakenedID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("durablepg: wake waiting run: %w", err)
	}

	cleanupQuery := fmt.Sprintf(`
DELETE FROM %s
WHERE run_id = $1
  AND event_key = $2;
`, e.table("waiters"))
	if _, err := e.db.Exec(ctx, cleanupQuery, awakenedID, key); err != nil {
		return false, fmt.Errorf("durablepg: cleanup waiter after wake: %w", err)
	}
	e.notifyWakeup(ctx, e.queue)
	return true, nil
}

func (e *Engine) eventExists(ctx context.Context, key string) (bool, error) {
	query := fmt.Sprintf(`
SELECT payload_json
FROM %s
WHERE event_key = $1
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at DESC
LIMIT 1;
`, e.table("event_log"))
	var raw []byte
	err := e.db.QueryRow(ctx, query, key).Scan(&raw)
	if err == nil {
		return true, nil
	}
	if isNoRows(err) {
		return false, nil
	}
	return false, fmt.Errorf("durablepg: lookup event: %w", err)
}

func (e *Engine) lookupCheckpoint(ctx context.Context, runID WorkflowID, stepKey string) (json.RawMessage, bool, error) {
	query := fmt.Sprintf(`
SELECT value_json
FROM %s
WHERE run_id = $1
  AND step_key = $2;
`, e.table("step_checkpoints"))
	var raw []byte
	err := e.db.QueryRow(ctx, query, string(runID), stepKey).Scan(&raw)
	if err == nil {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, true, nil
	}
	if isNoRows(err) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("durablepg: lookup checkpoint: %w", err)
}

func (e *Engine) persistCheckpoint(ctx context.Context, run claimedRun, stepKey string, value []byte) (json.RawMessage, error) {
	// Lock the run row so recovery/reclaim cannot interleave between checking
	// ownership and inserting the checkpoint. ON CONFLICT preserves the first
	// completed result after a retry.
	insert := fmt.Sprintf(`
WITH owned AS (
	SELECT id FROM %s
	WHERE id = $1 AND state = 'leased'
	  AND lease_owner = $4 AND lease_until > clock_timestamp()
	FOR UPDATE
)
INSERT INTO %s (run_id, step_key, value_json, completed_at)
SELECT id, $2, $3::jsonb, now() FROM owned WHERE true
ON CONFLICT (run_id, step_key) DO NOTHING;
`, e.table("workflow_runs"), e.table("step_checkpoints"))
	tag, err := e.db.Exec(ctx, insert, string(run.ID), stepKey, value, run.LeaseOwner)
	if err != nil {
		return nil, fmt.Errorf("durablepg: insert checkpoint: %w", err)
	}
	if tag.RowsAffected() > 0 {
		cp := make([]byte, len(value))
		copy(cp, value)
		return cp, nil
	}
	existing, found, err := e.lookupCheckpoint(ctx, run.ID, stepKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errLostLease
	}
	return existing, nil
}

func (e *Engine) loadStepValues(ctx context.Context, runID WorkflowID) (map[string]json.RawMessage, error) {
	query := fmt.Sprintf(`
SELECT step_key, value_json
FROM %s
WHERE run_id = $1;
`, e.table("step_checkpoints"))
	rows, err := e.db.Query(ctx, query, string(runID))
	if err != nil {
		return nil, fmt.Errorf("durablepg: load checkpoints: %w", err)
	}
	defer rows.Close()

	values := make(map[string]json.RawMessage)
	for rows.Next() {
		var stepKey string
		var raw []byte
		if err := rows.Scan(&stepKey, &raw); err != nil {
			return nil, fmt.Errorf("durablepg: scan checkpoint: %w", err)
		}
		name := stepName(stepKey)
		cp := make([]byte, len(raw))
		copy(cp, raw)
		values[name] = cp
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durablepg: iterate checkpoints: %w", err)
	}
	return values, nil
}

func (e *Engine) completeRun(ctx context.Context, run claimedRun, values map[string]json.RawMessage) error {
	output, err := marshalOutput(values)
	if err != nil {
		return err
	}

	query := fmt.Sprintf(`
UPDATE %s
SET state = 'completed',
	output_json = $2::jsonb,
	lease_owner = NULL,
	lease_until = NULL,
	waiting_event_key = NULL,
	waiting_deadline = NULL,
	last_error = NULL,
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(run.ID), output, run.LeaseOwner)
	if err != nil {
		return fmt.Errorf("durablepg: complete run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLostLease
	}
	return nil
}

func (e *Engine) failOrRetry(ctx context.Context, run claimedRun, cause error) error {
	if errors.Is(cause, errLostLease) {
		return nil
	}

	msg := cause.Error()
	if len(msg) > 4000 {
		msg = msg[:4000]
	}

	nextAttempt := run.Attempt + 1
	if nextAttempt >= run.MaxAttempts {
		query := fmt.Sprintf(`
UPDATE %s
SET state = 'failed',
	attempt = $2,
	last_error = $3,
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $4
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
		_, err := e.db.Exec(ctx, query, string(run.ID), nextAttempt, msg, run.LeaseOwner)
		if err != nil {
			return fmt.Errorf("durablepg: mark failed: %w", err)
		}
		return nil
	}

	delay := backoffDuration(nextAttempt)
	query := fmt.Sprintf(`
UPDATE %s
SET state = 'ready',
	attempt = $2,
	next_run_at = now() + ($3::bigint * INTERVAL '1 millisecond'),
	last_error = $4,
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now()
WHERE id = $1
  AND state = 'leased' 
  AND lease_owner = $5
  AND lease_until > clock_timestamp();
`, e.table("workflow_runs"))
	_, err := e.db.Exec(ctx, query, string(run.ID), nextAttempt, delay.Milliseconds(), msg, run.LeaseOwner)
	if err != nil {
		return fmt.Errorf("durablepg: schedule retry: %w", err)
	}
	e.notifyWakeup(ctx, e.queue)
	return nil
}

func (e *Engine) recoverExpiredLeases(ctx context.Context) error {
	query := fmt.Sprintf(`
WITH picked AS (
	SELECT id
	FROM %s
	WHERE state = 'leased' AND lease_until < clock_timestamp()
	ORDER BY lease_until, id
	LIMIT 1024
	FOR UPDATE SKIP LOCKED
)
UPDATE %s wr
SET state = CASE WHEN wr.lease_failures + 1 >= wr.max_attempts OR wr.attempt >= wr.max_attempts
                 THEN 'failed' ELSE 'ready' END,
	lease_failures = wr.lease_failures + 1,
	next_run_at = clock_timestamp() + (LEAST(60000, 250 * (1 << LEAST(wr.lease_failures, 8))) * INTERVAL '1 millisecond'),
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now(),
	last_error = 'lease expired'
FROM picked
WHERE wr.id = picked.id;
`, e.table("workflow_runs"), e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("durablepg: recover leases: %w", err)
	}
	if tag.RowsAffected() > 0 {
		e.notifyWakeup(ctx, e.queue)
	}
	return nil
}

func (e *Engine) promoteTimedOutWaiters(ctx context.Context) error {
	query := fmt.Sprintf(`
WITH picked AS (
	SELECT id
	FROM %s
	WHERE state = 'waiting_event' AND waiting_deadline <= clock_timestamp()
	ORDER BY waiting_deadline, id
	LIMIT 1024
	FOR UPDATE SKIP LOCKED
), promoted AS (
	UPDATE %s wr
	SET state = 'ready',
		next_run_at = now(),
		waiting_event_key = NULL,
		waiting_deadline = NULL,
		updated_at = now(),
		last_error = 'wait_event timeout'
	FROM picked
	WHERE wr.id = picked.id
	RETURNING wr.id
), removed AS (
	DELETE FROM %s w USING promoted p WHERE w.run_id = p.id
)
SELECT count(*) FROM promoted;
`, e.table("workflow_runs"), e.table("workflow_runs"), e.table("waiters"))
	var count int
	if err := e.db.QueryRow(ctx, query).Scan(&count); err != nil {
		return fmt.Errorf("durablepg: promote waiters: %w", err)
	}
	if count > 0 {
		e.notifyWakeup(ctx, e.queue)
	}

	// Remove orphaned expired registrations left by earlier, non-atomic wakes.
	// Never delete a still-active registration, even when its deadline passed.
	cleanup := fmt.Sprintf(`
WITH expired AS (
	SELECT w.event_key, w.run_id
	FROM %s w
	WHERE w.deadline <= clock_timestamp()
	ORDER BY w.deadline, w.run_id
	LIMIT 1024
	FOR UPDATE OF w SKIP LOCKED
)
DELETE FROM %s w USING expired x
WHERE w.event_key = x.event_key AND w.run_id = x.run_id
  AND NOT EXISTS (
	SELECT 1 FROM %s wr
	WHERE wr.id = w.run_id AND wr.state = 'waiting_event'
	  AND wr.waiting_event_key = w.event_key
	  AND wr.waiting_deadline IS NOT DISTINCT FROM w.deadline
  );
`, e.table("waiters"), e.table("waiters"), e.table("workflow_runs"))
	if _, err := e.db.Exec(ctx, cleanup); err != nil {
		return fmt.Errorf("durablepg: cleanup stale waiters: %w", err)
	}
	return nil
}

func (e *Engine) pruneExpiredEvents(ctx context.Context) error {
	query := fmt.Sprintf(`
WITH expired AS (
	SELECT tableoid, ctid
	FROM %s
	WHERE expires_at <= clock_timestamp()
	ORDER BY expires_at
	LIMIT 2048
	FOR UPDATE SKIP LOCKED
)
DELETE FROM %s ev USING expired
WHERE ev.tableoid = expired.tableoid AND ev.ctid = expired.ctid;
`, e.table("event_log"), e.table("event_log"))
	// Retention must keep pace with event volume without one unbounded DELETE.
	// Limit each sweep so claim and wake queries retain access to the pool.
	for range 64 {
		tag, err := e.db.Exec(ctx, query)
		if err != nil {
			return fmt.Errorf("durablepg: prune events: %w", err)
		}
		if tag.RowsAffected() < 2048 {
			return nil
		}
	}
	return nil
}

func backoffDuration(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 8 {
		shift = 8
	}
	delay := 250 * time.Millisecond * time.Duration(1<<shift)
	if delay > time.Minute {
		delay = time.Minute
	}
	return delay
}

func formatStepKey(index int, step string) string {
	return fmt.Sprintf("%04d:%s", index, step)
}

func stepName(stepKey string) string {
	parts := strings.SplitN(stepKey, ":", 2)
	if len(parts) != 2 {
		return stepKey
	}
	return parts[1]
}

func cloneValues(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func marshalOutput(values map[string]json.RawMessage) ([]byte, error) {
	if len(values) == 0 {
		return []byte(`{}`), nil
	}
	out := make(map[string]json.RawMessage, len(values))
	for k, v := range values {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("durablepg: marshal output: %w", err)
	}
	return raw, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func executeStepSafely(ctx context.Context, name string, fn StepFunc, sc *StepContext) (_ any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("durablepg: step %q panicked: %v\n%s", name, r, string(debug.Stack()))
		}
	}()
	return fn(ctx, sc)
}
