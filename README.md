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
    if err := engine.Init(ctx); err != nil {
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

`Init` applies schema migrations and creates event-log partitions for the current month and the next 12. Call it before enqueueing or starting a worker. `StartWorker` blocks until its context is canceled; run it in a goroutine if the process must also serve requests.

## Define a workflow

`RegisterWorkflow(name, build)` compiles version 1 when called. Operations run in builder order:

| Operation | What happens |
| --- | --- |
| `Step(name, fn)` | Calls `fn` unless its result was checkpointed; saves the JSON result for later steps. Step names must be unique. |
| `Sleep(duration)` | Stores the next position and a due time; no worker stays occupied during the sleep. |
| `WaitEvent(key, timeout)` | Waits on a fixed event key. |
| `WaitEventFunc(resolve, timeout)` | Derives an exact key for this run from its input or prior results. The resolver must return the same key on retry. |

A step receives its input through `sc.DecodeInput`, earlier results through `sc.StepResult` or `sc.Value`, and a cancellation-aware `context.Context`. `WithStepTimeout` cancels that context after a deadline; it cannot forcibly stop code that ignores cancellation. Outputs must be JSON-serializable.

Use `sc.IdempotencyKey()` when an external API accepts an idempotency token. It combines the run ID and step key and remains stable across retries and lease recovery:

```go
b.Step("charge", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
    return payments.Charge(ctx, amount, sc.IdempotencyKey())
})
```

This fragment assumes your application's `payments` client and `amount`. The engine does not coordinate a transaction with that client. For effects in your own database, a transactional outbox is another option.

### Events are signals, not step results

For an order-specific wait, resolve the key from the run rather than capturing one order ID when registering the definition:

```go
b.WaitEventFunc(func(sc *durablepg.StepContext) (string, error) {
    var in orderInput
    if err := sc.DecodeInput(&in); err != nil {
        return "", err
    }
    return "order.paid:" + in.OrderID, nil
}, 30*time.Minute)
```

The payment handler can call `engine.EmitEvent(ctx, "order.paid:ord-42", payload)`. Emission stores the event and wakes runs waiting on that exact key. Events remain available for 24 hours: a matching event emitted *before* a run reaches the wait can also satisfy it. Keys shared by multiple runs can wake all of them. The payload is stored in `event_log` but is **not** passed to the next step.

A timeout also advances to the next operation. If that operation requires payment, confirmation, or another real-world condition, check its source of truth there; reaching the next step does not prove an event arrived.

## Enqueueing and deployments

`Run(ctx, name, input, options...)` inserts a `ready` run and returns its ID. `Enqueue` is equivalent. Useful options include `WithScheduledAt`, `WithQueue`, `WithMaxAttempts`, `WithWorkflowVersion`, and `WithDeduplicationKey`.

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
- Workers poll for due runs (250 ms by default). `LISTEN/NOTIFY` can wake them sooner, but notifications are hints, not durable work. With a one-connection pool, the listener is disabled and polling still works.
- Canceling `StartWorker` stops new claims and allows active work to drain for up to `max(LeaseTTL, 5s)`. It then cancels remaining work. Go cannot kill a step that ignores its context. Call `WaitForIdle` **after** `StartWorker` returns and before closing the pool if all step goroutines must have exited; an uncooperative step can make that wait indefinite.

`Config` requires a `*pgxpool.Pool`. The default schema and queue are `durable` and `default`; `MaxConcurrency` defaults to `runtime.GOMAXPROCS(0)`, `LeaseTTL` to 30 s, and `HeartbeatInterval` to 10 s. `PollInterval`, `Schema`, `Queue`, and a `*slog.Logger` are configurable. Worker database errors go to that logger (`slog.Default()` if unset). Size the pool for concurrent steps, heartbeats, maintenance, and the listener; watch pool acquisition time before raising concurrency.

## Operating the database

The principal tables are `workflow_runs` (state, input, due time, version, lease), `step_checkpoints` (saved step values), `waiters` (event registrations), `event_log` (partitioned event history), and `schema_migrations`. See [Architecture](ARCHITECTURE.md#storage-and-migrations) for ownership and indexes.

`Init` is safe to repeat. Existing event partitions avoid an exclusive table lock, but creating a missing month can take one while moving matching rows out of the default partition. Pre-create future ranges. Workers delete expired events in bounded batches; they do **not** automatically drop old partitions, which can contain events with no expiration. Plan retention around your event volume and monitor overdue `ready` runs, `waiting_event` deadlines, failed runs, and pool pressure.

## Benchmarks

The repository's benchmarks use a real PostgreSQL instance and isolated databases via `pgtestdb`. A paired local run on an Apple M5 Max, PostgreSQL 17 in Podman, `GOMAXPROCS=4`, and `-benchtime=100x -count=1` produced:

| Benchmark | Before architecture changes | After |
| --- | ---: | ---: |
| Enqueue | 2,735 runs/s | 3,047 runs/s |
| One-step completion, one slot | 101.8 workflows/s | 704.0 workflows/s |
| One-step completion, four slots | 409.3 workflows/s | 1,116 workflows/s |
| Event wake-to-completion | 6,709 µs | 7,229 µs |

The wake path was slower in this sample. Single-pass local numbers are comparisons, not a capacity promise. To repeat the same command, provide a disposable PostgreSQL 17 database as `DURABLEPG_TEST_DATABASE_URL`:

```bash
GOMAXPROCS=4 DURABLEPG_TEST_DATABASE_URL='postgres://postgres:localtest@127.0.0.1:55432/durablepg?sslmode=disable' \
go test -run '^$' -bench 'Benchmark(Enqueue|E2ESingleStep|E2EEventWakeLatency)$' -benchtime=100x -count=1
```

The integration tests also use `DURABLEPG_TEST_DATABASE_URL`; without it, they skip PostgreSQL-specific cases.

## License

Apache License 2.0. See [LICENSE](LICENSE).
