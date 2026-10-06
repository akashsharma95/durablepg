package durablepg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

var errLostLease = errors.New("durablepg: lease lost")

const maxErrorLength = 4000

type claimedRun struct {
	ID           WorkflowID
	WorkflowName string
	Version      int
	StepIndex    int
	Input        json.RawMessage
	Attempt      int
	Token        string
	// checkpoints maps operation indexes to stored results.
	checkpoints map[int]json.RawMessage
	deadline    time.Time
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
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
	// Claimed work and the heartbeat outlive the dispatcher's cancellation while
	// shutdown drains. A bounded drain handles steps that ignore cancellation.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	leases := newLeaseTable()
	g.Go(func() error { return e.listenLoop(dispatchCtx, wake) })
	g.Go(func() error { return e.maintenanceLoop(dispatchCtx) })
	g.Go(func() error { return e.retentionLoop(dispatchCtx) })
	g.Go(func() error { return e.heartbeatLoop(workCtx, leases) })
	g.Go(func() error { return e.dispatchLoop(dispatchCtx, workCtx, cancelWork, leases, wake) })
	return g.Wait()
}

// listenLoop wakes dispatch on notifications for this worker's queue. It
// holds its own connection outside the pool, so even a one-connection pool
// keeps notifications. Notifications are hints; polling is the recovery path.
func (e *Engine) listenLoop(ctx context.Context, wake chan<- struct{}) error {
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	for {
		err := e.listen(ctx, signal)
		if ctx.Err() != nil {
			return nil
		}
		e.logger.Error("workflow notification listener", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) listen(ctx context.Context, signal func()) error {
	conn, err := pgx.ConnectConfig(ctx, e.db.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	if _, err := conn.Exec(ctx, "LISTEN "+quoteIdentifier(e.channel)); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// Work may have arrived while disconnected.
	signal()
	for {
		note, err := conn.WaitForNotification(ctx)
		if err != nil {
			signal()
			return err
		}
		if note.Payload == e.queue {
			signal()
		}
	}
}

// maintenanceLoop recovers leases and times out waits independently of event retention.
func (e *Engine) maintenanceLoop(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// Retention has its own loop so a large prune cannot defer recovery.
	maintain := func() {
		if err := e.recoverExpiredLeases(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("recover expired workflow leases", "error", err)
		}
		if err := e.promoteTimedOutWaiters(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("promote timed-out workflow waits", "error", err)
		}
	}
	maintain()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			maintain()
		}
	}
}

func (e *Engine) retentionLoop(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.pruneExpiredEvents(ctx); err != nil && ctx.Err() == nil {
				e.logger.Error("prune expired workflow events", "error", err)
			}
		}
	}
}

func (e *Engine) recoverExpiredLeases(ctx context.Context) error {
	return batches(time.Now(), 8, maintenanceBatch, func() (int64, error) {
		tag, err := e.db.Exec(ctx, e.sql.recoverLeases)
		return tag.RowsAffected(), err
	})
}

// promoteTimedOutWaiters records a timed-out outcome for expired waits and
// makes their runs ready.
func (e *Engine) promoteTimedOutWaiters(ctx context.Context) error {
	return batches(time.Now(), 8, maintenanceBatch, func() (int64, error) {
		var promoted, notified int64
		err := e.db.QueryRow(ctx, e.sql.promoteTimedOut, e.channel).Scan(&promoted, &notified)
		return promoted, err
	})
}

// pruneExpiredEvents keeps each sweep bounded even if expired events arrive
// faster than deletion; lease renewals share this pool.
func (e *Engine) pruneExpiredEvents(ctx context.Context) error {
	return batches(time.Now(), 64, pruneBatch, func() (int64, error) {
		tag, err := e.db.Exec(ctx, e.sql.pruneEvents)
		return tag.RowsAffected(), err
	})
}

// batches repeats batch while it processes full batches, within a count and
// a one-second budget, so a backlog cannot starve the pool.
func batches(started time.Time, maxBatches int, size int64, batch func() (int64, error)) error {
	for range maxBatches {
		n, err := batch()
		if err != nil {
			return err
		}
		if n < size || time.Since(started) >= time.Second {
			return nil
		}
	}
	return nil
}

// heartbeatLoop renews all of this worker's leases every heartbeat interval
// and cancels any claim whose last confirmed deadline passes without renewal.
func (e *Engine) heartbeatLoop(ctx context.Context, leases *leaseTable) error {
	nextRenewal := time.Now().Add(e.heartbeatEvery)
	timer := time.NewTimer(e.heartbeatEvery)
	defer timer.Stop()
	for {
		wakeAt := nextRenewal
		if earliest, ok := leases.expire(time.Now()); ok && earliest.Before(wakeAt) {
			wakeAt = earliest
		}
		timer.Reset(time.Until(wakeAt))
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		now := time.Now()
		if now.Before(nextRenewal) {
			continue
		}
		nextRenewal = now.Add(e.heartbeatEvery)
		// Recomputed here: claims taken during the sleep are not in wakeAt.
		earliest, ok := leases.expire(now)
		runIDs, tokens := leases.snapshot()
		if len(tokens) == 0 {
			continue
		}
		// Never wait for a renewal past the earliest confirmed deadline.
		bound := now.Add(e.leaseTTL)
		if ok {
			bound = earliest
		}
		renewCtx, stop := context.WithDeadline(ctx, bound)
		renewed, err := e.renewLeases(renewCtx, runIDs, tokens)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient failures are tolerated until each lease's deadline.
			e.logger.Warn("renew workflow leases", "error", err)
			continue
		}
		leases.applyRenewal(tokens, renewed, now.Add(e.leaseTTL))
	}
}

func (e *Engine) renewLeases(ctx context.Context, runIDs, tokens []string) (map[string]struct{}, error) {
	rows, err := e.db.Query(ctx, e.sql.renewLeases, runIDs, tokens, e.leaseTTL.Milliseconds())
	if err != nil {
		return nil, err
	}
	renewed := make(map[string]struct{}, len(tokens))
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			rows.Close()
			return nil, err
		}
		renewed[token] = struct{}{}
	}
	return renewed, rows.Err()
}

func (e *Engine) dispatchLoop(ctx, workCtx context.Context, cancelWork context.CancelFunc, leases *leaseTable, wake chan struct{}) error {
	// The heartbeat stops once dispatch returns: claims have either drained or
	// been abandoned.
	defer cancelWork()
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
		// Not ctx: canceling mid-statement can drop rows the server already
		// leased, leaving them to expire as lease failures without running.
		runs, err := e.claimReadyRuns(workCtx, available)
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
			runCtx, release := leases.insert(workCtx, string(run.ID), run.Token, run.deadline)
			wg.Go(func() {
				defer func() {
					release()
					inFlight.Add(-1)
					select {
					case wake <- struct{}{}:
					default:
					}
					e.activeClaims.Done()
				}()
				if err := e.executeClaim(runCtx, run); err != nil {
					e.logger.Error("record workflow run failure", "run_id", run.ID, "workflow", run.WorkflowName, "version", run.Version, "error", err)
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
			drainTimeout := max(e.leaseTTL, 5*time.Second)
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				// Noncooperative user code cannot be killed. Cancel the heartbeat
				// and fence all writes; the run is recovered when the lease expires.
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
	query := e.sql.claimMulti
	var nameArg, versionArg any = names, versions
	if len(names) == 1 {
		query = e.sql.claimSingle
		nameArg, versionArg = names[0], versions[0]
	}

	started := time.Now()
	rows, err := e.db.Query(ctx, query, e.queue, limit, e.leaseTTL.Milliseconds(), nameArg, versionArg)
	if err != nil {
		return nil, fmt.Errorf("durablepg: claim runs: %w", err)
	}
	defer rows.Close()

	// The database set lease_until after started, so this is conservative.
	deadline := started.Add(e.leaseTTL)
	runs := make([]claimedRun, 0, limit)
	for rows.Next() {
		var run claimedRun
		var runID string
		var input, checkpoints []byte
		if err := rows.Scan(&runID, &run.WorkflowName, &run.Version, &run.StepIndex, &run.Attempt, &run.Token, &input, &checkpoints); err != nil {
			return nil, fmt.Errorf("durablepg: scan claimed run: %w", err)
		}
		var raw [][2]json.RawMessage
		if err := json.Unmarshal(checkpoints, &raw); err != nil {
			return nil, fmt.Errorf("durablepg: decode checkpoints: %w", err)
		}
		run.checkpoints = make(map[int]json.RawMessage, len(raw))
		for _, pair := range raw {
			var index int
			if err := json.Unmarshal(pair[0], &index); err != nil {
				return nil, fmt.Errorf("durablepg: decode checkpoint index: %w", err)
			}
			run.checkpoints[index] = pair[1]
		}
		run.ID = WorkflowID(runID)
		run.Input = append([]byte(nil), input...)
		run.deadline = deadline
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("durablepg: iterate claimed runs: %w", err)
	}
	return runs, nil
}

// executeClaim drives a run and records a failure. Lost leases, cancellation,
// and abandoned claims write nothing; it returns only failures to record one.
func (e *Engine) executeClaim(ctx context.Context, run claimedRun) error {
	err := e.drive(ctx, run)
	if err == nil || errors.Is(err, errLostLease) || ctx.Err() != nil {
		return nil
	}
	return e.failOrRetry(ctx, run, err)
}

func (e *Engine) drive(ctx context.Context, run claimedRun) error {
	wf, ok := e.workflow(run.WorkflowName, run.Version)
	if !ok || wf == nil {
		// Claims filter on registered definitions; leave it for lease recovery.
		return errLostLease
	}
	values := make([]json.RawMessage, len(wf.ops))
	for index, raw := range run.checkpoints {
		if index >= 0 && index < len(values) {
			values[index] = raw
		}
	}
	last := len(wf.ops) - 1
	for index := run.StepIndex; index < len(wf.ops); index++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A checkpoint at the cursor was committed by an earlier claim (or an
		// event delivery) whose cursor update this claim has not seen yet.
		if values[index] != nil {
			continue
		}
		complete := index == last
		op := wf.ops[index]
		switch op.kind {
		case opStep:
			raw, err := runStep(ctx, e.stepContext(run, wf, values, index, op.step.name), op.step)
			if err != nil {
				return err
			}
			if values[index], err = e.commitStep(ctx, e.db, run, index, raw, complete); err != nil {
				return err
			}
		case opSleep:
			return e.parkForSleep(ctx, run, index+1, op.sleep)
		case opWaitEvent:
			key, err := op.wait.resolve(e.stepContext(run, wf, values, index, op.wait.name))
			if err != nil {
				return err
			}
			raw, err := e.waitForEvent(ctx, run, index, key, op.wait.timeout, complete)
			if err != nil || raw == nil {
				return err
			}
			values[index] = raw
		default:
			return fmt.Errorf("unsupported operation kind %d", op.kind)
		}
		if complete {
			return nil
		}
	}
	var output json.RawMessage
	if wf.outputIndex >= 0 {
		output = values[wf.outputIndex]
	}
	tag, err := e.db.Exec(ctx, e.sql.completeRun, string(run.ID), run.Token, len(wf.ops), output)
	if err != nil {
		return fmt.Errorf("durablepg: complete run: %w", err)
	}
	return fenced(tag.RowsAffected())
}

// stepContext snapshots completed values so a retained context cannot see
// later results.
func (e *Engine) stepContext(run claimedRun, wf *compiledWorkflow, values []json.RawMessage, index int, name string) *StepContext {
	return &StepContext{
		RunID:    run.ID,
		Workflow: run.WorkflowName,
		StepKey:  fmt.Sprintf("%d:%s", index, name),
		Input:    run.Input,
		values:   append([]json.RawMessage(nil), values[:index]...),
		names:    wf.names,
	}
}

func runStep(ctx context.Context, sc *StepContext, step *stepOp) (json.RawMessage, error) {
	execCtx := ctx
	if step.opts.timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, step.opts.timeout)
		defer cancel()
	}
	out, err := executeStepSafely(execCtx, step.name, step.fn, sc)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("durablepg: marshal step %q output: %w", step.name, err)
	}
	return raw, nil
}

// commitStep saves the result at index and advances the cursor; with
// complete, it also finishes the run. It returns the stored value, which is an
// earlier claim's checkpoint if one already exists at this position.
func (e *Engine) commitStep(ctx context.Context, q querier, run claimedRun, index int, value []byte, complete bool) (json.RawMessage, error) {
	var existing []byte
	err := q.QueryRow(ctx, e.sql.commitStep, string(run.ID), run.Token, index, value, complete).Scan(&existing)
	if isNoRows(err) {
		return nil, errLostLease
	}
	if err != nil {
		return nil, fmt.Errorf("durablepg: commit step: %w", err)
	}
	if existing != nil {
		return existing, nil
	}
	return value, nil
}

func (e *Engine) parkForSleep(ctx context.Context, run claimedRun, next int, d time.Duration) error {
	tag, err := e.db.Exec(ctx, e.sql.parkSleep, string(run.ID), run.Token, next, d.Milliseconds())
	if err != nil {
		return fmt.Errorf("durablepg: park for sleep: %w", err)
	}
	return fenced(tag.RowsAffected())
}

// waitForEvent delivers an already-emitted event, or parks the run on key.
// It returns the stored outcome when delivered and nil when parked.
func (e *Engine) waitForEvent(ctx context.Context, run claimedRun, index int, key string, timeout time.Duration, complete bool) (json.RawMessage, error) {
	tx, err := e.beginEventTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("durablepg: begin wait tx: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	// Matches EmitEvent's lock. Whichever commits second sees the other: an
	// event committed first is found below; a waiter committed first is woken
	// by the emitter.
	if _, err := tx.Exec(ctx, e.sql.lockEventKey, e.schema, key); err != nil {
		return nil, fmt.Errorf("durablepg: lock event key: %w", err)
	}
	var payload []byte
	err = tx.QueryRow(ctx, e.sql.latestEvent, key).Scan(&payload)
	switch {
	case err == nil:
		received := make([]byte, 0, len(payload)+len(`{"received":}`))
		received = append(append(append(received, `{"received":`...), payload...), '}')
		stored, err := e.commitStep(ctx, tx, run, index, received, complete)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("durablepg: commit event delivery: %w", err)
		}
		return stored, nil
	case !isNoRows(err):
		return nil, fmt.Errorf("durablepg: look up event: %w", err)
	}
	tag, err := tx.Exec(ctx, e.sql.parkWait, string(run.ID), run.Token, index, key, timeout.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("durablepg: park for event: %w", err)
	}
	if err := fenced(tag.RowsAffected()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("durablepg: commit wait: %w", err)
	}
	return nil, nil
}

func (e *Engine) failOrRetry(ctx context.Context, run claimedRun, cause error) error {
	msg := truncateError(cause.Error())
	attempt := run.Attempt + 1
	tag, err := e.db.Exec(ctx, e.sql.failOrRetry, string(run.ID), run.Token, attempt, backoffDuration(attempt).Milliseconds(), msg)
	if err != nil {
		return fmt.Errorf("durablepg: record failure: %w", err)
	}
	if tag.RowsAffected() > 0 {
		e.logger.Warn("workflow run attempt failed", "run_id", run.ID, "attempt", attempt, "error", msg)
	}
	return nil
}

// truncateError bounds a stored error without splitting a UTF-8 sequence,
// which PostgreSQL would reject, losing the failure record.
func truncateError(msg string) string {
	if len(msg) <= maxErrorLength {
		return msg
	}
	end := maxErrorLength
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}
	return msg[:end]
}

func fenced(rowsAffected int64) error {
	if rowsAffected == 0 {
		return errLostLease
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
