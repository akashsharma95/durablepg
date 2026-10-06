package durablepg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewRequiresDB(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error when DB is nil")
	}
}

// Run IDs are UUIDv7 so primary-key inserts stay clustered by time.
func TestNewUUIDFormat(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("newUUID() len = %d, want 36", len(id))
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("newUUID() = %q, invalid format", id)
	}
	if id[14] != '7' {
		t.Fatalf("newUUID() = %q, want version 7", id)
	}
	time.Sleep(2 * time.Millisecond)
	if later := newUUID(); later <= id {
		t.Fatalf("newUUID() not time-ordered: %q then %q", id, later)
	}
}

func TestBeginWorkerGuardsDoubleStart(t *testing.T) {
	e := &Engine{}
	if err := e.beginWorker(); err != nil {
		t.Fatalf("beginWorker() first call error = %v", err)
	}
	if err := e.beginWorker(); err == nil {
		t.Fatal("beginWorker() second call expected error")
	}
	e.endWorker()
	if err := e.beginWorker(); err != nil {
		t.Fatalf("beginWorker() after endWorker error = %v", err)
	}
}

// integrationEngine uses a disposable schema in an explicitly configured test database.
func integrationEngine(t *testing.T) (*Engine, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("DURABLEPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set DURABLEPG_TEST_DATABASE_URL to enable PostgreSQL integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(newUUID(), "-", "")
	engine, err := New(Config{
		DB: pool, Schema: schema, PollInterval: 10 * time.Millisecond,
		LeaseTTL: 2 * time.Second, HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+engine.qSchema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		pool.Close()
	})
	if err := engine.ApplySchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine, pool
}

func waitRunState(t *testing.T, e *Engine, runID WorkflowID, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var state string
		err := e.db.QueryRow(ctx, fmt.Sprintf("SELECT state FROM %s WHERE id = $1", e.table("workflow_runs")), string(runID)).Scan(&state)
		if err == nil && state == want {
			return
		}
		if state == "failed" && want != "failed" {
			t.Fatalf("run %s failed while awaiting %s", runID, want)
		}
		if ctx.Err() != nil {
			t.Fatalf("run %s did not reach %s (last state %s, query error %v)", runID, want, state, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startTestWorker(t *testing.T, e *Engine) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.StartWorker(ctx) }()
	return func() {
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
}

func TestIntegrationUnregisteredWorkflowRemainsReady(t *testing.T) {
	capable, pool := integrationEngine(t)
	incapable, err := New(Config{DB: pool, Schema: capable.schema, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	capable.RegisterWorkflow("only_capable", func(b *Builder) {
		b.Step("ok", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	runID, err := capable.Run(context.Background(), "only_capable", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, incapable)
	defer stop()
	time.Sleep(150 * time.Millisecond)
	var state string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT state FROM %s WHERE id = $1", capable.table("workflow_runs")), string(runID)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("unregistered workflow was claimed: %s", state)
	}
	stopCapable := startTestWorker(t, capable)
	defer stopCapable()
	waitRunState(t, capable, runID, "completed")
}

func TestIntegrationEventKeysArePerRun(t *testing.T) {
	e, _ := integrationEngine(t)
	e.RegisterWorkflow("orders", func(b *Builder) {
		b.WaitEventFunc("paid", func(sc *StepContext) (string, error) {
			var in struct {
				OrderID string `json:"order_id"`
			}
			if err := sc.DecodeInput(&in); err != nil {
				return "", err
			}
			return "order.paid:" + in.OrderID, nil
		}, time.Minute)
		b.Step("done", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	stop := startTestWorker(t, e)
	defer stop()
	first, err := e.Run(context.Background(), "orders", map[string]string{"order_id": "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Run(context.Background(), "orders", map[string]string{"order_id": "second"})
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, first, "waiting_event")
	waitRunState(t, e, second, "waiting_event")
	if _, err := e.EmitEvent(context.Background(), "order.paid:first", true); err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, first, "completed")
	var state string
	if err := e.db.QueryRow(context.Background(), fmt.Sprintf("SELECT state FROM %s WHERE id = $1", e.table("workflow_runs")), string(second)).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "waiting_event" {
		t.Fatalf("unrelated order resumed: %s", state)
	}
	if _, err := e.EmitEvent(context.Background(), "order.paid:second", true); err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, second, "completed")
}

func TestIntegrationStepPanicDoesNotStopWorker(t *testing.T) {
	e, _ := integrationEngine(t)
	e.RegisterWorkflow("panics", func(b *Builder) {
		b.Step("bad", func(context.Context, *StepContext) (any, error) { panic("boom") })
	})
	e.RegisterWorkflow("healthy", func(b *Builder) {
		b.Step("good", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	stop := startTestWorker(t, e)
	defer stop()
	bad, err := e.Run(context.Background(), "panics", nil, WithMaxAttempts(1))
	if err != nil {
		t.Fatal(err)
	}
	good, err := e.Run(context.Background(), "healthy", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, bad, "failed")
	waitRunState(t, e, good, "completed")
}

func TestIntegrationWorkerWithOneConnection(t *testing.T) {
	producer, _ := integrationEngine(t)
	config, err := pgxpool.ParseConfig(os.Getenv("DURABLEPG_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	worker, err := New(Config{DB: pool, Schema: producer.schema, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	build := func(b *Builder) {
		b.Step("done", func(context.Context, *StepContext) (any, error) { return true, nil })
	}
	producer.RegisterWorkflow("single_pool", build)
	worker.RegisterWorkflow("single_pool", build)
	stop := startTestWorker(t, worker)
	defer stop()
	runID, err := producer.Run(context.Background(), "single_pool", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, producer, runID, "completed")
}

func TestIntegrationShutdownDrainsActiveClaim(t *testing.T) {
	e, _ := integrationEngine(t)
	started := make(chan struct{})
	release := make(chan struct{})
	e.RegisterWorkflow("drain", func(b *Builder) {
		b.Step("work", func(context.Context, *StepContext) (any, error) {
			close(started)
			<-release
			return "saved", nil
		})
	})
	runID, err := e.Run(context.Background(), "drain", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.StartWorker(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("step did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not drain")
	}
	waitRunState(t, e, runID, "completed")
}

func TestIntegrationOldLeaseCannotCheckpoint(t *testing.T) {
	e, pool := integrationEngine(t)
	started := make(chan struct{})
	release := make(chan struct{})
	e.RegisterWorkflow("fenced", func(b *Builder) {
		b.Step("side_effect", func(context.Context, *StepContext) (any, error) {
			close(started)
			<-release
			return "stale", nil
		})
	})
	runID, err := e.Run(context.Background(), "fenced", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		stop()
		t.Fatal("step did not start")
	}
	query := fmt.Sprintf("UPDATE %s SET lease_token = gen_random_uuid() WHERE id = $1 AND state = 'leased'", e.table("workflow_runs"))
	if _, err := pool.Exec(context.Background(), query, string(runID)); err != nil {
		close(release)
		stop()
		t.Fatal(err)
	}
	close(release)
	time.Sleep(200 * time.Millisecond)
	stop()
	var checkpoints int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf("SELECT count(*) FROM %s WHERE run_id = $1", e.table("step_checkpoints")), string(runID)).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 {
		t.Fatalf("stale worker persisted %d checkpoints", checkpoints)
	}
}

// Every process calls ApplySchema on start, so repeated and concurrent calls
// must converge on one recorded migration.
func TestIntegrationApplySchemaIsIdempotentAndConcurrentSafe(t *testing.T) {
	e, pool := integrationEngine(t)
	ctx := context.Background()
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- e.ApplySchema(ctx) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent ApplySchema: %v", err)
		}
	}
	var versions []int32
	rows, err := pool.Query(ctx, fmt.Sprintf("SELECT version FROM %s ORDER BY version", e.table("schema_migrations")))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int32
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("migration history = %v, want [1]", versions)
	}
}

// A schema migrated by a newer library must be refused, not misused.
func TestIntegrationNewerSchemaVersionIsRejected(t *testing.T) {
	e, pool := integrationEngine(t)
	if _, err := pool.Exec(context.Background(), fmt.Sprintf("INSERT INTO %s (version) VALUES (2)", e.table("schema_migrations"))); err != nil {
		t.Fatal(err)
	}
	if err := e.ApplySchema(context.Background()); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("ApplySchema on newer schema = %v, want ErrUnsupportedSchema", err)
	}
}

func TestIntegrationVersionedRunsRequireCompatibleWorkers(t *testing.T) {
	producer, pool := integrationEngine(t)
	step := func(value string) func(*Builder) {
		return func(b *Builder) {
			b.Step("result", func(context.Context, *StepContext) (any, error) { return value, nil })
		}
	}
	producer.RegisterWorkflow("upgrade", step("old"))
	producer.RegisterWorkflowVersion("upgrade", 2, step("new"))
	older, err := New(Config{DB: pool, Schema: producer.schema, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	older.RegisterWorkflow("upgrade", step("old"))
	stopOld := startTestWorker(t, older)
	defer stopOld()

	newRun, err := producer.Run(context.Background(), "upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldRun, err := producer.Run(context.Background(), "upgrade", nil, WithWorkflowVersion(1))
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, producer, oldRun, "completed")
	var state string
	var version int
	query := fmt.Sprintf("SELECT state, workflow_version FROM %s WHERE id = $1", producer.table("workflow_runs"))
	if err := pool.QueryRow(context.Background(), query, string(newRun)).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || version != 2 {
		t.Fatalf("older worker claimed version %d run in state %s", version, state)
	}
	newer, err := New(Config{DB: pool, Schema: producer.schema, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	newer.RegisterWorkflowVersion("upgrade", 2, step("new"))
	stopNew := startTestWorker(t, newer)
	defer stopNew()
	waitRunState(t, producer, newRun, "completed")
}

// An event for one key must not wake a run waiting on another key, even one
// it previously waited on.
func TestIntegrationEventForPreviousKeyDoesNotWakeNextWait(t *testing.T) {
	e, _ := integrationEngine(t)
	e.RegisterWorkflow("two_waits", func(b *Builder) {
		b.WaitEvent("first", "key-a", time.Minute)
		b.WaitEvent("second", "key-c", time.Minute)
	})
	runID, err := e.Run(context.Background(), "two_waits", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	emit := func(key string, payload int, want int) {
		t.Helper()
		woken, err := e.EmitEvent(context.Background(), key, payload)
		if err != nil || woken != want {
			t.Fatalf("EmitEvent(%s) woke %d, %v; want %d", key, woken, err, want)
		}
	}
	waitRunState(t, e, runID, "waiting_event")
	emit("key-b", 0, 0)
	emit("key-a", 1, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := e.RunStatus(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if st.WaitingEventKey != nil && *st.WaitingEventKey == "key-c" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never waited on key-c: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	emit("key-a", 2, 0)
	emit("key-c", 3, 1)
	waitRunState(t, e, runID, "completed")
	var outcome struct {
		Received int `json:"received"`
	}
	if ok, err := e.RunOutput(context.Background(), runID, &outcome); err != nil || !ok || outcome.Received != 3 {
		t.Fatalf("output = %+v, %v, %v; want received 3", outcome, ok, err)
	}
}

func TestIntegrationExpiredLeasesHaveBoundedBackoff(t *testing.T) {
	e, pool := integrationEngine(t)
	e.RegisterWorkflow("crash_loop", func(b *Builder) {
		b.Step("work", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	runID, err := e.Run(context.Background(), "crash_loop", nil, WithMaxAttempts(2))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expire := fmt.Sprintf("UPDATE %s SET lease_until = now() - interval '1 second' WHERE id = $1", e.table("workflow_runs"))
	for claim := range 2 {
		runs, err := e.claimReadyRuns(ctx, 1)
		if err != nil || len(runs) != 1 {
			t.Fatalf("claim %d: runs=%d error=%v", claim, len(runs), err)
		}
		if _, err := pool.Exec(ctx, expire, string(runID)); err != nil {
			t.Fatal(err)
		}
		if err := e.recoverExpiredLeases(ctx); err != nil {
			t.Fatal(err)
		}
		var state string
		var failures, stepAttempts int
		var due bool
		query := fmt.Sprintf("SELECT state, lease_failures, attempt, next_run_at > now() FROM %s WHERE id = $1", e.table("workflow_runs"))
		if err := pool.QueryRow(ctx, query, string(runID)).Scan(&state, &failures, &stepAttempts, &due); err != nil {
			t.Fatal(err)
		}
		if failures != claim+1 || stepAttempts != 0 {
			t.Fatalf("recovery %d: failures=%d step attempts=%d", claim, failures, stepAttempts)
		}
		if claim == 0 && (state != "ready" || !due) {
			t.Fatalf("first recovery: state=%s backoff=%v", state, due)
		}
		if claim == 1 && state != "failed" {
			t.Fatalf("repeated lease expiry: state=%s, want failed", state)
		}
		if claim == 0 {
			advance := fmt.Sprintf("UPDATE %s SET next_run_at = now() - interval '1 second' WHERE id = $1", e.table("workflow_runs"))
			if _, err := pool.Exec(ctx, advance, string(runID)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestIntegrationPruneExpiredEventsPreservesLiveEvents(t *testing.T) {
	e, pool := integrationEngine(t)
	ctx := context.Background()
	for _, key := range []string{"expired", "live"} {
		if _, err := e.EmitEvent(ctx, key, true); err != nil {
			t.Fatal(err)
		}
	}
	expire := fmt.Sprintf("UPDATE %s SET expires_at = now() - interval '1 second' WHERE event_key = 'expired'", e.table("event_log"))
	if _, err := pool.Exec(ctx, expire); err != nil {
		t.Fatal(err)
	}
	if err := e.pruneExpiredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var live, expired int
	count := fmt.Sprintf("SELECT count(*) FILTER (WHERE event_key = 'live'), count(*) FILTER (WHERE event_key = 'expired') FROM %s", e.table("event_log"))
	if err := pool.QueryRow(ctx, count).Scan(&live, &expired); err != nil {
		t.Fatal(err)
	}
	if live != 1 || expired != 0 {
		t.Fatalf("event retention: live=%d expired=%d", live, expired)
	}
}

// A wait that times out records that outcome, so the next step can tell a
// timeout from a delivery.
func TestIntegrationTimedOutWaitRecordsTimedOut(t *testing.T) {
	e, pool := integrationEngine(t)
	e.RegisterWorkflow("timeout", func(b *Builder) {
		b.WaitEvent("payment", "forgotten", time.Minute)
		b.Step("after", func(_ context.Context, sc *StepContext) (any, error) {
			return sc.Event("payment", nil)
		})
	})
	runID, err := e.Run(context.Background(), "timeout", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	waitRunState(t, e, runID, "waiting_event")
	past := fmt.Sprintf("UPDATE %s SET waiting_deadline = now() - interval '1 second' WHERE id = $1", e.table("workflow_runs"))
	if _, err := pool.Exec(context.Background(), past, string(runID)); err != nil {
		t.Fatal(err)
	}
	if err := e.promoteTimedOutWaiters(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, runID, "completed")
	var received bool
	if ok, err := e.RunOutput(context.Background(), runID, &received); err != nil || !ok || received {
		t.Fatalf("after timeout, Event reported received=%v (ok=%v, err=%v)", received, ok, err)
	}
	if woken, err := e.EmitEvent(context.Background(), "forgotten", true); err != nil || woken != 0 {
		t.Fatalf("late event woke %d runs, %v", woken, err)
	}
}

func TestIntegrationExternalIdempotencyKeySurvivesRetry(t *testing.T) {
	e, _ := integrationEngine(t)
	keys := make(chan string, 3)
	attempts := 0
	e.RegisterWorkflow("payment", func(b *Builder) {
		b.Step("charge", func(_ context.Context, sc *StepContext) (any, error) {
			attempts++
			keys <- sc.IdempotencyKey()
			if attempts == 1 {
				return nil, errors.New("retry payment")
			}
			return true, nil
		})
		b.Step("ship", func(_ context.Context, sc *StepContext) (any, error) {
			keys <- sc.IdempotencyKey()
			return true, nil
		})
	})
	runID, err := e.Run(context.Background(), "payment", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	waitRunState(t, e, runID, "completed")
	first, second, third := <-keys, <-keys, <-keys
	if first != second || first == third || first != string(runID)+":0:charge" || third != string(runID)+":1:ship" {
		t.Fatalf("idempotency keys across retry and steps: %q %q %q", first, second, third)
	}
}

func TestIntegrationWaitForIdleIncludesUncooperativeStep(t *testing.T) {
	e, _ := integrationEngine(t)
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	e.RegisterWorkflow("slow_stop", func(b *Builder) {
		b.Step("block", func(context.Context, *StepContext) (any, error) {
			close(started)
			<-release
			return true, nil
		})
	})
	if _, err := e.Run(context.Background(), "slow_stop", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.StartWorker(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("step did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("bounded shutdown did not return")
	}
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := e.WaitForIdle(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("noncooperative step was considered idle: %v", err)
	}
	close(release)
	released = true
	joined, finish := context.WithTimeout(context.Background(), 5*time.Second)
	defer finish()
	if err := e.WaitForIdle(joined); err != nil {
		t.Fatalf("step did not quiesce: %v", err)
	}
}

// Cancelling a running run must cancel its step context promptly and fence
// its writes; cancelling again reports the terminal state.
func TestIntegrationCancelStopsRunningStep(t *testing.T) {
	e, _ := integrationEngine(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	e.RegisterWorkflow("cancel_me", func(b *Builder) {
		b.Step("block", func(ctx context.Context, _ *StepContext) (any, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		})
	})
	stop := startTestWorker(t, e)
	defer stop()
	runID, err := e.Run(context.Background(), "cancel_me", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if out, err := e.Cancel(context.Background(), runID); err != nil || !out.Cancelled {
		t.Fatalf("Cancel = %+v, %v", out, err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled step context was not canceled")
	}
	st, err := e.RunStatus(context.Background(), runID)
	if err != nil || st.State != RunCancelled {
		t.Fatalf("status after cancel = %+v, %v", st, err)
	}
	if out, err := e.Cancel(context.Background(), runID); err != nil || out.Cancelled || out.State != RunCancelled {
		t.Fatalf("second Cancel = %+v, %v", out, err)
	}
	if _, err := e.Cancel(context.Background(), WorkflowID(newUUID())); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Cancel of unknown run = %v, want ErrRunNotFound", err)
	}
}

// Cancelling a waiting run removes it from event delivery.
func TestIntegrationCancelledWaitingRunIsNotWoken(t *testing.T) {
	e, _ := integrationEngine(t)
	e.RegisterWorkflow("wait_cancel", func(b *Builder) {
		b.WaitEvent("signal", "cancel-key", time.Minute)
	})
	stop := startTestWorker(t, e)
	defer stop()
	runID, err := e.Run(context.Background(), "wait_cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, runID, "waiting_event")
	if out, err := e.Cancel(context.Background(), runID); err != nil || !out.Cancelled {
		t.Fatalf("Cancel = %+v, %v", out, err)
	}
	if woken, err := e.EmitEvent(context.Background(), "cancel-key", 1); err != nil || woken != 0 {
		t.Fatalf("event woke %d cancelled runs, %v", woken, err)
	}
}

// Notifications drive dispatch: with polling effectively disabled, a new run
// still starts promptly through the listener's dedicated connection.
func TestIntegrationNotificationWakesIdleWorker(t *testing.T) {
	producer, pool := integrationEngine(t)
	worker, err := New(Config{DB: pool, Schema: producer.schema, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	build := func(b *Builder) {
		b.Step("done", func(context.Context, *StepContext) (any, error) { return true, nil })
	}
	producer.RegisterWorkflow("notified", build)
	worker.RegisterWorkflow("notified", build)
	stop := startTestWorker(t, worker)
	defer stop()
	// Let the first poll and listener setup pass.
	time.Sleep(300 * time.Millisecond)
	runID, err := producer.Run(context.Background(), "notified", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, producer, runID, "completed")
}

// The notify channel is per schema, so engines on different schemas in one
// database do not wake each other.
func TestIntegrationNotificationsAreScopedToSchema(t *testing.T) {
	e, pool := integrationEngine(t)
	e.RegisterWorkflow("scoped", func(b *Builder) {
		b.Step("done", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	listen := func(channel string) *pgxpool.Conn {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(context.Background(), "LISTEN "+quoteIdentifier(channel)); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	own := listen("durablepg_" + e.schema)
	defer own.Release()
	foreign := listen("durablepg_other_" + e.schema)
	defer foreign.Release()
	if _, err := e.Run(context.Background(), "scoped", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	note, err := own.Conn().WaitForNotification(ctx)
	if err != nil || note.Payload != "default" {
		t.Fatalf("own schema notification = %+v, %v; want queue payload", note, err)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	if stray, err := foreign.Conn().WaitForNotification(short); err == nil {
		t.Fatalf("other schema was notified: %+v", stray)
	}
}
