package durablepg

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/peterldowns/pgtestdb"
)

const benchmarkDatabaseURLEnv = "DURABLEPG_TEST_DATABASE_URL"
const benchmarkSchemaName = "durable"

// These benchmarks use a real PostgreSQL instance because the meaningful
// performance questions for this library are end-to-end:
// 1. producer throughput for inserting new workflow runs,
// 2. fully durable completion throughput through claim/checkpoint/complete, and
// 3. wake-up latency for event-driven workflows.

func BenchmarkEnqueue(b *testing.B) {
	h := newBenchmarkHarness(b, "enqueue", runtime.GOMAXPROCS(0))
	h.engine.RegisterWorkflow("enqueue_bench", func(wf *Builder) {
		wf.Step("noop", func(context.Context, *StepContext) (any, error) {
			return map[string]any{"ok": true}, nil
		})
	})

	b.ReportAllocs()
	started := time.Now()
	b.ResetTimer()

	var once sync.Once
	var benchErr error
	var failed atomic.Bool

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if failed.Load() {
				return
			}
			if _, err := h.engine.Run(context.Background(), "enqueue_bench", map[string]any{"ok": true}); err != nil {
				once.Do(func() {
					benchErr = err
					failed.Store(true)
				})
				return
			}
		}
	})

	b.StopTimer()
	if benchErr != nil {
		b.Fatalf("enqueue benchmark failed: %v", benchErr)
	}

	elapsed := time.Since(started)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "runs/s")
}

func BenchmarkE2ESingleStep(b *testing.B) {
	workerCounts := []int{1}
	if workers := max(1, runtime.GOMAXPROCS(0)); workers != 1 {
		workerCounts = append(workerCounts, workers)
	}

	for _, workers := range workerCounts {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			h := newBenchmarkHarness(b, "e2e_single_step", workers)
			h.engine.RegisterWorkflow("single_step_bench", func(wf *Builder) {
				wf.Step("process", func(context.Context, *StepContext) (any, error) {
					return map[string]any{"ok": true}, nil
				})
			})
			h.startWorker(b)

			b.ReportAllocs()
			started := time.Now()
			b.ResetTimer()

			var once sync.Once
			var benchErr error
			var enqueued atomic.Int64
			var failed atomic.Bool

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if failed.Load() {
						return
					}
					if _, err := h.engine.Run(context.Background(), "single_step_bench", map[string]any{"n": 1}); err != nil {
						once.Do(func() {
							benchErr = err
							failed.Store(true)
						})
						return
					}
					enqueued.Add(1)
				}
			})

			if benchErr != nil {
				b.StopTimer()
				b.Fatalf("e2e single-step enqueue failed: %v", benchErr)
			}

			if err := h.waitForCompletedRuns(b, int(enqueued.Load()), 30*time.Second); err != nil {
				b.StopTimer()
				b.Fatalf("e2e single-step benchmark did not finish: %v", err)
			}

			b.StopTimer()
			elapsed := time.Since(started)
			b.ReportMetric(float64(enqueued.Load())/elapsed.Seconds(), "workflows/s")
		})
	}
}

func BenchmarkE2EEventWakeLatency(b *testing.B) {
	h := newBenchmarkHarness(b, "e2e_event_wake", max(1, runtime.GOMAXPROCS(0)))
	const eventKey = "event_wake_bench"

	h.engine.RegisterWorkflow("event_wake_bench", func(wf *Builder) {
		wf.WaitEvent(eventKey, time.Minute)
		wf.Step("resume", func(context.Context, *StepContext) (any, error) {
			return map[string]any{"resumed": true}, nil
		})
	})
	h.startWorker(b)

	var totalWake time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()

	for i := 0; i < b.N; i++ {
		if err := h.clearEventKey(context.Background(), eventKey); err != nil {
			b.Fatalf("event wake benchmark cleanup failed: %v", err)
		}

		b.StartTimer()
		runID, err := h.engine.Run(context.Background(), "event_wake_bench", map[string]any{"iteration": i})
		if err != nil {
			b.StopTimer()
			b.Fatalf("event wake benchmark enqueue failed: %v", err)
		}
		if err := h.waitForRunState(runID, "waiting_event", 10*time.Second); err != nil {
			b.StopTimer()
			b.Fatalf("event wake benchmark did not reach waiting state: %v", err)
		}

		wakeStarted := time.Now()
		if err := h.engine.EmitEvent(context.Background(), eventKey, map[string]any{"iteration": i}); err != nil {
			b.StopTimer()
			b.Fatalf("event wake benchmark emit failed: %v", err)
		}
		if err := h.waitForRunState(runID, "completed", 10*time.Second); err != nil {
			b.StopTimer()
			b.Fatalf("event wake benchmark did not complete: %v", err)
		}
		totalWake += time.Since(wakeStarted)
		b.StopTimer()
	}

	if b.N > 0 {
		b.ReportMetric(float64(totalWake.Microseconds())/float64(b.N), "wake-us/op")
	}
}

type benchmarkHarness struct {
	pool   *pgxpool.Pool
	engine *Engine
}

func newBenchmarkHarness(tb testing.TB, prefix string, maxConcurrency int) *benchmarkHarness {
	tb.Helper()

	pool := benchmarkPool(tb)

	engine, err := New(Config{
		DB:                pool,
		Schema:            benchmarkSchemaName,
		MaxConcurrency:    maxConcurrency,
		PollInterval:      10 * time.Millisecond,
		LeaseTTL:          5 * time.Second,
		HeartbeatInterval: time.Second,
	})
	if err != nil {
		tb.Fatalf("new benchmark engine: %v", err)
	}

	tb.Cleanup(func() {
		pool.Close()
	})

	return &benchmarkHarness{
		pool:   pool,
		engine: engine,
	}
}

func (h *benchmarkHarness) startWorker(tb testing.TB) {
	tb.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.engine.StartWorker(ctx)
	}()

	tb.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && err != context.Canceled {
				tb.Fatalf("worker shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			tb.Fatalf("worker shutdown timed out")
		}
	})
}

func (h *benchmarkHarness) waitForCompletedRuns(tb testing.TB, want int, timeout time.Duration) error {
	tb.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		completed, failed, err := h.countRunStates(ctx)
		if err != nil {
			return err
		}
		if failed > 0 {
			return fmt.Errorf("observed %d failed runs", failed)
		}
		if completed >= want {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %d completed runs (got %d)", want, completed)
		case <-ticker.C:
		}
	}
}

func (h *benchmarkHarness) waitForRunState(runID WorkflowID, want string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		state, err := h.runState(ctx, runID)
		if err != nil {
			return err
		}
		if state == want {
			return nil
		}
		if state == "failed" {
			return fmt.Errorf("run %s entered failed state", runID)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for run %s to reach %q (last state %q)", runID, want, state)
		case <-ticker.C:
		}
	}
}

func (h *benchmarkHarness) clearEventKey(ctx context.Context, key string) error {
	query := fmt.Sprintf(`DELETE FROM %s WHERE event_key = $1;`, h.engine.table("event_log"))
	_, err := h.pool.Exec(ctx, query, key)
	if err != nil {
		return fmt.Errorf("clear event key %q: %w", key, err)
	}
	return nil
}

func (h *benchmarkHarness) countRunStates(ctx context.Context) (completed int, failed int, err error) {
	query := fmt.Sprintf(`
SELECT
	COUNT(*) FILTER (WHERE state = 'completed') AS completed,
	COUNT(*) FILTER (WHERE state = 'failed') AS failed
FROM %s;
`, h.engine.table("workflow_runs"))
	err = h.pool.QueryRow(ctx, query).Scan(&completed, &failed)
	if err != nil {
		return 0, 0, fmt.Errorf("count run states: %w", err)
	}
	return completed, failed, nil
}

func (h *benchmarkHarness) runState(ctx context.Context, runID WorkflowID) (string, error) {
	query := fmt.Sprintf(`SELECT state FROM %s WHERE id = $1;`, h.engine.table("workflow_runs"))
	var state string
	if err := h.pool.QueryRow(ctx, query, string(runID)).Scan(&state); err != nil {
		return "", fmt.Errorf("lookup run %s state: %w", runID, err)
	}
	return state, nil
}

func benchmarkPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminConf := benchmarkAdminConfig(tb)
	testConf := pgtestdb.Custom(tb, adminConf, benchmarkMigrator{
		schema:      benchmarkSchemaName,
		partitions:  12,
		templateTag: "durablepg-bench-v1",
	})

	pool, err := pgxpool.New(ctx, testConf.URL())
	if err != nil {
		tb.Fatalf("connect benchmark database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		tb.Fatalf("ping benchmark database: %v", err)
	}
	return pool
}

func benchmarkAdminConfig(tb testing.TB) pgtestdb.Config {
	tb.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(benchmarkDatabaseURLEnv))
	if databaseURL == "" {
		databaseURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if databaseURL == "" {
		tb.Skipf("set %s or DATABASE_URL to point at a dedicated admin Postgres for pgtestdb", benchmarkDatabaseURLEnv)
	}

	u, err := url.Parse(databaseURL)
	if err != nil {
		tb.Fatalf("parse benchmark database URL: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		tb.Fatalf("unsupported benchmark database URL scheme %q", u.Scheme)
	}

	password, _ := u.User.Password()
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" {
		database = "postgres"
	}

	return pgtestdb.Config{
		DriverName:                "pgx",
		Host:                      u.Hostname(),
		Port:                      defaultString(u.Port(), "5432"),
		User:                      u.User.Username(),
		Password:                  password,
		Database:                  database,
		Options:                   u.RawQuery,
		ForceTerminateConnections: true,
	}
}

type benchmarkMigrator struct {
	schema      string
	partitions  int
	templateTag string
}

func (m benchmarkMigrator) Hash() (string, error) {
	engine := &Engine{
		schema:  m.schema,
		qSchema: quoteIdentifier(m.schema),
	}

	sum := sha256.Sum256([]byte(engine.SchemaSQL() + fmt.Sprintf("|partitions=%d|tag=%s", m.partitions, m.templateTag)))
	return hex.EncodeToString(sum[:]), nil
}

func (m benchmarkMigrator) Migrate(ctx context.Context, db *sql.DB, _ pgtestdb.Config) error {
	engine := &Engine{
		schema:  m.schema,
		qSchema: quoteIdentifier(m.schema),
	}

	if _, err := db.ExecContext(ctx, engine.SchemaSQL()); err != nil {
		return fmt.Errorf("apply durablepg schema: %w", err)
	}
	if m.partitions > 0 {
		query := fmt.Sprintf("SELECT %s.ensure_event_log_partitions($1)", engine.qSchema)
		if _, err := db.ExecContext(ctx, query, m.partitions); err != nil {
			return fmt.Errorf("create durablepg event partitions: %w", err)
		}
	}
	return nil
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
