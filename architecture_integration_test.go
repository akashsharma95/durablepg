package durablepg

import (
	"context"
	"encoding/json"
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
	cfg, err := pgxpool.ParseConfig(os.Getenv("DURABLEPG_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
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

// The first worker misses the event, but its lookup cannot return until the
// producer has committed. The first worker then loses its next completion (or
// the old post-commit race check) as if interrupted. A restarted worker must
// still finish without another emission, including when no waiter existed at
// the moment EmitEvent scanned registrations.
func TestIntegrationEventBetweenLookupAndWaitSurvivesRestart(t *testing.T) {
	producer, pool := integrationEngine(t)
	const key = "event-during-registration"
	build := func(b *Builder) { b.WaitEvent(key, time.Hour) }
	producer.RegisterWorkflow("event_restart", build)

	lookupDone := make(chan struct{})
	resumeLookup := make(chan struct{})
	interrupted := make(chan struct{})
	resumeInterrupt := make(chan struct{})
	lookupRelease := sync.OnceFunc(func() { close(resumeLookup) })
	interruptRelease := sync.OnceFunc(func() { close(resumeInterrupt) })
	var firstLookup atomic.Bool
	type lookupMark struct{}
	trace := architectureQueryTrace{
		start: func(ctx context.Context, data pgx.TraceQueryStartData) context.Context {
			if strings.Contains(data.SQL, "SELECT EXISTS") && strings.Contains(data.SQL, "event_log") && firstLookup.CompareAndSwap(false, true) {
				return context.WithValue(ctx, lookupMark{}, true)
			}
			// Force a crash at the old, post-commit rescue path. The new path
			// has already advanced its cursor and instead reaches completion.
			if strings.Contains(data.SQL, "SET state = 'completed'") ||
				(strings.Contains(data.SQL, "UPDATE ") && strings.Contains(data.SQL, "event_log") && strings.Contains(data.SQL, "EXISTS (")) {
				close(interrupted)
				<-resumeInterrupt
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			}
			return ctx
		},
		end: func(ctx context.Context, _ pgx.TraceQueryEndData) {
			if ctx.Value(lookupMark{}) != nil {
				close(lookupDone)
				<-resumeLookup
			}
		},
	}
	first, firstPool := architectureWorker(t, producer, trace)
	first.RegisterWorkflow("event_restart", build)
	defer firstPool.Close()

	runID, err := producer.Run(context.Background(), "event_restart", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.StartWorker(ctx) }()
	stopped := false
	defer func() {
		lookupRelease()
		interruptRelease()
		if !stopped {
			architectureStopWorker(t, cancel, done)
		}
	}()

	architectureSignal(t, lookupDone, "initial event miss")
	if err := producer.EmitEvent(context.Background(), key, "retained"); err != nil {
		t.Fatal(err)
	}
	lookupRelease()
	architectureSignal(t, interrupted, "interrupted first worker")
	cancel()
	interruptRelease()
	architectureStopWorker(t, cancel, done)
	stopped = true

	var state string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT state FROM %s WHERE id = $1", producer.table("workflow_runs")), string(runID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "completed" {
		t.Fatal("first worker completed despite the injected interruption")
	}
	// A process lost while its completion query is in flight leaves a lease.
	// Expire and recover it explicitly instead of waiting for the test worker's
	// two-second TTL plus the next periodic maintenance pass.
	if state == "leased" {
		expire := fmt.Sprintf("UPDATE %s SET lease_until = now() - interval '1 second' WHERE id = $1", producer.table("workflow_runs"))
		if _, err := pool.Exec(context.Background(), expire, string(runID)); err != nil {
			t.Fatal(err)
		}
		if err := producer.recoverExpiredLeases(context.Background()); err != nil {
			t.Fatal(err)
		}
		due := fmt.Sprintf("UPDATE %s SET next_run_at = now() - interval '1 second' WHERE id = $1 AND state = 'ready'", producer.table("workflow_runs"))
		if _, err := pool.Exec(context.Background(), due, string(runID)); err != nil {
			t.Fatal(err)
		}
	}

	restarted := startTestWorker(t, producer)
	defer restarted()
	waitRunState(t, producer, runID, "completed")
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
	query := fmt.Sprintf("INSERT INTO %s (run_id, step_key, value_json) VALUES ($1, $2, $3::jsonb)", e.table("step_checkpoints"))
	if _, err := pool.Exec(context.Background(), query, string(runID), formatStepKey(0, "charged"), `"canonical"`); err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	waitRunState(t, e, runID, "completed")
	var output []byte
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT output_json FROM %s WHERE id = $1", e.table("workflow_runs")), string(runID)).Scan(&output); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 0 || result["charged"] != "canonical" || result["receipt"] != "receipt:canonical" {
		t.Fatalf("checkpoint recovery invoked callback %d times, output %s", called.Load(), output)
	}
}

// Pause after the database has committed the checkpoint but before the old
// owner proceeds. Replacing the lease must fence that owner, and recovery of
// the committed step must produce its canonical result without reexecution.
func TestIntegrationAtomicCheckpointCommitFencesStaleOwner(t *testing.T) {
	producer, pool := integrationEngine(t)
	var called atomic.Int32
	build := func(b *Builder) {
		b.Step("charge", func(context.Context, *StepContext) (any, error) {
			called.Add(1)
			return "canonical", nil
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
			if strings.Contains(data.SQL, "step_checkpoints") && strings.Contains(data.SQL, "workflow_runs") &&
				strings.Contains(data.SQL, "INSERT") && firstCommit.CompareAndSwap(false, true) {
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
	query := fmt.Sprintf(`SELECT wr.step_index, cp.value_json FROM %s wr JOIN %s cp ON cp.run_id = wr.id WHERE wr.id = $1 AND cp.step_key = $2`,
		producer.table("workflow_runs"), producer.table("step_checkpoints"))
	if err := pool.QueryRow(context.Background(), query, string(runID), formatStepKey(0, "charge")).Scan(&cursor, &canonical); err != nil {
		t.Fatal(err)
	}
	if cursor != 1 || canonical != `"canonical"` {
		t.Fatalf("checkpoint and cursor did not commit atomically: cursor=%d value=%s", cursor, canonical)
	}
	steal := fmt.Sprintf(`UPDATE %s SET lease_owner = 'replacement', lease_until = clock_timestamp() - INTERVAL '1 second' WHERE id = $1 AND state = 'leased'`, producer.table("workflow_runs"))
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
	var output []byte
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT output_json FROM %s WHERE id = $1", producer.table("workflow_runs")), string(runID)).Scan(&output); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 || result["charge"] != "canonical" {
		t.Fatalf("stale-lease recovery invoked charge %d times, output %s", called.Load(), output)
	}
}
