// growthbench measures workflow reads and vacuum behavior in a dedicated PostgreSQL database.
// It creates its own schema and requires pgstattuple. Never point it at production.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	durablepg "github.com/akashsharma95/durablepg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = "growthbench"

const claimProbe = `SELECT id FROM growthbench.workflow_runs
WHERE queue='probe' AND workflow_name='probe' AND workflow_version=1
  AND state='ready' AND next_run_at<=now() AND attempt<max_attempts
ORDER BY next_run_at,id LIMIT 16 FOR UPDATE SKIP LOCKED`

const recoveryProbe = `SELECT id FROM growthbench.workflow_runs
WHERE state='leased' AND lease_until<statement_timestamp()
ORDER BY lease_until,id LIMIT 1024 FOR UPDATE SKIP LOCKED`

type queryStart struct {
	at    time.Time
	stage int32
}

type claimTrace struct {
	stage   atomic.Int32
	mu      sync.Mutex
	byStage map[int32][]time.Duration
}

type traceKey struct{}

func (t *claimTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "WITH picked AS") || !strings.Contains(data.SQL, "RETURNING wr.id") || len(data.Args) == 0 || data.Args[0] != "short" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, queryStart{at: time.Now(), stage: t.stage.Load()})
}

func (t *claimTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(traceKey{}).(queryStart)
	if !ok || start.stage < 0 || data.Err != nil || data.CommandTag.RowsAffected() == 0 {
		return
	}
	t.mu.Lock()
	t.byStage[start.stage] = append(t.byStage[start.stage], time.Since(start.at))
	t.mu.Unlock()
}

func (t *claimTrace) percentiles(stage int32) (int, time.Duration, time.Duration, time.Duration) {
	t.mu.Lock()
	durations := append([]time.Duration(nil), t.byStage[stage]...)
	t.mu.Unlock()
	if len(durations) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return len(durations), durations[len(durations)/2], durations[(95*len(durations)+99)/100-1], durations[(99*len(durations)+99)/100-1]
}

type plan struct {
	Plan struct {
		SharedHit  int64 `json:"Shared Hit Blocks"`
		SharedRead int64 `json:"Shared Read Blocks"`
	} `json:"Plan"`
	ExecutionMS float64 `json:"Execution Time"`
}

func explain(ctx context.Context, db *pgxpool.Pool, query string) (plan, error) {
	var raw []byte
	if err := db.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query).Scan(&raw); err != nil {
		return plan{}, err
	}
	var plans []plan
	if err := json.Unmarshal(raw, &plans); err != nil {
		return plan{}, err
	}
	if len(plans) != 1 {
		return plan{}, fmt.Errorf("expected one EXPLAIN plan, got %d", len(plans))
	}
	return plans[0], nil
}

type measurement struct {
	Live, DeadEstimate, Updates, HOT, AutoCount, HeapBytes, IndexBytes int64
	HeapDead, LeaseDead, LeaseBytes                                    int64
	AutoMS, WALBytes, AutoReadBytes, AutoWriteBytes                    float64
	ShortCompleted, ShortFailed, LongLeased, LongCompleted             int64
	Claim, Recovery                                                    plan
}

func measure(ctx context.Context, db *pgxpool.Pool) (measurement, error) {
	var m measurement
	err := db.QueryRow(ctx, `SELECT n_live_tup,n_dead_tup,n_tup_upd,n_tup_hot_upd,autovacuum_count,
		total_autovacuum_time,pg_relation_size(relid),pg_indexes_size(relid)
		FROM pg_stat_user_tables WHERE schemaname=$1 AND relname='workflow_runs'`, schema).Scan(
		&m.Live, &m.DeadEstimate, &m.Updates, &m.HOT, &m.AutoCount, &m.AutoMS, &m.HeapBytes, &m.IndexBytes)
	if err != nil {
		return m, err
	}
	if err = db.QueryRow(ctx, `SELECT dead_tuple_count FROM pgstattuple_approx('growthbench.workflow_runs'::regclass)`).Scan(&m.HeapDead); err != nil {
		return m, err
	}
	if err = db.QueryRow(ctx, `SELECT dead_tuple_count,table_len FROM pgstattuple('growthbench.workflow_runs_leased_idx'::regclass)`).Scan(&m.LeaseDead, &m.LeaseBytes); err != nil {
		return m, err
	}
	if err = db.QueryRow(ctx, `SELECT wal_bytes::float8 FROM pg_stat_wal`).Scan(&m.WALBytes); err != nil {
		return m, err
	}
	if err = db.QueryRow(ctx, `SELECT coalesce(sum(read_bytes),0)::float8,coalesce(sum(write_bytes),0)::float8
		FROM pg_stat_io WHERE backend_type='autovacuum worker' AND object='relation'`).Scan(&m.AutoReadBytes, &m.AutoWriteBytes); err != nil {
		return m, err
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE workflow_name='short' AND state='completed'),
		count(*) FILTER (WHERE workflow_name='short' AND state='failed'),
		count(*) FILTER (WHERE workflow_name='long' AND state='leased'),
		count(*) FILTER (WHERE workflow_name='long' AND state='completed')
		FROM growthbench.workflow_runs WHERE workflow_name IN ('short','long')`).Scan(&m.ShortCompleted, &m.ShortFailed, &m.LongLeased, &m.LongCompleted); err != nil {
		return m, err
	}
	if m.Claim, err = explain(ctx, db, claimProbe); err != nil {
		return m, err
	}
	if m.Recovery, err = explain(ctx, db, recoveryProbe); err != nil {
		return m, err
	}
	return m, nil
}

func seedHistory(ctx context.Context, db *pgxpool.Pool, from, to int) error {
	for first := from; first <= to; first += 100000 {
		last := min(first+99999, to)
		_, err := db.Exec(ctx, `INSERT INTO growthbench.workflow_runs
			(id,workflow_name,workflow_version,queue,state,step_index,attempt,max_attempts,next_run_at,input_json,output_json,idempotency_key)
			SELECT 'history-'||g,'history',1,'short','completed',1,0,25,now(),'{}'::jsonb,'true'::jsonb,'dedup-'||g
			FROM generate_series($1::int,$2::int) g`, first, last)
		if err != nil {
			return err
		}
		_, err = db.Exec(ctx, `INSERT INTO growthbench.step_checkpoints (run_id,step_key,value_json)
			SELECT 'history-'||g,'process','true'::jsonb FROM generate_series($1::int,$2::int) g`, first, last)
		if err != nil {
			return err
		}
	}
	_, err := db.Exec(ctx, "ANALYZE growthbench.workflow_runs")
	return err
}

func await(ctx context.Context, label string, check func() (bool, error)) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", label, ctx.Err())
		case <-ticker.C:
		}
	}
}

func parseHistory(s string) ([]int, error) {
	var targets []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || (len(targets) > 0 && n <= targets[len(targets)-1]) {
			return nil, fmt.Errorf("history targets must be strictly increasing nonnegative integers: %q", s)
		}
		targets = append(targets, n)
	}
	if len(targets) < 2 || targets[0] != 0 {
		return nil, errors.New("history targets must start at zero and contain at least two milestones")
	}
	return targets, nil
}

func run() error {
	url := flag.String("url", os.Getenv("DURABLEPG_GROWTH_DATABASE_URL"), "disposable PostgreSQL 18 database URL")
	profile := flag.String("profile", "default", "vacuum settings: default or tuned")
	history := flag.String("history", "0,100000,300000", "cumulative terminal-row milestones")
	stageDuration := flag.Duration("stage", 45*time.Second, "measurement time per milestone (>= 30s)")
	rate := flag.Int("rate", 100, "short workflows enqueued per second")
	active := flag.Int("active", 64, "long-lived concurrent claims")
	flag.Parse()
	if *url == "" || (*profile != "default" && *profile != "tuned") || *stageDuration < 30*time.Second || *rate < 1 || *rate > 10000 || *active < 1 || *active > 1000 {
		return errors.New("provide a disposable -url, -profile=default|tuned, -stage>=30s, -rate=1..10000, and -active=1..1000")
	}
	targets, err := parseHistory(*history)
	if err != nil {
		return err
	}
	ctx := context.Background()
	trace := &claimTrace{byStage: make(map[int32][]time.Duration)}
	trace.stage.Store(-1)
	cfg, err := pgxpool.ParseConfig(*url)
	if err != nil {
		return err
	}
	cfg.MaxConns = 64
	cfg.ConnConfig.Tracer = trace
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.Ping(ctx); err != nil {
		return err
	}
	var version int
	if err = db.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		return err
	}
	if version < 180000 {
		return fmt.Errorf("requires PostgreSQL 18 for vacuum timing metrics, got %d", version)
	}
	var existing *string
	if err = db.QueryRow(ctx, "SELECT to_regnamespace($1)::text", schema).Scan(&existing); err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("schema %s already exists; use a new disposable database", schema)
	}
	short, err := durablepg.New(durablepg.Config{DB: db, Schema: schema, Queue: "short", MaxConcurrency: 8, PollInterval: 10 * time.Millisecond})
	if err != nil {
		return err
	}
	long, err := durablepg.New(durablepg.Config{DB: db, Schema: schema, Queue: "long", MaxConcurrency: *active, PollInterval: 10 * time.Millisecond})
	if err != nil {
		return err
	}
	if err = short.Init(ctx); err != nil {
		return err
	}
	if _, err = db.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pgstattuple"); err != nil {
		return err
	}
	if *profile == "tuned" {
		_, err = db.Exec(ctx, `ALTER TABLE growthbench.workflow_runs SET (autovacuum_vacuum_scale_factor=0.005,autovacuum_vacuum_threshold=200,vacuum_index_cleanup=on)`)
		if err != nil {
			return err
		}
	}
	short.RegisterWorkflow("short", func(b *durablepg.Builder) {
		b.Step("process", func(context.Context, *durablepg.StepContext) (any, error) { return true, nil })
	})
	var longStarted atomic.Int64
	release := make(chan struct{})
	releaseClaim := sync.OnceFunc(func() { close(release) })
	defer releaseClaim()
	long.RegisterWorkflow("long", func(b *durablepg.Builder) {
		b.Step("process", func(c context.Context, _ *durablepg.StepContext) (any, error) {
			longStarted.Add(1)
			select {
			case <-release:
				return true, nil
			case <-c.Done():
				return nil, c.Err()
			}
		})
	})
	_, err = db.Exec(ctx, `INSERT INTO growthbench.workflow_runs (id,workflow_name,workflow_version,queue,state,step_index,attempt,max_attempts,next_run_at,input_json)
		SELECT 'probe-'||g,'probe',1,'probe','ready',0,0,25,now()-interval '1 minute','{}'::jsonb FROM generate_series(1,16) g`)
	if err != nil {
		return err
	}
	for i := 0; i < *active; i++ {
		if _, err = long.Run(ctx, "long", nil); err != nil {
			return err
		}
	}
	workersCtx, cancelWorkers := context.WithCancel(ctx)
	workers := make(chan error, 2)
	go func() { workers <- long.StartWorker(workersCtx) }()
	go func() { workers <- short.StartWorker(workersCtx) }()
	stoppedWorkers := 0
	defer func() {
		releaseClaim()
		cancelWorkers()
		for stoppedWorkers < 2 {
			<-workers
			stoppedWorkers++
		}
	}()
	startCtx, stopStart := context.WithTimeout(ctx, 30*time.Second)
	err = await(startCtx, "active long claims", func() (bool, error) { return longStarted.Load() == int64(*active), nil })
	stopStart()
	if err != nil {
		return err
	}
	producerStop := make(chan struct{})
	stopProducer := sync.OnceFunc(func() { close(producerStop) })
	defer stopProducer()
	var enqueued atomic.Int64
	producerDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(*rate))
		defer ticker.Stop()
		for {
			select {
			case <-producerStop:
				producerDone <- nil
				return
			case <-ticker.C:
				// Do not cancel an in-flight INSERT at shutdown: it could commit
				// without being counted, making the final drain ambiguous.
				if _, err := short.Run(ctx, "short", nil); err != nil {
					producerDone <- err
					return
				}
				enqueued.Add(1)
			}
		}
	}()
	fmt.Printf("CONFIG profile=%s postgres=%d rate=%d/s active=%d heartbeat=10s lease=30s stage=%s history=%v\n", *profile, version, *rate, *active, *stageDuration, targets)
	previous := 0
	for stage, target := range targets {
		if target > previous {
			begin := time.Now()
			if err = seedHistory(ctx, db, previous+1, target); err != nil {
				return err
			}
			fmt.Printf("SEED profile=%s rows=%d elapsed=%s\n", *profile, target-previous, time.Since(begin).Round(time.Millisecond))
		}
		previous = target
		stageStartEnqueued := enqueued.Load()
		trace.stage.Store(int32(stage))
		time.Sleep(*stageDuration)
		stageEnqueued := enqueued.Load()
		trace.stage.Store(-1)
		m, err := measure(ctx, db)
		if err != nil {
			return err
		}
		current := enqueued.Load()
		samples, p50, p95, p99 := trace.percentiles(int32(stage))
		pool := db.Stat()
		fmt.Printf("OBS profile=%s history_target=%d live=%d enqueued=%d enqueued_stage=%d short_completed=%d short_failed=%d long_leased=%d long_completed=%d claim_n=%d claim_p50_us=%d claim_p95_us=%d claim_p99_us=%d probe_claim_hit=%d probe_claim_read=%d probe_claim_ms=%.3f recovery_hit=%d recovery_read=%d recovery_ms=%.3f heap_dead_exact=%d heap_dead_est=%d lease_dead=%d heap_mib=%.1f indexes_mib=%.1f lease_index_kib=%d updates=%d hot=%d autovac=%d autovac_ms=%.0f autovac_read_mib=%.1f autovac_write_mib=%.1f wal_mib=%.1f pool_wait_ms=%.0f\n",
			*profile, target, m.Live, current, stageEnqueued-stageStartEnqueued, m.ShortCompleted, m.ShortFailed, m.LongLeased, m.LongCompleted, samples, p50.Microseconds(), p95.Microseconds(), p99.Microseconds(),
			m.Claim.Plan.SharedHit, m.Claim.Plan.SharedRead, m.Claim.ExecutionMS, m.Recovery.Plan.SharedHit, m.Recovery.Plan.SharedRead, m.Recovery.ExecutionMS,
			m.HeapDead, m.DeadEstimate, m.LeaseDead, float64(m.HeapBytes)/(1<<20), float64(m.IndexBytes)/(1<<20), m.LeaseBytes/1024, m.Updates, m.HOT, m.AutoCount, m.AutoMS, m.AutoReadBytes/(1<<20), m.AutoWriteBytes/(1<<20), m.WALBytes/(1<<20), float64(pool.EmptyAcquireWaitTime().Milliseconds()))
		if m.ShortFailed != 0 || m.LongLeased != int64(*active) || m.LongCompleted != 0 {
			return fmt.Errorf("workflow state changed unexpectedly at milestone %d", target)
		}
	}
	stopProducer()
	if err = <-producerDone; err != nil {
		return err
	}
	releaseClaim()
	finishCtx, stopFinish := context.WithTimeout(ctx, 45*time.Second)
	err = await(finishCtx, "all workflows completed", func() (bool, error) {
		var shortDone, longDone, failed int64
		if err := db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE workflow_name='short' AND state='completed'),count(*) FILTER (WHERE workflow_name='long' AND state='completed'),count(*) FILTER (WHERE state='failed') FROM growthbench.workflow_runs WHERE workflow_name IN ('short','long')`).Scan(&shortDone, &longDone, &failed); err != nil {
			return false, err
		}
		if failed > 0 {
			return false, fmt.Errorf("%d failed runs", failed)
		}
		return shortDone == enqueued.Load() && longDone == int64(*active), nil
	})
	stopFinish()
	if err != nil {
		return err
	}
	cancelWorkers()
	for stoppedWorkers < 2 {
		err = <-workers
		stoppedWorkers++
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	fmt.Printf("DONE profile=%s short=%d long=%d\n", *profile, enqueued.Load(), *active)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
