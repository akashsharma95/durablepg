package durablepg

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// architectureQueryTrace gates a completed database operation, not a timer:
// the producer can commit precisely after the worker's first event miss.
type architectureQueryTrace struct {
	start func(context.Context, pgx.TraceQueryStartData) context.Context
	end   func(context.Context, pgx.TraceQueryEndData)
}

func (tr architectureQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if tr.start != nil {
		return tr.start(ctx, data)
	}
	return ctx
}

func (tr architectureQueryTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if tr.end != nil {
		tr.end(ctx, data)
	}
}

func architectureWorker(t *testing.T, producer *Engine, tracer pgx.QueryTracer) (*Engine, *pgxpool.Pool) {
	t.Helper()
	return architectureWorkerWith(t, producer, func(cfg *pgxpool.Config) { cfg.ConnConfig.Tracer = tracer })
}

func architectureWorkerWith(t *testing.T, producer *Engine, configure func(*pgxpool.Config)) (*Engine, *pgxpool.Pool) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DURABLEPG_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	configure(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(Config{
		DB: pool, Schema: producer.schema, PollInterval: 10 * time.Millisecond,
		LeaseTTL: 2 * time.Second, HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	return worker, pool
}

func architectureSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out awaiting %s", name)
	}
}

func architectureStopWorker(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker shutdown timed out")
	}
}

// The waiter-registration race: an event committed while the worker is
// blocked on the key lock must be seen by the worker's lookup, so the run is
// not parked until its timeout.
func TestIntegrationEventCommittedWhileWaiterBlocksOnLockIsDelivered(t *testing.T) {
	producer, _ := integrationEngine(t)
	worker, pool := architectureWorkerWith(t, producer, func(*pgxpool.Config) {})
	defer pool.Close()
	eventCommittedDuringLockWaitIsDelivered(t, producer, worker)
}

// The race protocol needs each statement's snapshot to start after the key
// lock. Under a REPEATABLE READ default the snapshot is taken by the lock
// statement itself, before the lock is granted, so the engine must pin READ
// COMMITTED or the run parks after the event and is never woken.
func TestIntegrationWaitRaceHoldsUnderRepeatableReadDefault(t *testing.T) {
	producer, _ := integrationEngine(t)
	worker, pool := architectureWorkerWith(t, producer, func(cfg *pgxpool.Config) {
		cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	})
	defer pool.Close()
	eventCommittedDuringLockWaitIsDelivered(t, producer, worker)
}

func eventCommittedDuringLockWaitIsDelivered(t *testing.T, producer, worker *Engine) {
	t.Helper()
	ctx := context.Background()
	build := func(b *Builder) { b.WaitEvent("signal", "race-key", time.Minute) }
	producer.RegisterWorkflow("race", build)
	worker.RegisterWorkflow("race", build)

	// Hold the key lock and write an event in an open transaction, as a
	// concurrent emitter would.
	emitter, err := producer.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer emitter.Rollback(ctx) //nolint:errcheck
	if _, err := emitter.Exec(ctx, producer.sql.lockEventKey, producer.schema, "race-key"); err != nil {
		t.Fatal(err)
	}
	insert := fmt.Sprintf("INSERT INTO %s (event_key, payload_json, expires_at) VALUES ('race-key', '7', now() + interval '1 hour')", producer.table("event_log"))
	if _, err := emitter.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}

	stop := startTestWorker(t, worker)
	defer stop()
	runID, err := producer.Run(ctx, "race", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, producer, runID, "leased")
	time.Sleep(200 * time.Millisecond)
	if st, err := producer.RunStatus(ctx, runID); err != nil || st.State != RunLeased {
		t.Fatalf("worker must block on the key lock: %+v, %v", st, err)
	}
	if err := emitter.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	waitRunState(t, producer, runID, "completed")
	// A retry could also deliver the event, but the protocol must not need one:
	// a parked run would only be found again at its timeout.
	st, err := producer.RunStatus(ctx, runID)
	if err != nil || st.Attempt != 0 {
		t.Fatalf("delivered after a failed attempt: %+v, %v", st, err)
	}
	var outcome struct {
		Received int `json:"received"`
	}
	if ok, err := producer.RunOutput(ctx, runID, &outcome); err != nil || !ok || outcome.Received != 7 {
		t.Fatalf("output = %+v, %v, %v; want received 7", outcome, ok, err)
	}
}

// A checkpoint left at the current cursor by an older or interrupted worker
// is authoritative. Recovery must expose that value to later steps and must
// never repeat the checkpointed callback.
func TestIntegrationCheckpointAtCursorRecoversCanonicalOutput(t *testing.T) {
	e, pool := integrationEngine(t)
	var called atomic.Int32
	e.RegisterWorkflow("cursor_recovery", func(b *Builder) {
		b.Step("charged", func(context.Context, *StepContext) (any, error) {
			called.Add(1)
			return "duplicate-charge", nil
		})
		b.Step("receipt", func(_ context.Context, sc *StepContext) (any, error) {
			var charged string
			if _, err := sc.Value("charged", &charged); err != nil {
				return nil, err
			}
			return "receipt:" + charged, nil
		})
	})
	runID, err := e.Run(context.Background(), "cursor_recovery", nil)
	if err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("INSERT INTO %s (run_id, step_index, value_json) VALUES ($1, 0, $2::jsonb)", e.table("step_checkpoints"))
	if _, err := pool.Exec(context.Background(), query, string(runID), `"canonical"`); err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	waitRunState(t, e, runID, "completed")
	var output string
	if _, err := e.RunOutput(context.Background(), runID, &output); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 0 || output != "receipt:canonical" {
		t.Fatalf("checkpoint recovery invoked callback %d times, output %q", called.Load(), output)
	}
}

// Pause after the database has committed the first checkpoint but before the
// old owner proceeds. Replacing the lease must fence that owner, and recovery
// of the committed step must use its canonical result without reexecution.
func TestIntegrationAtomicCheckpointCommitFencesStaleOwner(t *testing.T) {
	producer, pool := integrationEngine(t)
	var called atomic.Int32
	build := func(b *Builder) {
		b.Step("charge", func(context.Context, *StepContext) (any, error) {
			called.Add(1)
			return "canonical", nil
		})
		b.Step("receipt", func(_ context.Context, sc *StepContext) (any, error) {
			var charged string
			err := sc.StepResult("charge", &charged)
			return "receipt:" + charged, err
		})
	}
	producer.RegisterWorkflow("atomic_checkpoint", build)
	committed := make(chan struct{})
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	var firstCommit atomic.Bool
	type commitMark struct{}
	trace := architectureQueryTrace{
		start: func(ctx context.Context, data pgx.TraceQueryStartData) context.Context {
			// The step commit; maintenance also inserts checkpoints for timeouts.
			if strings.Contains(data.SQL, "owned AS MATERIALIZED") && firstCommit.CompareAndSwap(false, true) {
				return context.WithValue(ctx, commitMark{}, true)
			}
			return ctx
		},
		end: func(ctx context.Context, _ pgx.TraceQueryEndData) {
			if ctx.Value(commitMark{}) != nil {
				close(committed)
				<-resume
			}
		},
	}
	first, firstPool := architectureWorker(t, producer, trace)
	first.RegisterWorkflow("atomic_checkpoint", build)
	defer firstPool.Close()

	runID, err := producer.Run(context.Background(), "atomic_checkpoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.StartWorker(ctx) }()
	stopped := false
	defer func() {
		release()
		if !stopped {
			architectureStopWorker(t, cancel, done)
		}
	}()
	architectureSignal(t, committed, "first checkpoint commit")

	var cursor int
	var canonical string
	query := fmt.Sprintf(`SELECT wr.step_index, cp.value_json FROM %s wr JOIN %s cp ON cp.run_id = wr.id WHERE wr.id = $1 AND cp.step_index = 0`,
		producer.table("workflow_runs"), producer.table("step_checkpoints"))
	if err := pool.QueryRow(context.Background(), query, string(runID)).Scan(&cursor, &canonical); err != nil {
		t.Fatal(err)
	}
	if cursor != 1 || canonical != `"canonical"` {
		t.Fatalf("checkpoint and cursor did not commit atomically: cursor=%d value=%s", cursor, canonical)
	}
	steal := fmt.Sprintf(`UPDATE %s SET lease_token = gen_random_uuid(), lease_until = clock_timestamp() - INTERVAL '1 second' WHERE id = $1 AND state = 'leased'`, producer.table("workflow_runs"))
	if tag, err := pool.Exec(context.Background(), steal, string(runID)); err != nil {
		t.Fatal(err)
	} else if tag.RowsAffected() != 1 {
		t.Fatal("original claim was not leased when replacing its owner")
	}
	cancel()
	release()
	architectureStopWorker(t, cancel, done)
	stopped = true

	var state string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT state FROM %s WHERE id = $1", producer.table("workflow_runs")), string(runID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "completed" {
		t.Fatal("stale owner completed the run after its lease was replaced")
	}

	stop := startTestWorker(t, producer)
	defer stop()
	waitRunState(t, producer, runID, "completed")
	var output string
	if _, err := producer.RunOutput(context.Background(), runID, &output); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 || output != "receipt:canonical" {
		t.Fatalf("stale-lease recovery invoked charge %d times, output %q", called.Load(), output)
	}
}
