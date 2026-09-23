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

func TestNewUUIDFormat(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("newUUID() len = %d, want 36", len(id))
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("newUUID() = %q, invalid format", id)
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
	if err := engine.Init(context.Background()); err != nil {
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
		b.WaitEventFunc(func(sc *StepContext) (string, error) {
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
	if err := e.EmitEvent(context.Background(), "order.paid:first", true); err != nil {
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
	if err := e.EmitEvent(context.Background(), "order.paid:second", true); err != nil {
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
	time.Sleep(100 * time.Millisecond) // Allow LISTEN to acquire the only connection if enabled.
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
	query := fmt.Sprintf("UPDATE %s SET lease_owner = 'new_owner' WHERE id = $1 AND state = 'leased'", e.table("workflow_runs"))
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

func TestIntegrationLegacySchemaMigrationAndPartitionCatchup(t *testing.T) {
	e, pool := integrationEngine(t)
	ctx := context.Background()
	from := time.Now().UTC().AddDate(0, 14, 0)
	month := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	insert := fmt.Sprintf(
		"INSERT INTO %s (event_key, payload_json, created_at) VALUES ('retained', '{}'::jsonb, $1) RETURNING id",
		e.table("event_log"),
	)
	var id int64
	if err := pool.QueryRow(ctx, insert, month.Add(12*time.Hour)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.RegisterWorkflow("legacy", func(b *Builder) {
		b.Step("old", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	legacyID, err := e.Run(ctx, "legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing, unversioned installation with the baseline tables.
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"DROP TABLE %s; DROP INDEX %s; DROP INDEX %s; ALTER TABLE %s DROP COLUMN workflow_version, DROP COLUMN lease_failures",
		e.table("schema_migrations"), e.table("workflow_runs_waiting_deadline_idx"),
		e.table("workflow_runs_selective_ready_idx"), e.table("workflow_runs"),
	)); err != nil {
		t.Fatal(err)
	}
	migrationsDone := make(chan error, 2)
	for range 2 {
		go func() { migrationsDone <- e.ApplySchema(ctx) }()
	}
	for range 2 {
		if err := <-migrationsDone; err != nil {
			t.Fatalf("concurrent migration of unversioned installation: %v", err)
		}
	}
	if err := e.ApplySchema(ctx); err != nil {
		t.Fatalf("repeating migration: %v", err)
	}
	var indexFound bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", e.schema+".workflow_runs_waiting_deadline_idx").Scan(&indexFound); err != nil || !indexFound {
		t.Fatalf("missing waiting-event deadline index: found=%v error=%v", indexFound, err)
	}
	var migrations int
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", e.table("schema_migrations"))).Scan(&migrations); err != nil || migrations != 4 {
		t.Fatalf("migration history = %d, %v; want versions 1 through 4", migrations, err)
	}
	var restoredVersion, failures int
	versionQuery := fmt.Sprintf("SELECT workflow_version, lease_failures FROM %s WHERE id = $1", e.table("workflow_runs"))
	if err := pool.QueryRow(ctx, versionQuery, string(legacyID)).Scan(&restoredVersion, &failures); err != nil || restoredVersion != 1 || failures != 0 {
		t.Fatalf("legacy run migration: version=%d failures=%d error=%v", restoredVersion, failures, err)
	}

	if err := e.EnsurePartitions(ctx, 14); err != nil {
		t.Fatalf("creating partition with default rows: %v", err)
	}
	var partition string
	where := fmt.Sprintf("SELECT tableoid::regclass::text FROM %s WHERE id = $1", e.table("event_log"))
	if err := pool.QueryRow(ctx, where, id).Scan(&partition); err != nil {
		t.Fatalf("event lost during repartition: %v", err)
	}
	want := fmt.Sprintf("event_log_%04d%02d", month.Year(), month.Month())
	if !strings.HasSuffix(partition, want) {
		t.Fatalf("event remained in %s rather than %s", partition, want)
	}

	next := month.AddDate(0, 1, 0)
	if err := pool.QueryRow(ctx, insert, next.Add(12*time.Hour)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ddl, err := e.EventLogMonthlyPartitionsSQL(next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("generated DDL cannot repartition existing events: %v", err)
	}
	if err := pool.QueryRow(ctx, where, id).Scan(&partition); err != nil {
		t.Fatalf("event lost with generated DDL: %v", err)
	}
	want = fmt.Sprintf("event_log_%04d%02d", next.Year(), next.Month())
	if !strings.HasSuffix(partition, want) {
		t.Fatalf("event remained in %s rather than %s", partition, want)
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

func TestIntegrationStaleWaiterCannotWakeDifferentEvent(t *testing.T) {
	e, pool := integrationEngine(t)
	e.RegisterWorkflow("switch", func(b *Builder) {
		b.WaitEvent("current", time.Minute)
		b.Step("done", func(context.Context, *StepContext) (any, error) { return true, nil })
	})
	runID, err := e.Run(context.Background(), "switch", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startTestWorker(t, e)
	defer stop()
	waitRunState(t, e, runID, "waiting_event")
	stale := fmt.Sprintf("INSERT INTO %s (event_key, run_id) VALUES ('old', $1)", e.table("waiters"))
	if _, err := pool.Exec(context.Background(), stale, string(runID)); err != nil {
		t.Fatal(err)
	}
	if err := e.EmitEvent(context.Background(), "old", true); err != nil {
		t.Fatal(err)
	}
	var state, key string
	query := fmt.Sprintf("SELECT state, waiting_event_key FROM %s WHERE id = $1", e.table("workflow_runs"))
	if err := pool.QueryRow(context.Background(), query, string(runID)).Scan(&state, &key); err != nil {
		t.Fatal(err)
	}
	if state != "waiting_event" || key != "current" {
		t.Fatalf("stale event awakened current wait: state=%s key=%s", state, key)
	}
	if err := e.EmitEvent(context.Background(), "current", true); err != nil {
		t.Fatal(err)
	}
	waitRunState(t, e, runID, "completed")
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
		if err := e.EmitEvent(ctx, key, true); err != nil {
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

func TestIntegrationTimedOutWaitRemovesRegistration(t *testing.T) {
	e, pool := integrationEngine(t)
	e.RegisterWorkflow("timeout", func(b *Builder) {
		b.WaitEvent("forgotten", time.Minute)
		b.Step("after", func(context.Context, *StepContext) (any, error) { return true, nil })
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
	var waiters int
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE run_id = $1", e.table("waiters"))
	if err := pool.QueryRow(context.Background(), query, string(runID)).Scan(&waiters); err != nil || waiters != 0 {
		t.Fatalf("timeout left %d registrations: %v", waiters, err)
	}
	waitRunState(t, e, runID, "completed")
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
	if first != second || first == third || first != string(runID)+":0000:charge" || third != string(runID)+":0001:ship" {
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
