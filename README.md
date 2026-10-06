# durablepg

`durablepg` runs Go workflows backed by PostgreSQL. A workflow is an ordered list of steps, sleeps, and event waits. The database holds each run's position and completed step results, so another process can resume it after a crash.

**Execution is at least once.** A step can make an external change and then crash before its result is saved. On recovery, that step runs again. Use an idempotency key with external systems; a run deduplication key does not make step effects exactly once.

Requires Go 1.25+ and PostgreSQL 17+. [Architecture](ARCHITECTURE.md) explains the storage and failure protocols; [Go Reference](https://pkg.go.dev/github.com/akashsharma95/durablepg) lists the complete interface.

## Get started

```bash
go get github.com/akashsharma95/durablepg
```

This program creates a run, starts a worker, and stops on SIGINT or SIGTERM. Set `DATABASE_URL` to a PostgreSQL connection string before running it.

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    durablepg "github.com/akashsharma95/durablepg"
    "github.com/jackc/pgx/v5/pgxpool"
)

type orderInput struct {
    OrderID string `json:"order_id"`
}

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
    if err != nil {
        panic(err)
    }
    defer pool.Close()

    engine, err := durablepg.New(durablepg.Config{DB: pool})
    if err != nil {
        panic(err)
    }
    if err := engine.ApplySchema(ctx); err != nil {
        panic(err)
    }

    engine.RegisterWorkflow("order", func(b *durablepg.Builder) {
        b.Step("validate", func(_ context.Context, sc *durablepg.StepContext) (any, error) {
            var in orderInput
            if err := sc.DecodeInput(&in); err != nil {
                return nil, err
            }
            if in.OrderID == "" {
                return nil, errors.New("order_id is required")
            }
            return in.OrderID, nil
        })
        b.Step("finish", func(_ context.Context, sc *durablepg.StepContext) (any, error) {
            var orderID string
            if err := sc.StepResult("validate", &orderID); err != nil {
                return nil, err
            }
            return map[string]string{"order_id": orderID, "status": "done"}, nil
        })
    })

    id, err := engine.Run(ctx, "order", orderInput{OrderID: "ord-42"},
        durablepg.WithDeduplicationKey("order:ord-42"))
    if err != nil {
        panic(err)
    }
    fmt.Println("enqueued", id)

    if err := engine.StartWorker(ctx); err != nil {
        panic(err)
    }
    // StartWorker has a bounded drain; an uncooperative step may still be running.
    if err := engine.WaitForIdle(context.Background()); err != nil {
        panic(err)
    }
}
```

`ApplySchema` creates or upgrades the schema; it is safe to call on every start. Call it before enqueueing or starting a worker. `StartWorker` blocks until its context is canceled; run it in a goroutine if the process must also serve requests.

## Define a workflow

`RegisterWorkflow(name, build)` compiles version 1 when called. Operations run in builder order:

| Operation | What happens |
| --- | --- |
| `Step(name, fn)` | Calls `fn` unless its result was checkpointed; saves the JSON result for later steps. Step names must be unique. |
| `Sleep(duration)` | Stores the next position and a due time; no worker stays occupied during the sleep. |
| `WaitEvent(name, key, timeout)` | Waits on a fixed event key and records the outcome under `name`. |
| `WaitEventFunc(name, resolve, timeout)` | Derives an exact key for this run from its input or prior results. The resolver must return the same key on retry. |

Step and wait names must be unique within a definition. A step receives its input through `sc.DecodeInput`, earlier results through `sc.StepResult` or `sc.Value`, and a cancellation-aware `context.Context`. `WithStepTimeout` cancels that context after a deadline; it cannot forcibly stop code that ignores cancellation. Outputs must be JSON-serializable.

Use `sc.IdempotencyKey()` when an external API accepts an idempotency token. It combines the run ID, the step's position, and its name, and remains stable across retries and lease recovery:

```go
b.Step("charge", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
    return payments.Charge(ctx, amount, sc.IdempotencyKey())
})
```

This fragment assumes your application's `payments` client and `amount`. The engine does not coordinate a transaction with that client. For effects in your own database, a transactional outbox is another option.

### Events

For an order-specific wait, resolve the key from the run rather than capturing one order ID when registering the definition, and read the outcome in a later step:

```go
b.WaitEventFunc("payment", func(sc *durablepg.StepContext) (string, error) {
    var in orderInput
    if err := sc.DecodeInput(&in); err != nil {
        return "", err
    }
    return "order.paid:" + in.OrderID, nil
}, 30*time.Minute)
b.Step("finish", func(_ context.Context, sc *durablepg.StepContext) (any, error) {
    var cents int
    paid, err := sc.Event("payment", &cents)
    if err != nil || !paid {
        return "expired", err
    }
    return cents, nil
})
```

The payment handler calls `engine.EmitEvent(ctx, "order.paid:ord-42", 1999)`, which stores the event, wakes runs waiting on that exact key, and returns how many woke. Each woken run records the payload as the wait's result; a wait that times out records that instead, so `Event` reports `false`. Events remain available for 24 hours: an event emitted *before* a run reaches the wait also satisfies it, with the most recent payload. Keys shared by multiple runs wake all of them.

## Enqueueing and deployments

`Run(ctx, name, input, options...)` inserts a `ready` run and returns its ID. `Enqueue` is equivalent. Useful options include `WithScheduledAt`, `WithQueue`, `WithMaxAttempts`, `WithWorkflowVersion`, `WithDeduplicationKey`, and `WithRunID`.

Without an explicit ID, `Run` generates a time-ordered UUIDv7 locally; an ID passed to `WithRunID` must be a UUID. `EmitEvent` and waiter registration serialize by schema and event key, so an event published while a run registers its wait is never missed. Use `EmitEvent` rather than inserting event rows directly.

| Method | Purpose |
| --- | --- |
| `RunStatus(ctx, id)` | State, cursor, attempts, lease failures, wait key and deadline, last error. |
| `RunOutput(ctx, id, &dst)` | Decodes the completed run's output: the result of its last step or wait. Reports `false` until the run completes. |
| `Cancel(ctx, id)` | Moves an unfinished run to `cancelled`. A worker executing it loses its lease, and the step's context is canceled at the next renewal. |

Both `RunStatus` and `Cancel` return `ErrRunNotFound` for an unknown ID.

Without an explicit version, `Run` chooses the highest version registered **on that engine**. A worker claims only `(name, version)` pairs it has registered:

```go
engine.RegisterWorkflow("order", buildOrderV1)
engine.RegisterWorkflowVersion("order", 2, buildOrderV2)

id, err := engine.Run(ctx, "order", input) // Version 2 on this engine.
```

Treat a published definition as immutable. Deploy version 2 while keeping version 1 available until its runs finish; changing the operations of an existing version can make stored positions and checkpoints mean something else. Producers also need the definition registered locally to enqueue it. A deduplication key is unique per **workflow name and key**, not per version: reusing it after an upgrade returns the original run ID.

## Failure and shutdown behavior

- Completed step results are checkpointed. A crash before a checkpoint can repeat the step; a crash after it replays the saved result. PostgreSQL lease fencing prevents an expired worker from writing new workflow progress, **not** from making an external call it already started.
- A returned step error schedules another attempt with exponential backoff from 250 ms to 60 s. The default maximum is 25 failed attempts per run. Consecutive expired leases have separate failure accounting and backoff; progress resets that counter.
- Workers poll for due runs (250 ms by default). `LISTEN/NOTIFY` on a per-schema channel wakes them sooner, but notifications are hints, not durable work. The listener opens its own connection outside the pool, so even a one-connection pool keeps notifications.
- Canceling `StartWorker` stops new claims and allows active work to drain for up to `max(LeaseTTL, 5s)`. It then cancels remaining work. Go cannot kill a step that ignores its context. Call `WaitForIdle` **after** `StartWorker` returns and before closing the pool if all step goroutines must have exited; an uncooperative step can make that wait indefinite.

`Config` requires a `*pgxpool.Pool`. The default schema and queue are `durable` and `default`; `MaxConcurrency` defaults to `runtime.GOMAXPROCS(0)`, `LeaseTTL` to 30 s, and `HeartbeatInterval` to 10 s, which must be shorter than the lease TTL. `PollInterval`, `Schema`, `Queue`, and a `*slog.Logger` are configurable. Worker database errors go to that logger (`slog.Default()` if unset). Each worker renews all of its leases in one statement per heartbeat. Size the pool for concurrent steps, the heartbeat, and maintenance; the listener's connection is extra. Watch pool acquisition time before raising concurrency.

## Operating the database

The tables are `workflow_runs` (state, input, output, due time, version, lease, wait key), `step_checkpoints` (saved results by position), `event_log` (event history), and `schema_migrations`. See [Architecture](ARCHITECTURE.md#storage-and-migrations) for ownership and indexes. This schema is not compatible with releases before the Rust-parity rewrite; there is no in-place migration, so drain old runs and use a fresh schema.

Workers delete expired events in bounded batches. Monitor overdue `ready` runs, `waiting_event` deadlines, failed runs, and pool pressure.

Completed, failed, and cancelled runs and checkpoints have no automatic TTL. Deleting a run also forgets its deduplication key; set a retention policy that accounts for late producer retries before purging history. Before retiring a workflow version, drain or keep a compatible worker for its nonterminal runs. After restoring a database backup, reconcile external effects performed after the backup and ensure their idempotency keys remain valid before restarting workers. See [operational limits](ARCHITECTURE.md#operational-signals-and-limits) for pool, backlog, recovery, and retention guidance.

## Benchmarks

The repository's benchmarks use a real PostgreSQL instance and isolated databases via `pgtestdb`. The table compares the previous design, the current one, and the [Rust port](https://github.com/akashsharma95/durablepg-rs) it was aligned with. All three ran in alternation on one host (Apple M5 Max, PostgreSQL 18 in Podman), three rounds each, with the same settings: 1,000 runs per workload into a fresh schema from four producers, poll 10 ms, lease 5 s, heartbeat 1 s, a pool of `max(4, CPUs)`, four slots unless noted, `GOMAXPROCS=4` and a four-thread Tokio runtime. Rust used fixed-count probes that mirror these Go benchmarks rather than its Criterion suite.

| Workload | Previous Go | Current Go | Rust |
| --- | ---: | ---: | ---: |
| Enqueue | 8,934–9,063 runs/s | 8,742–8,834 runs/s | 8,127–9,214 runs/s |
| One-step completion, one slot | 1,321–1,324 workflows/s | 2,058–2,105 workflows/s | 2,048–2,077 workflows/s |
| One-step completion, four slots | 2,604–2,625 workflows/s | 4,053–4,120 workflows/s | 4,215–4,269 workflows/s |
| Ten steps | 879–881 workflows/s | 996–999 workflows/s | 1,020–1,031 workflows/s |
| Ten 64 KiB results | 110 workflows/s | 238–239 workflows/s | 241–244 workflows/s |
| Event wake to completion, back-to-back state queries (200 wakes) | 1.80–1.88 ms | 1.16–1.17 ms | 1.21–1.22 ms |
| `EmitEvent` alone | 0.85–0.91 ms | 0.54–0.56 ms | 0.59–0.60 ms |

The gains come from fewer statements, not the driver: a one-step run now takes two statements (claim with checkpoints, commit that also completes) instead of four, the output is the last result rather than a map of every result, and `EmitEvent` is one statement after the key lock. Upgrading pgx from 5.7.6 to 5.11.0 changed no workload by more than run-to-run noise. Enqueue did not improve: the previous design, an insert followed by a separate `pg_notify` statement, was 1–3% faster than the current single statement. With the same design, Go and Rust are within 4% of each other on every workload. Single-host samples are comparisons, not capacity guarantees.

To repeat the Go side, provide a disposable PostgreSQL database as `DURABLEPG_TEST_DATABASE_URL`:

```bash
GOMAXPROCS=4 DURABLEPG_TEST_DATABASE_URL='postgres://postgres:localtest@127.0.0.1:55433/durablepg?sslmode=disable' \
go test -run '^$' -bench . -benchtime=1000x
```

`BenchmarkE2EEventWakeLatency` polls run state every 5 ms, which dominates its result (about 7 ms for both designs); the event rows above use back-to-back state queries instead.

The integration tests also use `DURABLEPG_TEST_DATABASE_URL`; without it, they skip PostgreSQL-specific cases.

For retention and vacuum behavior, `go run ./cmd/growthbench` uses a **fresh, disposable PostgreSQL 18 database** (with permission to install `pgstattuple`). It refuses an existing `growthbench` schema. Run the two profiles on separate, otherwise identical databases, one at a time:

```bash
DURABLEPG_GROWTH_DATABASE_URL='postgres://postgres:localtest@127.0.0.1:55432/postgres?sslmode=disable' \
go run ./cmd/growthbench -profile=default -stage=45s -rate=100 -active=64 -history=0,100000,300000
# Point the same command at a different fresh database for -profile=tuned.
```

The workload holds 64 long claims at the default 10-second heartbeat, enqueues 100 real short workflows per second, and adds terminal runs plus checkpoints at each history milestone. `tuned` tests per-table `autovacuum_vacuum_scale_factor=0.005`, `autovacuum_vacuum_threshold=200`, and `vacuum_index_cleanup=on`; it is an experimental comparison, **not** a production setting. Output includes productive claim-query p50/p95/p99, claim/recovery buffer probes, physical dead tuples, relation sizes, per-run-table autovacuum count/time, cluster-wide autovacuum relation I/O, total WAL bytes, and pool waits. Synthetic history isolates the effect of retained rows; the short workflows generate real claim/checkpoint/finish updates. Short local runs do not establish cold-cache or long-term production performance.

One local PostgreSQL 18.6 Podman run per profile, each on a fresh container with 45-second stages, measured the current schema. The previous schema's run (text IDs, per-claim heartbeats) is shown for comparison:

| Retained terminal runs | Default claim p95 (previous) | Tuned claim p95 (previous) | Default / tuned run-table autovacuums |
| ---: | ---: | ---: | ---: |
| 0 | 1.02 ms (1.04) | 0.96 ms (1.01) | 0 / 0 |
| 100,000 | 1.00 ms (1.02) | 0.95 ms (1.03) | 1 / 1 |
| 300,000 | 0.94 ms (1.05) | 0.95 ms (1.04) | 2 / 2 |
| 1,000,000 | 0.93 ms (1.03) | 0.96 ms (1.07) | 3 / 3 |

The current run reached one million rows as a fourth stage of the same run, so its autovacuum counts are cumulative; the previous one used separate 0→1,000,000 runs. At one million, the default profile had 4,392 dead leased-index entries and its expired-lease probe hit 11 buffers (previously 5,418 and 94); the tuned profile had none and hit 2 (previously 1,252 and 191). Run-table autovacuum time to that point was 3.9 s default and 4.3 s tuned. Cluster-wide autovacuum relation reads/writes were 190/125 MiB with defaults versus 388/127 MiB tuned; these totals include other tables. Neither profile showed a claim-latency regression through one million retained runs, and these data still do **not** justify the tested vacuum override or active/history separation. Single runs, synthetic history, local caches, and the brief duration limit that conclusion.

## License

Apache License 2.0. See [LICENSE](LICENSE).
