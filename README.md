# durablepg

[![Go Reference](https://pkg.go.dev/badge/github.com/akashsharma95/durablepg.svg)](https://pkg.go.dev/github.com/akashsharma95/durablepg)
[![Go Report Card](https://goreportcard.com/badge/github.com/akashsharma95/durablepg)](https://goreportcard.com/report/github.com/akashsharma95/durablepg)

`durablepg` is a durable workflow engine for Go backed by PostgreSQL. Define workflows in code that survive process crashes, restarts, and deployments—state is automatically checkpointed at each step.

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Architecture](#architecture)
- [API Reference](#api-reference)
- [Configuration](#configuration)
- [Database Schema](#database-schema)
- [Workflow Lifecycle](#workflow-lifecycle)
- [Durability Guarantees](#durability-guarantees)
- [Best Practices](#best-practices)
- [Performance](#performance)
- [Requirements](#requirements)

## Features

- **Code-First Workflows**: Define workflows as Go code using a fluent builder API.
- **Automatic Durability**: Every step result is persisted; workflows resume exactly where they left off.
- **PostgreSQL-Native**: Uses PostgreSQL for persistence, locking, and pub/sub (no external message queue required).
- **Event-Driven**: Workflows can wait for external events with `WaitEvent`.
- **Timed Operations**: Built-in support for `Sleep` with precise scheduling.
- **Retry with Backoff**: Automatic exponential backoff on step failures.
- **Idempotency Keys**: Built-in idempotency key support to prevent duplicate workflow runs.
- **Horizontal Scaling**: Multiple workers can run concurrently with `FOR UPDATE SKIP LOCKED` coordination.
- **LISTEN/NOTIFY Wakeups**: Hint-only notifications with polling fallback for reliability.
- **UUIDv7 Support**: Automatic PostgreSQL 18 `uuidv7()` run IDs when available.
- **Partitioned Event Log**: Event tables are automatically partitioned for efficient cleanup.

## Installation

```bash
go get github.com/akashsharma95/durablepg
```

## Quick Start

### 1. Initialize the Engine

```go
package main

import (
    "context"
    "os"
    "time"

    durablepg "github.com/akashsharma95/durablepg"
    "github.com/jackc/pgx/v5/pgxpool"
)

func main() {
    ctx := context.Background()

    // Connect to PostgreSQL
    pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
    if err != nil {
        panic(err)
    }
    defer pool.Close()

    // Create the engine
    engine, err := durablepg.New(durablepg.Config{DB: pool})
    if err != nil {
        panic(err)
    }

    // Initialize schema and default partitions
    if err := engine.Init(ctx); err != nil {
        panic(err)
    }

    // Register workflows
    engine.RegisterWorkflow("signup", func(wf *durablepg.Builder) {
        wf.Step("create_user", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
            var in struct {
                Email string `json:"email"`
            }
            if err := sc.DecodeInput(&in); err != nil {
                return nil, err
            }
            userID := createUser(ctx, in.Email)
            return map[string]any{"user_id": userID}, nil
        })

        wf.WaitEvent("signup.confirmed", 30*time.Minute)

        wf.Step("activate", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
            var createResult struct {
                UserID string `json:"user_id"`
            }
            if err := sc.StepResult("create_user", &createResult); err != nil {
                return nil, err
            }
            activateUser(ctx, createResult.UserID)
            return map[string]any{"active": true}, nil
        })
    })

    // Start the worker (runs until context is cancelled)
    go engine.StartWorker(ctx)

    // Enqueue a workflow run
    runID, err := engine.Run(ctx, "signup", map[string]any{
        "email": "user@example.com",
    }, durablepg.WithDeduplicationKey("signup:user@example.com"))
    if err != nil {
        panic(err)
    }
    println("Started workflow:", runID)

    // Block forever (or handle shutdown signal)
    select {}
}
```

### 2. Define a Workflow

```go
engine.RegisterWorkflow("signup", func(b *durablepg.Builder) {
    // Step 1: Create user account
    b.Step("create_user", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var in struct {
            Email string `json:"email"`
        }
        if err := sc.DecodeInput(&in); err != nil {
            return nil, err
        }
        // Idempotent side effect: create user in database
        userID := createUser(ctx, in.Email)
        return map[string]any{"user_id": userID}, nil
    })

    // Step 2: Wait for email confirmation (external event)
    b.WaitEvent("signup.confirmed", 30*time.Minute)

    // Step 3: Activate the account
    b.Step("activate", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        // Access results from previous steps
        var createResult struct {
            UserID string `json:"user_id"`
        }
        if err := sc.StepResult("create_user", &createResult); err != nil {
            return nil, err
        }
        activateUser(ctx, createResult.UserID)
        return map[string]any{"active": true}, nil
    })
})
```

### 3. Emit Events

```go
// When user confirms email (e.g., from HTTP handler)
err := engine.EmitEvent(ctx, "signup.confirmed:user@example.com", map[string]any{
    "confirmed_at": time.Now(),
})
```

---

## Architecture

### High-Level Overview

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Application Layer                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌──────────────┐     ┌──────────────┐     ┌──────────────────────────┐    │
│   │   Workflow   │     │   Workflow   │     │       Engine             │    │
│   │  Definition  │────▶│   Builder    │────▶│  (Coordinator)           │    │
│   │  (User Code) │     │              │     │                          │    │
│   └──────────────┘     └──────────────┘     │  - Register workflows    │    │
│                                              │  - Enqueue runs          │   │
│                                              │  - Emit events           │   │
│                                              │  - Manage workers        │   │
│                                              └───────────┬──────────────┘   │
│                                                          │                  │
│                                              ┌───────────▼──────────────┐   │
│                                              │        Worker            │   │
│                                              │   (Background Loop)      │   │
│                                              │                          │   │
│                                              │  - Poll for ready runs   │   │
│                                              │  - Claim via leases      │   │
│                                              │  - Execute steps         │   │
│                                              │  - Checkpoint results    │   │
│                                              └───────────┬──────────────┘   │
│                                                          │                  │
└──────────────────────────────────────────────────────────┼──────────────────┘
                                                           │
┌──────────────────────────────────────────────────────────▼───────────────────┐
│                            PostgreSQL Layer                                  │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   ┌─────────────────┐  ┌──────────────────┐  ┌────────────────────────────┐  │
│   │  workflow_runs  │  │ step_checkpoints │  │       event_log            │  │
│   ├─────────────────┤  ├──────────────────┤  │     (Partitioned)          │  │
│   │ id              │  │ run_id           │  ├────────────────────────────┤  │
│   │ workflow_name   │  │ step_key         │  │ id                         │  │
│   │ state           │  │ result_json      │  │ event_key                  │  │
│   │ step_index      │  │ created_at       │  │ payload_json               │  │
│   │ input_json      │  └──────────────────┘  │ created_at                 │  │
│   │ next_run_at     │                        └────────────────────────────┘  │
│   │ lease_owner     │                                                        │
│   │ lease_until     │          ┌──────────────────────────────────┐          │
│   │ retries         │          │      LISTEN/NOTIFY Channel       │          │
│   │ ...             │          │   (durablepg_workflow_ready)     │          │
│   └─────────────────┘          └──────────────────────────────────┘          │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Component Details

#### Engine (`engine.go`)

The **Engine** is the central coordinator responsible for:

| Responsibility | Description |
|----------------|-------------|
| **Workflow Registration** | Maintains a registry mapping workflow names to their compiled operation lists |
| **Run Enqueueing** | Inserts new workflow runs into `workflow_runs` table and triggers NOTIFY |
| **Event Emission** | Writes events to `event_log` and wakes waiting workflows |
| **Worker Management** | Initializes and coordinates background workers |
| **Schema Management** | Provides methods to create/migrate database schema |

```go
type Engine struct {
    db         *pgxpool.Pool
    workflows  map[string]*compiledWorkflow  // name -> operations
    config     Config
    workerID   string                         // unique per instance
}
```

#### Worker (`worker.go`)

The **Worker** is a background goroutine that drives workflow execution:

| Responsibility | Description |
|----------------|-------------|
| **Polling** | Periodically queries for `ready` workflows or those with expired leases |
| **Claiming** | Uses `FOR UPDATE SKIP LOCKED` to claim workflows atomically |
| **Leasing** | Sets `lease_owner` and `lease_until` to prevent other workers from claiming |
| **Step Execution** | Runs step functions, persisting results to `step_checkpoints` |
| **State Transitions** | Updates workflow state based on execution results |
| **LISTEN Handling** | Subscribes to PostgreSQL notifications for immediate wakeup |

**Worker Loop:**

```text
┌─────────────────────────────────────────────────────────────────┐
│                        Worker Loop                              │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   ┌─────────┐    ┌──────────┐    ┌───────────┐    ┌──────────┐  │
│   │  LISTEN │───▶│  Claim   │───▶│  Execute  │───▶│  Update  │  │
│   │   or    │    │   Runs   │    │   Steps   │    │  State   │  │
│   │  Poll   │    │          │    │           │    │          │  │
│   └─────────┘    └──────────┘    └───────────┘    └──────────┘  │
│        │                                                  │     │
│        └──────────────────────────────────────────────────┘     │
│                         (repeat)                                │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

#### Builder (`builder.go`)

The **Builder** provides a fluent API for defining workflow operations:

```go
type Builder struct {
    operations []operation
}

type operation struct {
    kind       opKind           // opStep, opSleep, opWaitEvent
    key        string           // unique identifier
    stepFn     StepFunc         // for opStep
    duration   time.Duration    // for opSleep
    timeout    time.Duration    // for opWaitEvent
}
```

**Operation Types:**

| Operation | Description |
|-----------|-------------|
| `Step(key, fn)` | Execute a function and persist its result |
| `Sleep(duration)` | Pause execution for a specified duration |
| `WaitEvent(key, timeout)` | Wait for an external event or timeout |

#### StepContext (`types.go`)

**StepContext** is passed to every step function, providing access to:

```go
type StepContext struct {
    RunID    WorkflowID
    Workflow string
    Input    json.RawMessage
}

// Methods:
func (sc *StepContext) DecodeInput(dest any) error      // Unmarshal workflow input
func (sc *StepContext) StepResult(key string, dest any) error
func (sc *StepContext) WorkflowID() WorkflowID
```

### Data Flow

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                            Workflow Execution Flow                           │
└──────────────────────────────────────────────────────────────────────────────┘

1. ENQUEUE
   ─────────────────────────────────────────────────────────────────────────────
   Application                         Engine                        PostgreSQL
       │                                  │                               │
       │  Enqueue("signup", input)        │                               │
       │─────────────────────────────────▶│                               │
       │                                  │  INSERT workflow_runs         │
       │                                  │  (state='ready')              │
       │                                  │──────────────────────────────▶│
       │                                  │                               │
       │                                  │  NOTIFY durablepg_ready       │
       │                                  │──────────────────────────────▶│
       │                                  │                               │
       │  ◀───────── runID ───────────────│                               │


2. CLAIM & EXECUTE
   ─────────────────────────────────────────────────────────────────────────────
   Worker                                                            PostgreSQL
       │                                                                  │
       │  SELECT ... FOR UPDATE SKIP LOCKED                               │
       │  WHERE state='ready' AND next_run_at <= now()                    │
       │─────────────────────────────────────────────────────────────────▶│
       │                                                                  │
       │  UPDATE: state='leased', lease_owner=workerID, lease_until=...   │
       │─────────────────────────────────────────────────────────────────▶│
       │                                                                  │
       │                                                                  │
       │  ┌────────────────────────────────────────────────────────────┐  │
       │  │                   For each operation:                      │  │
       │  │                                                            │  │
       │  │   [Step]       Check step_checkpoints for cached result    │  │
       │  │                If miss: execute fn, INSERT checkpoint      │  │
       │  │                Advance step_index                          │  │
       │  │                                                            │  │
       │  │   [Sleep]      UPDATE next_run_at = now() + duration       │  │
       │  │                SET state = 'ready', release lease          │  │
       │  │                (worker exits, resumes later)               │  │
       │  │                                                            │  │
       │  │   [WaitEvent]  SET state = 'waiting_event'                 │  │
       │  │                SET wait_key = event_key                    │  │
       │  │                SET wait_deadline = now() + timeout         │  │
       │  │                (worker exits, waits for event)             │  │
       │  └────────────────────────────────────────────────────────────┘  │
       │                                                                  │
       │  All operations complete: UPDATE state='completed'               │
       │─────────────────────────────────────────────────────────────────▶│


3. EVENT EMISSION
   ─────────────────────────────────────────────────────────────────────────────
   Application                         Engine                        PostgreSQL
       │                                  │                               │
       │  EmitEvent("signup.confirmed",   │                               │
       │            payload)              │                               │
       │─────────────────────────────────▶│                               │
       │                                  │  INSERT event_log             │
       │                                  │──────────────────────────────▶│
       │                                  │                               │
       │                                  │  UPDATE workflow_runs         │
       │                                  │  SET state='ready'            │
       │                                  │  WHERE wait_key LIKE 'signup.%'
       │                                  │──────────────────────────────▶│
       │                                  │                               │
       │                                  │  NOTIFY durablepg_ready       │
       │                                  │──────────────────────────────▶│
```

### Concurrency Model

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Multi-Worker Concurrency                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌──────────────┐    ┌──────────────┐    ┌──────────────┐                  │
│   │   Worker 1   │    │   Worker 2   │    │   Worker 3   │                  │
│   │  (Server A)  │    │  (Server B)  │    │  (Server B)  │                  │
│   └──────┬───────┘    └──────┬───────┘    └──────┬───────┘                  │
│          │                   │                   │                          │
│          │    FOR UPDATE SKIP LOCKED             │                          │
│          ▼                   ▼                   ▼                          │
│   ┌─────────────────────────────────────────────────────────────────────┐   │
│   │                                                                     │   │
│   │   workflow_runs table                                               │   │
│   │   ┌─────────────────────────────────────────────────────────────┐   │   │
│   │   │ run_1 │ leased │ worker_1 │ ████████████                    │   │   │
│   │   │ run_2 │ leased │ worker_2 │ ████████                        │   │   │
│   │   │ run_3 │ leased │ worker_3 │ ██████████████                  │   │   │
│   │   │ run_4 │ ready  │   NULL   │ (available for claiming)        │   │   │
│   │   │ run_5 │ ready  │   NULL   │ (available for claiming)        │   │   │
│   │   └─────────────────────────────────────────────────────────────┘   │   │
│   │                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
│   Key guarantees:                                                           │
│   - Each run is processed by exactly one worker at a time                   │
│   - SKIP LOCKED prevents contention and blocking                            │
│   - Expired leases are reclaimed by any worker                              │
│   - No external coordinator required                                        │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Retry & Backoff Strategy

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Exponential Backoff                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   Step fails ──▶ Calculate backoff ──▶ Schedule retry ──▶ Release lease     │
│                                                                             │
│   Backoff = min(BaseBackoff * 2^retries, MaxBackoff)                        │
│                                                                             │
│   Example (BaseBackoff=1s, MaxBackoff=1h):                                  │
│                                                                             │
│   Retry 0:  1s                                                              │
│   Retry 1:  2s                                                              │
│   Retry 2:  4s                                                              │
│   Retry 3:  8s                                                              │
│   Retry 4:  16s                                                             │
│   Retry 5:  32s                                                             │
│   Retry 6:  64s                                                             │
│   Retry 7:  128s                                                            │
│   Retry 8:  256s                                                            │
│   Retry 9:  512s                                                            │
│   Retry 10: 1024s → capped at 1h                                            │
│   ...                                                                       │
│   Retry N:  → state='failed' (after MaxRetries)                             │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## API Reference

### Engine

| Method | Description |
|--------|-------------|
| `New(cfg Config) (*Engine, error)` | Create a new engine instance. |
| `Init(ctx) error` | Apply schema and create default future partitions. |
| `ApplySchema(ctx) error` | Create/migrate database tables. |
| `EnsurePartitions(ctx, monthsAhead) error` | Ask PostgreSQL to create future event-log partitions. |
| `EventLogMonthlyPartitionsSQL(start, months) (string, error)` | Generate DDL for event log partitions. |
| `RegisterWorkflow(name, build)` | Register a workflow using a builder function. |
| `Register(wf Workflow)` | Register a workflow definition. |
| `StartWorker(ctx)` | Start the background worker (blocks until ctx cancelled). |
| `Run(ctx, name, input, ...opts) (string, error)` | Preferred alias for starting a workflow run. |
| `Enqueue(ctx, name, input, ...opts) (string, error)` | Start a new workflow run. |
| `EmitEvent(ctx, key, payload) error` | Emit an event that may wake waiting workflows. |

### Workflow Interface

```go
type Workflow interface {
    Name() string           // Unique workflow identifier
    Build(b *Builder)       // Define workflow operations
}
```

### Builder Methods

| Method | Description |
|--------|-------------|
| `Step(key string, fn StepFunc)` | Add a durable step that persists its result. |
| `Sleep(duration time.Duration)` | Pause workflow execution for a duration. |
| `WaitEvent(keyPrefix string, timeout time.Duration)` | Wait for an external event or timeout. |

### StepFunc Type

```go
type StepFunc func(ctx context.Context, sc *StepContext) (any, error)
```

### StepContext Methods

| Method | Description |
|--------|-------------|
| `DecodeInput(dest any) error` | Unmarshal workflow input into dest. |
| `StepResult(key string, dest any) error` | Get result from a previous step. |
| `WorkflowID() WorkflowID` | Get the current workflow run ID. |

### Enqueue Options

| Option | Description |
|--------|-------------|
| `WithWorkflowID(id WorkflowID)` | Provide a deterministic workflow run ID. |
| `WithDeduplicationKey(key string)` | Preferred alias for deduplicating workflow runs. |
| `WithIdempotencyKey(key string)` | Prevent duplicate runs with same key. |
| `WithScheduledAt(t time.Time)` | Schedule workflow to start at a future time. |

---

## Configuration

```go
type Config struct {
    // DB is the PostgreSQL connection pool (required)
    DB *pgxpool.Pool

    // Schema is the PostgreSQL schema name
    // Default: "durable"
    Schema string

    // Queue is the default queue name for new runs
    // Default: "default"
    Queue string

    // MaxConcurrency caps in-flight workflow executions per engine
    // Default: runtime.GOMAXPROCS(0)
    MaxConcurrency int

    // PollInterval is how often workers check for ready workflows
    // Default: 250ms
    PollInterval time.Duration

    // LeaseTTL is how long a worker holds a workflow lease
    // Default: 30s
    LeaseTTL time.Duration

    // HeartbeatInterval controls how often leased runs refresh ownership
    // Default: 10s
    HeartbeatInterval time.Duration
}
```

---

## Database Schema

### Tables

#### `workflow_runs`

Primary table storing workflow instances and their execution state.

| Column | Type | Description |
|--------|------|-------------|
| `id` | `UUID` | Primary key (UUIDv7 when available). |
| `workflow_name` | `TEXT` | Registered workflow name. |
| `state` | `TEXT` | Current state (see below). |
| `step_index` | `INT` | Current operation index. |
| `input_json` | `JSONB` | Workflow input data. |
| `idempotency_key` | `TEXT` | Optional deduplication key. |
| `next_run_at` | `TIMESTAMPTZ` | When to resume execution. |
| `lease_owner` | `TEXT` | Worker ID holding the lease. |
| `lease_until` | `TIMESTAMPTZ` | Lease expiration time. |
| `wait_key` | `TEXT` | Event key being waited on. |
| `wait_deadline` | `TIMESTAMPTZ` | Event wait timeout. |
| `retries` | `INT` | Number of retry attempts. |
| `created_at` | `TIMESTAMPTZ` | Creation timestamp. |
| `updated_at` | `TIMESTAMPTZ` | Last update timestamp. |

#### `step_checkpoints`

Stores persisted results of completed steps.

| Column | Type | Description |
|--------|------|-------------|
| `run_id` | `UUID` | Foreign key to `workflow_runs`. |
| `step_key` | `TEXT` | Step identifier. |
| `result_json` | `JSONB` | Serialized step result. |
| `created_at` | `TIMESTAMPTZ` | Checkpoint timestamp. |

#### `event_log` (Partitioned)

Stores emitted events for workflows waiting on external signals.

| Column | Type | Description |
|--------|------|-------------|
| `id` | `UUID` | Primary key. |
| `event_key` | `TEXT` | Event identifier. |
| `payload_json` | `JSONB` | Event payload. |
| `created_at` | `TIMESTAMPTZ` | Emission timestamp. |

**Partitioning**: Monthly range partitions on `created_at` for efficient cleanup.

### Workflow States

| State | Description |
|-------|-------------|
| `ready` | Workflow is queued and ready to execute. |
| `leased` | A worker has claimed the workflow and is executing. |
| `waiting_event` | Workflow is paused, waiting for an external event. |
| `completed` | Workflow finished all operations successfully. |
| `failed` | Workflow exceeded maximum retry attempts. |

### State Transitions

```text
                    ┌──────────────────────────────────────────┐
                    │                                          │
                    ▼                                          │
              ┌──────────┐                                     │
  Enqueue ───▶│  ready   │◀────────────────────────────────────┤
              └────┬─────┘                                     │
                   │                                           │
                   │ Worker claims                             │
                   ▼                                           │
              ┌──────────┐                                     │
              │  leased  │─────────────────────────────────────┤
              └────┬─────┘                                     │
                   │                                           │
        ┌──────────┼──────────┬──────────────┐                │
        │          │          │              │                │
        │ step ok  │ Sleep    │ WaitEvent    │ step fails     │
        │          │          │              │ (retry)        │
        ▼          ▼          ▼              │                │
   ┌──────────┐   next    ┌─────────────┐    │                │
   │completed │   run_at  │waiting_event│    │                │
   └──────────┘    │      └──────┬──────┘    │                │
                   │             │           │                │
                   │      Event  │           │                │
                   │      arrives│           │                │
                   │             │           │                │
                   └─────────────┴───────────┘
                             │
                             │ MaxRetries exceeded
                             ▼
                       ┌──────────┐
                       │  failed  │
                       └──────────┘
```

---

## Workflow Lifecycle

### 1. Registration

When you call `engine.RegisterWorkflow(name, build)`:
- The `Builder` is invoked to collect all operations.
- Operations are compiled into an ordered list.
- The workflow is stored in the engine's registry.

### 2. Enqueueing

When you call `engine.Run(ctx, "name", input)`:
- A new row is inserted into `workflow_runs` with `state='ready'`.
- `NOTIFY` is sent to wake any listening workers immediately.
- The run ID is returned to the caller.

### 3. Claiming

Workers poll or listen for ready workflows:
- Query: `SELECT ... WHERE state='ready' AND next_run_at <= now() FOR UPDATE SKIP LOCKED`.
- Update: `SET state='leased', lease_owner=workerID, lease_until=now()+leaseDuration`.
- Multiple workers can run concurrently without conflicts.

### 4. Execution

For each operation in the workflow:

**Step:**
1. Check `step_checkpoints` for existing result.
2. If cached: use cached value, advance to next operation.
3. If not cached: execute function, persist result, advance.

**Sleep:**
1. Update `next_run_at = now() + duration`.
2. Set `state = 'ready'`.
3. Release lease, exit (workflow resumes later).

**WaitEvent:**
1. Set `state = 'waiting_event'`.
2. Set `wait_key` and `wait_deadline`.
3. Release lease, exit (workflow resumes when event arrives or timeout).

### 5. Completion

When all operations complete:
- Update `state = 'completed'`.
- Workflow is finished.

---

## Durability Guarantees

### At-Least-Once Execution

Every step is guaranteed to complete at least once:
- Results are persisted to `step_checkpoints` after successful execution.
- If a worker crashes before persisting, the step will re-execute on retry.
- Once persisted, the step will never re-execute (result is replayed).

### Exactly-Once Semantics

To achieve exactly-once semantics, make your steps idempotent:

```go
b.Step("charge-payment", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
    // Use workflow run ID as idempotency key with external services
    return paymentProvider.Charge(ctx, ChargeRequest{
        IdempotencyKey: string(sc.WorkflowID()) + "-charge",
        Amount:         100,
    })
})
```

### Failure Recovery

| Failure Mode | Recovery Mechanism |
|--------------|-------------------|
| Worker crash during step | Lease expires, another worker reclaims. |
| Worker crash after checkpoint | Cached result replayed, step skipped. |
| Database connection lost | Worker reconnects, resumes polling. |
| PostgreSQL failover | Connection pool handles reconnection. |

---

## Best Practices

### Step Idempotency

Steps should be idempotent whenever possible. While the engine guarantees a step won't re-execute after its result is persisted, a crash during step execution (before persistence) will cause a retry.

### Event Key Patterns

Use structured event keys for targeted delivery:

```go
// In workflow definition - include unique identifier
b.WaitEvent("order.paid:"+orderID, 24*time.Hour)

// When emitting - match the exact key
engine.EmitEvent(ctx, "order.paid:ORD-12345", payload)
```

### Error Handling

Return errors from steps to trigger automatic retry with backoff:

```go
b.Step("call-api", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
    resp, err := httpClient.Do(req)
    if err != nil {
        return nil, err // Will retry with exponential backoff
    }
    if resp.StatusCode >= 500 {
        return nil, fmt.Errorf("server error: %d", resp.StatusCode)
    }
    return parseResponse(resp)
})
```

### Graceful Shutdown

Handle shutdown signals to allow in-progress steps to complete:

```go
ctx, cancel := context.WithCancel(context.Background())

go func() {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    <-sigCh
    cancel()
}()

engine.StartWorker(ctx) // Returns when ctx is cancelled
```

### Event Log Partitioning

Pre-create monthly partitions for high-volume workloads:

```go
// On startup or via cron job
ddl, _ := engine.EventLogMonthlyPartitionsSQL(time.Now(), 6) // Next 6 months
pool.Exec(ctx, ddl)
```

---

## Performance

The repository includes real PostgreSQL integration benchmarks in `benchmark_test.go`. These are not microbenchmarks of isolated functions; they exercise the actual durable execution path.

### What Is Measured

| Benchmark | What it measures | Why it matters |
|-----------|------------------|----------------|
| `BenchmarkEnqueue` | Producer-side workflow creation throughput | Cost of the durable enqueue path (`INSERT` + `RETURNING` + `NOTIFY`) |
| `BenchmarkE2ESingleStep` | End-to-end throughput for a single-step workflow | Cost of claim, checkpoint, step advance, and completion |
| `BenchmarkE2EEventWakeLatency` | Wait-for-event resume latency | Cost of event wake-up, state transition, and completion |

### Latest Sample Results

These numbers were measured on **March 15, 2026** on:

- **Machine**: Apple M1 Pro
- **OS/Arch**: `darwin/arm64`
- **PostgreSQL**: 17 (Docker)
- **Go benchmark flags**: `-benchmem -count=1`

```text
BenchmarkEnqueue-8                         2266    442979 ns/op      2257 runs/s      3105 B/op     65 allocs/op
BenchmarkE2ESingleStep/workers_1-8         112  10330762 ns/op        96.79 workflows/s 16079 B/op   259 allocs/op
BenchmarkE2ESingleStep/workers_8-8         648   1626729 ns/op       614.6 workflows/s  12027 B/op   215 allocs/op
BenchmarkE2EEventWakeLatency-8              69  17163561 ns/op      7524 wake-us/op   34067 B/op   543 allocs/op
```

### Observations

- Enqueue throughput is about **2.26k runs/s** on a local Postgres instance.
- End-to-end single-step execution scales from **96.8 workflows/s** with one worker to **614.6 workflows/s** with eight workers.
- Event-driven wake-up latency is about **7.5ms** for the wake path and about **17.2ms** for the full wait-and-resume cycle.
- Allocation pressure is currently much higher on the event path than on simple enqueue. The most obvious optimization targets are JSON churn and the number of SQL round-trips in the wait/event flow.

These numbers are useful for relative comparisons between changes. They are not a portable SLA because they depend heavily on CPU, storage, network, and PostgreSQL configuration.

### Running the Benchmarks

The benchmark harness uses [`pgtestdb`](https://github.com/peterldowns/pgtestdb), which creates isolated test databases on top of an existing PostgreSQL instance. It does **not** start PostgreSQL for you.

Start a disposable local PostgreSQL 17 instance:

```bash
docker run --rm --name durablepg-bench-pg \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_DB=postgres \
  -p 55432:5432 \
  postgres:17
```

Then run the benchmark suite:

```bash
DURABLEPG_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable' \
go test -run '^$' -bench . -benchmem -count=1
```

---

## Requirements

- **PostgreSQL**: 17+ (18+ recommended for native `uuidv7()`).
- **Go**: 1.25+.
- **Dependencies**: `github.com/jackc/pgx/v5`.

---

## License

MIT License
