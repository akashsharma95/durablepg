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
	ID           WorkflowID
	WorkflowName string
	StepIndex    int
	Input        json.RawMessage
	Attempt      int
	MaxAttempts  int
}

// StartWorker starts polling, claiming, and executing workflows until ctx is canceled.
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

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return e.listenLoop(ctx, wake) })
	g.Go(func() error { return e.dispatchLoop(ctx, wake) })
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
			time.Sleep(time.Second)
			continue
		}

		_, err = conn.Exec(ctx, "LISTEN "+notifyChannel)
		if err != nil {
			conn.Release()
			if ctx.Err() != nil {
				return nil
			}
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
		time.Sleep(time.Second)
	}
}

func (e *Engine) dispatchLoop(ctx context.Context, wake <-chan struct{}) error {
	var wg sync.WaitGroup
	var inFlight atomic.Int64

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	cycles := 0
	dispatch := func() {
		if err := e.recoverExpiredLeases(ctx); err != nil {
			return
		}
		if err := e.promoteTimedOutWaiters(ctx); err != nil {
			return
		}
		cycles++
		if cycles%120 == 0 {
			_ = e.pruneExpiredEvents(ctx)
		}

		available := e.maxConcurrency - int(inFlight.Load())
		if available <= 0 {
			return
		}

		runs, err := e.claimReadyRuns(ctx, available)
		if err != nil {
			return
		}
		for i := range runs {
			inFlight.Add(1)
			run := runs[i]
			wg.Go(func() {
				defer inFlight.Add(-1)
				_ = e.executeClaim(ctx, run)
			})
		}
	}

	dispatch()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
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

	query := fmt.Sprintf(`
WITH picked AS (
	SELECT id
	FROM %s
	WHERE queue = $1
	  AND state = 'ready'
	  AND next_run_at <= now()
	  AND attempt < max_attempts
	ORDER BY next_run_at, id
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
UPDATE %s wr
SET state = 'leased',
	lease_owner = $3,
	lease_until = now() + ($4::bigint * INTERVAL '1 millisecond'),
	updated_at = now()
FROM picked
WHERE wr.id = picked.id
RETURNING wr.id, wr.workflow_name, wr.step_index, wr.input_json, wr.attempt, wr.max_attempts;
`, e.table("workflow_runs"), e.table("workflow_runs"))

	rows, err := e.db.Query(ctx, query, e.queue, limit, e.workerID, e.leaseTTL.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("durablepg: claim runs: %w", err)
	}
	defer rows.Close()

	runs := make([]claimedRun, 0, limit)
	for rows.Next() {
		var run claimedRun
		var runID string
		var input []byte
		if err := rows.Scan(&runID, &run.WorkflowName, &run.StepIndex, &input, &run.Attempt, &run.MaxAttempts); err != nil {
			return nil, fmt.Errorf("durablepg: scan claimed run: %w", err)
		}
		run.ID = WorkflowID(runID)
		run.Input = append([]byte(nil), input...)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durablepg: iterate claimed runs: %w", err)
	}
	return runs, nil
}

func (e *Engine) executeClaim(ctx context.Context, run claimedRun) error {
	wf, ok := e.workflow(run.WorkflowName)
	if !ok || wf == nil {
		return e.markRunFailed(ctx, run.ID, fmt.Errorf("workflow %q not registered", run.WorkflowName))
	}

	values, err := e.loadStepValues(ctx, run.ID)
	if err != nil {
		return e.failOrRetry(ctx, run, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var lostLease atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		e.heartbeatLoop(runCtx, run.ID, &lostLease, cancel)
	}()
	defer func() {
		cancel()
		<-hbDone
	}()

	index := run.StepIndex
	for index < len(wf.ops) {
		if runCtx.Err() != nil {
			if lostLease.Load() {
				return nil
			}
			return runCtx.Err()
		}

		op := wf.ops[index]
		switch op.kind {
		case opStep:
			err := e.runStep(runCtx, run, index, op.step, values)
			if err != nil {
				if errors.Is(err, errLostLease) || lostLease.Load() {
					return nil
				}
				return e.failOrRetry(ctx, run, err)
			}
			index++
		case opSleep:
			err := e.parkForSleep(ctx, run.ID, index+1, op.sleep)
			if err != nil && !errors.Is(err, errLostLease) {
				return e.failOrRetry(ctx, run, err)
			}
			return nil
		case opWaitEvent:
			waiting, err := e.parkForEvent(ctx, run.ID, index+1, op.wait)
			if err != nil {
				if errors.Is(err, errLostLease) || lostLease.Load() {
					return nil
				}
				return e.failOrRetry(ctx, run, err)
			}
			if waiting {
				return nil
			}
			index++
		default:
			return e.failOrRetry(ctx, run, fmt.Errorf("unsupported operation kind %d", op.kind))
		}
	}

	if lostLease.Load() {
		return nil
	}
	return e.completeRun(ctx, run.ID, values)
}

func (e *Engine) heartbeatLoop(ctx context.Context, runID WorkflowID, lost *atomic.Bool, cancel context.CancelFunc) {
	ticker := time.NewTicker(e.heartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			query := fmt.Sprintf(`
UPDATE %s
SET lease_until = now() + ($2::bigint * INTERVAL '1 millisecond'),
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3;
`, e.table("workflow_runs"))
			tag, err := e.db.Exec(ctx, query, string(runID), e.leaseTTL.Milliseconds(), e.workerID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if tag.RowsAffected() == 0 {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}

func (e *Engine) runStep(ctx context.Context, run claimedRun, index int, step *stepOp, values map[string]json.RawMessage) error {
	if step == nil {
		return fmt.Errorf("durablepg: nil step at index %d", index)
	}
	if _, ok := values[step.name]; ok {
		return e.advanceStep(ctx, run.ID, index+1)
	}

	stepKey := formatStepKey(index, step.name)
	if raw, found, err := e.lookupCheckpoint(ctx, run.ID, stepKey); err != nil {
		return err
	} else if found {
		values[step.name] = raw
		return e.advanceStep(ctx, run.ID, index+1)
	}

	sfKey := string(run.ID) + ":" + stepKey
	resultAny, err, _ := e.stepGroup.Do(sfKey, func() (any, error) {
		if raw, found, err := e.lookupCheckpoint(ctx, run.ID, stepKey); err != nil {
			return nil, err
		} else if found {
			return json.RawMessage(raw), nil
		}

		sc := &StepContext{
			RunID:    run.ID,
			Workflow: run.WorkflowName,
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
			return nil, err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("durablepg: marshal step %q output: %w", step.name, err)
		}

		raw, err = e.persistCheckpoint(execCtx, run.ID, stepKey, raw)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	})
	if err != nil {
		return err
	}

	raw, ok := resultAny.(json.RawMessage)
	if !ok {
		return errors.New("durablepg: invalid step result type")
	}
	values[step.name] = append([]byte(nil), raw...)
	return e.advanceStep(ctx, run.ID, index+1)
}

func (e *Engine) advanceStep(ctx context.Context, runID WorkflowID, nextIndex int) error {
	query := fmt.Sprintf(`
UPDATE %s
SET step_index = $2,
	updated_at = now(),
	last_error = NULL
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3;
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(runID), nextIndex, e.workerID)
	if err != nil {
		return fmt.Errorf("durablepg: advance step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLostLease
	}
	return nil
}

func (e *Engine) parkForSleep(ctx context.Context, runID WorkflowID, nextIndex int, d time.Duration) error {
	query := fmt.Sprintf(`
UPDATE %s
SET state = 'ready',
	step_index = $2,
	next_run_at = now() + ($3::bigint * INTERVAL '1 millisecond'),
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now(),
	last_error = NULL
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $4;
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(runID), nextIndex, d.Milliseconds(), e.workerID)
	if err != nil {
		return fmt.Errorf("durablepg: park for sleep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLostLease
	}
	e.notifyWakeup(ctx, e.queue)
	return nil
}

func (e *Engine) parkForEvent(ctx context.Context, runID WorkflowID, nextIndex int, wait *waitEventOp) (bool, error) {
	if wait == nil {
		return false, errors.New("durablepg: nil wait operation")
	}

	exists, err := e.eventExists(ctx, wait.key)
	if err != nil {
		return false, err
	}
	if exists {
		if err := e.advanceStep(ctx, runID, nextIndex); err != nil {
			return false, err
		}
		return false, nil
	}

	deadline := time.Now().UTC().Add(wait.timeout)
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
	if _, err := tx.Exec(ctx, insertWaiter, wait.key, string(runID), deadline); err != nil {
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
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $5;
`, e.table("workflow_runs"))
	tag, err := tx.Exec(ctx, updateRun, string(runID), nextIndex, wait.key, deadline, e.workerID)
	if err != nil {
		return false, fmt.Errorf("durablepg: set waiting_event state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, errLostLease
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("durablepg: commit wait tx: %w", err)
	}

	// Close registration races where the event committed during wait setup:
	// if an event already exists after commit, immediately move back to ready.
	woke, err := e.wakeWaitingRunIfEventExists(ctx, runID, wait.key)
	if err != nil {
		return false, err
	}
	if woke {
		return true, nil
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

func (e *Engine) persistCheckpoint(ctx context.Context, runID WorkflowID, stepKey string, value []byte) (json.RawMessage, error) {
	insert := fmt.Sprintf(`
INSERT INTO %s (run_id, step_key, value_json, completed_at)
VALUES ($1, $2, $3::jsonb, now())
ON CONFLICT (run_id, step_key)
DO NOTHING;
`, e.table("step_checkpoints"))
	tag, err := e.db.Exec(ctx, insert, string(runID), stepKey, value)
	if err != nil {
		return nil, fmt.Errorf("durablepg: insert checkpoint: %w", err)
	}
	if tag.RowsAffected() > 0 {
		cp := make([]byte, len(value))
		copy(cp, value)
		return cp, nil
	}
	existing, found, err := e.lookupCheckpoint(ctx, runID, stepKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("durablepg: checkpoint insert conflict but row not found")
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

func (e *Engine) completeRun(ctx context.Context, runID WorkflowID, values map[string]json.RawMessage) error {
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
  AND lease_owner = $3;
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, query, string(runID), output, e.workerID)
	if err != nil {
		return fmt.Errorf("durablepg: complete run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
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
  AND lease_owner = $4;
`, e.table("workflow_runs"))
		_, err := e.db.Exec(ctx, query, string(run.ID), nextAttempt, msg, e.workerID)
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
  AND lease_owner = $5;
`, e.table("workflow_runs"))
	_, err := e.db.Exec(ctx, query, string(run.ID), nextAttempt, delay.Milliseconds(), msg, e.workerID)
	if err != nil {
		return fmt.Errorf("durablepg: schedule retry: %w", err)
	}
	e.notifyWakeup(ctx, e.queue)
	return nil
}

func (e *Engine) markRunFailed(ctx context.Context, runID WorkflowID, cause error) error {
	query := fmt.Sprintf(`
UPDATE %s
SET state = 'failed',
	last_error = $2,
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $3;
`, e.table("workflow_runs"))
	_, err := e.db.Exec(ctx, query, string(runID), cause.Error(), e.workerID)
	if err != nil {
		return fmt.Errorf("durablepg: mark run failed: %w", err)
	}
	return nil
}

func (e *Engine) recoverExpiredLeases(ctx context.Context) error {
	query := fmt.Sprintf(`
UPDATE %s
SET state = 'ready',
	next_run_at = now(),
	lease_owner = NULL,
	lease_until = NULL,
	updated_at = now(),
	last_error = COALESCE(last_error, 'lease expired')
WHERE state = 'leased'
  AND lease_until IS NOT NULL
  AND lease_until < now();
`, e.table("workflow_runs"))
	_, err := e.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("durablepg: recover leases: %w", err)
	}
	return nil
}

func (e *Engine) promoteTimedOutWaiters(ctx context.Context) error {
	updateQuery := fmt.Sprintf(`
UPDATE %s wr
SET state = 'ready',
	next_run_at = now(),
	waiting_event_key = NULL,
	waiting_deadline = NULL,
	updated_at = now(),
	last_error = 'wait_event timeout'
WHERE wr.state = 'waiting_event'
  AND wr.waiting_deadline IS NOT NULL
  AND wr.waiting_deadline <= now();
`, e.table("workflow_runs"))
	tag, err := e.db.Exec(ctx, updateQuery)
	if err != nil {
		return fmt.Errorf("durablepg: promote waiters: %w", err)
	}

	cleanupQuery := fmt.Sprintf(`
DELETE FROM %s
WHERE deadline IS NOT NULL
  AND deadline <= now();
`, e.table("waiters"))
	if _, err := e.db.Exec(ctx, cleanupQuery); err != nil {
		return fmt.Errorf("durablepg: cleanup timed-out waiters: %w", err)
	}

	if tag.RowsAffected() > 0 {
		e.notifyWakeup(ctx, e.queue)
	}
	return nil
}

func (e *Engine) pruneExpiredEvents(ctx context.Context) error {
	query := fmt.Sprintf(`
DELETE FROM %s
WHERE expires_at IS NOT NULL
  AND expires_at <= now();
`, e.table("event_log"))
	_, err := e.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("durablepg: prune events: %w", err)
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
