# DurablePG - Durable Workflow Engine Architecture

## Table of Contents
1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Core Concepts](#core-concepts)
4. [Database Schema](#database-schema)
5. [Execution Model](#execution-model)
6. [Use Cases](#use-cases)
7. [Scalability](#scalability)
8. [Comparison with Alternatives](#comparison-with-alternatives)
9. [API Reference](#api-reference)
10. [Production Considerations](#production-considerations)

---

## Overview

DurablePG is a **code-first durable workflow engine** backed by PostgreSQL. It provides reliable, fault-tolerant execution of multi-step workflows with automatic retry, checkpointing, and distributed coordination.

### Key Features

| Feature | Description |
|---------|-------------|
| **Durable Execution** | Steps are checkpointed to PostgreSQL; workflows survive crashes and restarts |
| **Exactly-Once Semantics** | Step results are persisted before acknowledgment, preventing duplicate execution |
| **Distributed Workers** | Multiple workers can process workflows concurrently with lease-based coordination |
| **Event-Driven** | Workflows can wait for external events with configurable timeouts |
| **Scheduled Execution** | Support for delayed/scheduled workflow starts and built-in sleep operations |
| **Automatic Retry** | Exponential backoff retry with configurable max attempts |
| **Idempotency** | Built-in idempotency key support for deduplication |
| **Queue Isolation** | Multiple named queues for workload isolation and prioritization |

### Quick Start

```go
package main

import (
    "context"
    "github.com/jackc/pgx/v5/pgxpool"
    durablepg "github.com/akashsharma95/durablepg"
)

type OrderWorkflow struct{}

func (OrderWorkflow) Name() string { return "process_order" }

func (OrderWorkflow) Build(b *durablepg.Builder) {
    b.Step("validate", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input struct{ OrderID string `json:"order_id"` }
        sc.DecodeInput(&input)
        return map[string]bool{"valid": true}, nil
    })
    
    b.Step("charge_payment", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        return map[string]string{"transaction_id": "txn_123"}, nil
    })
    
    b.Step("fulfill", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        return map[string]string{"status": "shipped"}, nil
    })
}

func main() {
    ctx := context.Background()
    pool, _ := pgxpool.New(ctx, "postgres://localhost/mydb")
    
    engine, _ := durablepg.New(durablepg.Config{DB: pool})
    engine.Init(ctx)
    engine.Register(OrderWorkflow{})
    
    go engine.StartWorker(ctx)
    
    engine.Enqueue(ctx, "process_order", map[string]any{"order_id": "ord_456"})
}
```

---

## Architecture

### System Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              APPLICATION LAYER                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌─────────────┐    ┌─────────────┐    ┌─────────────┐                     │
│   │   Client    │    │   Client    │    │   Client    │                     │
│   │  Service A  │    │  Service B  │    │  Service C  │                     │
│   └──────┬──────┘    └──────┬──────┘    └──────┬──────┘                     │
│          │                  │                  │                            │
│          │    Enqueue()     │    Enqueue()     │   EmitEvent()              │
│          └─────────────┬────┴─────────────┬────┴──────────┘                 │
│                        ▼                  ▼                                 │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │                         ENGINE INSTANCE                            │    │
│   │  ┌──────────────────────────────────────────────────────────────┐  │    │
│   │  │                    Workflow Registry                         │  │    │
│   │  │   ┌───────────┐  ┌───────────┐  ┌───────────┐                │  │    │
│   │  │   │ Workflow1 │  │ Workflow2 │  │ Workflow3 │                │  │    │
│   │  │   └───────────┘  └───────────┘  └───────────┘                │  │    │
│   │  └──────────────────────────────────────────────────────────────┘  │    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      │ pgxpool
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              WORKER LAYER                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────┐   │
│   │                        StartWorker()                                │   │
│   │                                                                     │   │
│   │   ┌────────────────────┐      ┌────────────────────────────────┐    │   │
│   │   │   Listen Loop      │      │      Dispatch Loop             │    │   │
│   │   │                    │      │                                │    │   │
│   │   │ LISTEN channel     │─────▶│ ┌──────────────────────────┐   │    │   │
│   │   │ pg_notify wakeup   │      │ │ Claim Ready Runs         │   │    │   │
│   │   │                    │      │ │ SELECT FOR UPDATE        │   │    │   │
│   │   └────────────────────┘      │ │ SKIP LOCKED              │   │    │   │
│   │                               │ └────────────┬─────────────┘   │    │   │
│   │                               │              │                 │    │   │
│   │   ┌────────────────────┐      │              ▼                 │    │   │
│   │   │   Heartbeat Loop   │      │ ┌──────────────────────────┐   │    │   │
│   │   │                    │◀────▶│ │ Execute Workflow Steps   │   │    │   │
│   │   │ Renew lease_until  │      │ │                          │   │    │   │
│   │   │ every 10s          │      │ │ Step → Checkpoint → Next │   │    │   │
│   │   └────────────────────┘      │ └──────────────────────────┘   │    │   │
│   │                               └────────────────────────────────┘    │   │
│   └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      │ SQL
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           POSTGRESQL STORAGE                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌───────────────────┐  ┌───────────────────┐  ┌────────────────────────┐  │
│   │  workflow_runs    │  │ step_checkpoints  │  │      event_log         │  │
│   │                   │  │                   │  │   (partitioned)        │  │
│   │ - id              │  │ - run_id (FK)     │  │                        │  │
│   │ - workflow_name   │  │ - step_key        │  │ - event_key            │  │
│   │ - queue           │  │ - value_json      │  │ - payload_json         │  │
│   │ - state           │  │ - completed_at    │  │ - expires_at           │  │
│   │ - step_index      │  │                   │  │                        │  │
│   │ - attempt         │  │                   │  │                        │  │
│   │ - lease_owner     │  │                   │  │                        │  │
│   │ - lease_until     │  │                   │  │                        │  │
│   │ - input_json      │  │                   │  │                        │  │
│   │ - output_json     │  │                   │  │                        │  │
│   └───────────────────┘  └───────────────────┘  └────────────────────────┘  │
│                                                                             │
│   ┌───────────────────┐                                                     │
│   │     waiters       │   Indexes:                                          │
│   │                   │   - workflow_runs_ready_idx (queue, next_run_at)    │
│   │ - event_key       │   - workflow_runs_leased_idx (lease_until)          │
│   │ - run_id (FK)     │   - workflow_runs_idempotency_uidx                  │
│   │ - deadline        │   - event_log_lookup_idx                            │
│   └───────────────────┘                                                     │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Component Breakdown

#### Engine
The central coordinator that:
- Maintains the workflow registry (compiled workflow definitions)
- Provides the `Enqueue()` API for creating workflow runs
- Provides the `EmitEvent()` API for event-driven workflows
- Manages database schema and migrations

#### Worker
Background processor that:
- **Listen Loop**: Uses PostgreSQL `LISTEN/NOTIFY` for instant wakeup on new work
- **Dispatch Loop**: Polls for ready runs and distributes to goroutine pool
- **Heartbeat Loop**: Maintains lease ownership during long-running steps
- **Concurrency Control**: Limits in-flight executions via `MaxConcurrency`

#### Compiled Workflow
Pre-validated workflow definition containing:
- Ordered list of operations (steps, sleeps, wait events)
- Step name uniqueness validation
- Function references for step execution

---

## Core Concepts

### Workflow States

```
                    ┌─────────────────────────────────────────┐
                    │              STATE MACHINE              │
                    └─────────────────────────────────────────┘

                              Enqueue()
                                  │
                                  ▼
                           ┌──────────┐
                           │  ready   │◀──────────────────┐
                           └────┬─────┘                   │
                                │                         │
                    Worker claims│                        │ Retry (backoff)
                   (SELECT FOR UPDATE)                    │ or
                                │                         │ Sleep elapsed
                                ▼                         │ or
                           ┌──────────┐                   │ Event received
         ┌─────────────────│  leased  │───────────────────┤
         │                 └────┬─────┘                   │
         │                      │                         │
    Lease expires               │                         │
    (worker crash)              │                         │
         │           ┌──────────┼──────────┐              │
         │           │          │          │              │
         │           ▼          ▼          ▼              │
         │     ┌──────────┐  ┌─────────────────┐          │
         └────▶│ waiting_ │  │ All steps done  │          │
               │  event   │  └────────┬────────┘          │
               └────┬─────┘           │                   │
                    │                 ▼                   │
           Timeout  │          ┌───────────┐              │
           or Event │          │ completed │              │
           received │          └───────────┘              │
                    │                                     │
                    └─────────────────────────────────────┘

                    Max attempts exceeded
                           │
                           ▼
                    ┌───────────┐
                    │  failed   │
                    └───────────┘
```

### Step Execution Model

Each step follows an **exactly-once** execution pattern:

```
Step Execution Flow:
────────────────────

1. Check if step result exists in step_checkpoints
   │
   ├─ YES → Load cached result, advance to next step
   │
   └─ NO → Continue to execution
            │
            ▼
2. Execute step function with StepContext
   │
   ├─ SUCCESS → Marshal result to JSON
   │             │
   │             ▼
   │          3. Persist checkpoint (INSERT ... ON CONFLICT DO NOTHING)
   │             │
   │             ├─ INSERT succeeded → Use new result
   │             │
   │             └─ Conflict (another worker) → Load existing result
   │
   └─ ERROR → Schedule retry with exponential backoff
              or mark as failed if max_attempts exceeded
```

### Operations

| Operation | Description | Persistence |
|-----------|-------------|-------------|
| **Step** | Execute idempotent function | Result stored in `step_checkpoints` |
| **Sleep** | Pause for duration | `next_run_at` updated, state → ready |
| **WaitEvent** | Wait for external event | State → `waiting_event`, waiter registered |

### Lease-Based Coordination

```
Worker A                        PostgreSQL                      Worker B
────────                        ──────────                      ────────
    │                               │                               │
    │ ──SELECT FOR UPDATE──────────▶│                               │
    │       SKIP LOCKED             │                               │
    │                               │                               │
    │ ◀─────── run_1 ───────────────│                               │
    │    (lease_owner=A)            │                               │
    │                               │                               │
    │                               │◀── SELECT FOR UPDATE ─────────│
    │                               │     SKIP LOCKED               │
    │                               │                               │
    │                               │────── run_2 ─────────────────▶│
    │                               │   (lease_owner=B)             │
    │                               │                               │
    │ ──heartbeat (10s)────────────▶│                               │
    │   lease_until = now + 30s     │                               │
    │                               │                               │
    │         [Worker A crashes]    │                               │
    │                               │                               │
    │                               │ lease_until expires (30s)     │
    │                               │                               │
    │                               │◀── recoverExpiredLeases ──────│
    │                               │   state = 'ready'             │
    │                               │   lease_owner = NULL          │
    │                               │                               │
    │                               │────── run_1 ─────────────────▶│
    │                               │   (new lease to B)            │
```

---

## Database Schema

### Entity Relationship Diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│                         workflow_runs (parent)                             │
├────────────────────────────────────────────────────────────────────────────┤
│ PK: id (TEXT)                                                              │
│ ─────────────────────────────────────────────────────────────────────────  │
│ workflow_name        TEXT NOT NULL                                         │
│ queue                TEXT NOT NULL          -- workload isolation          │
│ state                TEXT NOT NULL          -- ready|leased|waiting_event  │
│                                             -- |completed|failed           │
│ step_index           INTEGER DEFAULT 0      -- current position            │
│ attempt              INTEGER DEFAULT 0      -- retry counter               │
│ max_attempts         INTEGER DEFAULT 25     -- retry limit                 │
│ next_run_at          TIMESTAMPTZ            -- scheduled execution         │
│ lease_owner          TEXT                   -- worker ID holding lease     │
│ lease_until          TIMESTAMPTZ            -- lease expiry                │
│ waiting_event_key    TEXT                   -- event being awaited         │
│ waiting_deadline     TIMESTAMPTZ            -- wait timeout                │
│ input_json           JSONB                  -- workflow input              │
│ output_json          JSONB                  -- final output (all steps)    │
│ idempotency_key      TEXT                   -- deduplication key           │
│ last_error           TEXT                   -- most recent error           │
│ created_at           TIMESTAMPTZ            -- creation timestamp          │
│ updated_at           TIMESTAMPTZ            -- last modification           │
└──────────────────────────────────┬─────────────────────────────────────────┘
                                   │
                    ┌──────────────┴──────────────┐
                    │                             │
                    ▼                             ▼
┌────────────────────────────────┐  ┌─────────────────────────────────────────┐
│     step_checkpoints           │  │             waiters                     │
├────────────────────────────────┤  ├─────────────────────────────────────────┤
│ PK: (run_id, step_key)         │  │ PK: (event_key, run_id)                 │
│ FK: run_id → workflow_runs     │  │ FK: run_id → workflow_runs              │
│ ─────────────────────────────  │  │ ────────────────────────────────────    │
│ step_key      TEXT NOT NULL    │  │ event_key    TEXT NOT NULL              │
│ value_json    JSONB NOT NULL   │  │ deadline     TIMESTAMPTZ                │
│ error_text    TEXT             │  │ created_at   TIMESTAMPTZ                │
│ completed_at  TIMESTAMPTZ      │  │                                         │
└────────────────────────────────┘  └─────────────────────────────────────────┘


┌────────────────────────────────────────────────────────────────────────────┐
│                    event_log (partitioned by created_at)                   │
├────────────────────────────────────────────────────────────────────────────┤
│ id             BIGINT GENERATED ALWAYS AS IDENTITY                         │
│ event_key      TEXT NOT NULL           -- event identifier                 │
│ payload_json   JSONB NOT NULL          -- event data                       │
│ created_at     TIMESTAMPTZ NOT NULL    -- partition key                    │
│ expires_at     TIMESTAMPTZ             -- TTL for cleanup                  │
├────────────────────────────────────────────────────────────────────────────┤
│ Partitions: event_log_YYYYMM (monthly)                                     │
│ Default:    event_log_default (catches overflow)                           │
└────────────────────────────────────────────────────────────────────────────┘
```

### Indexes

| Index | Purpose | Type |
|-------|---------|------|
| `workflow_runs_ready_idx` | Fast polling for ready work | Partial (state='ready') |
| `workflow_runs_leased_idx` | Expired lease recovery | Partial (state='leased') |
| `workflow_runs_idempotency_uidx` | Enforce idempotency per workflow | Unique partial |
| `step_checkpoints_run_completed_idx` | Load checkpoints by run | Composite |
| `event_log_lookup_idx` | Find events by key | Composite |
| `event_log_expires_idx` | Expired event cleanup | Single column |
| `waiters_deadline_idx` | Timeout processing | Single column |

---

## Execution Model

### Workflow Lifecycle

```
Timeline: Workflow with 3 steps + sleep + event wait
───────────────────────────────────────────────────

t0: Enqueue("order_workflow", {...})
    │
    └─▶ INSERT workflow_runs (state='ready')
        NOTIFY durable_wakeup

t1: Worker claims run
    │
    └─▶ UPDATE state='leased', lease_owner='worker-1'
        Load step_checkpoints (empty)

t2: Execute Step "validate"
    │
    ├─▶ Run validate() function
    ├─▶ INSERT step_checkpoints (step_key='0000:validate')
    └─▶ UPDATE step_index=1

t3: Execute Step "charge"
    │
    ├─▶ Run charge() function
    ├─▶ INSERT step_checkpoints (step_key='0001:charge')
    └─▶ UPDATE step_index=2

t4: Sleep(5 * time.Minute)
    │
    └─▶ UPDATE state='ready', step_index=3, next_run_at=now()+5m
        lease_owner=NULL

t4+5m: Worker claims run (after sleep)
    │
    └─▶ UPDATE state='leased', lease_owner='worker-2'

t4+5m+1: WaitEvent("payment_confirmed", 24h)
    │
    ├─▶ Check event_log for "payment_confirmed" → not found
    ├─▶ INSERT waiters (event_key, run_id, deadline)
    └─▶ UPDATE state='waiting_event', waiting_event_key='payment_confirmed'

t4+6m: External: EmitEvent("payment_confirmed", {...})
    │
    ├─▶ INSERT event_log
    ├─▶ UPDATE workflow_runs SET state='ready' WHERE waiting_event_key='payment_confirmed'
    └─▶ DELETE FROM waiters WHERE event_key='payment_confirmed'
        NOTIFY durable_wakeup

t4+6m+1: Worker claims run
    │
    └─▶ Resume from step_index=4

t4+6m+2: Execute Step "fulfill"
    │
    ├─▶ Run fulfill() function
    ├─▶ INSERT step_checkpoints (step_key='0004:fulfill')
    └─▶ UPDATE state='completed', output_json={...}
```

### Retry with Exponential Backoff

```
Backoff Formula: min(250ms × 2^(attempt-1), 60s)
───────────────────────────────────────────────

Attempt │ Delay        │ Total Wait
────────┼──────────────┼──────────────
   1    │ 250ms        │ 0.25s
   2    │ 500ms        │ 0.75s
   3    │ 1s           │ 1.75s
   4    │ 2s           │ 3.75s
   5    │ 4s           │ 7.75s
   6    │ 8s           │ 15.75s
   7    │ 16s          │ 31.75s
   8    │ 32s          │ 63.75s
   9    │ 60s (cap)    │ 123.75s
  10+   │ 60s          │ +60s each
```

---

## Use Cases

### 1. E-Commerce Order Processing

**Problem**: Order fulfillment involves multiple external services (payment, inventory, shipping) that can fail independently. Manual intervention or ad-hoc retry logic leads to inconsistent state.

**Solution with DurablePG**:

```go
type OrderWorkflow struct{}

func (OrderWorkflow) Name() string { return "order_fulfillment" }

func (OrderWorkflow) Build(b *durablepg.Builder) {
    b.Step("reserve_inventory", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input OrderInput
        sc.DecodeInput(&input)
        return inventoryService.Reserve(ctx, input.Items)
    }, durablepg.WithStepTimeout(30*time.Second))
    
    b.Step("charge_payment", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input OrderInput
        sc.DecodeInput(&input)
        return paymentService.Charge(ctx, input.PaymentMethod, input.Total)
    }, durablepg.WithStepTimeout(60*time.Second))
    
    b.WaitEvent("fraud_check_complete", 2*time.Hour)
    
    b.Step("ship_order", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var reserved InventoryReservation
        sc.Value("reserve_inventory", &reserved)
        return shippingService.CreateShipment(ctx, reserved.Items)
    })
    
    b.Step("send_confirmation", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input OrderInput
        sc.DecodeInput(&input)
        return emailService.SendOrderConfirmation(ctx, input.CustomerEmail)
    })
}
```

**Benefits**:
- Payment charges are never duplicated (checkpoint before ack)
- Fraud check can happen asynchronously via external service
- If shipping fails, retry doesn't re-charge payment
- Order state is always queryable from PostgreSQL

---

### 2. User Onboarding with Verification

**Problem**: Multi-day onboarding flows with email verification, document upload, manual review require tracking state across long time periods.

**Solution**:

```go
type UserOnboardingWorkflow struct{}

func (UserOnboardingWorkflow) Name() string { return "user_onboarding" }

func (UserOnboardingWorkflow) Build(b *durablepg.Builder) {
    b.Step("create_account", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input OnboardingInput
        sc.DecodeInput(&input)
        return userService.CreatePendingAccount(ctx, input)
    })
    
    b.Step("send_verification_email", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var account Account
        sc.Value("create_account", &account)
        return emailService.SendVerification(ctx, account.Email, account.VerificationToken)
    })
    
    b.WaitEvent("email_verified", 72*time.Hour)
    
    b.Step("request_documents", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var account Account
        sc.Value("create_account", &account)
        return documentService.RequestUpload(ctx, account.ID)
    })
    
    b.WaitEvent("documents_uploaded", 7*24*time.Hour)
    
    b.WaitEvent("manual_review_complete", 5*24*time.Hour)
    
    b.Step("activate_account", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var account Account
        sc.Value("create_account", &account)
        return userService.ActivateAccount(ctx, account.ID)
    })
    
    b.Step("send_welcome", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var account Account
        sc.Value("create_account", &account)
        return emailService.SendWelcome(ctx, account.Email)
    })
}
```

**Workflow initiated by**:
```go
engine.Enqueue(ctx, "user_onboarding", OnboardingInput{
    Email: "user@example.com",
    Name:  "John Doe",
}, durablepg.WithIdempotencyKey("user@example.com"))
```

**Events emitted by webhook handlers**:
```go
func handleEmailVerificationWebhook(w http.ResponseWriter, r *http.Request) {
    token := r.URL.Query().Get("token")
    engine.EmitEvent(r.Context(), fmt.Sprintf("email_verified:%s", token), map[string]any{
        "verified_at": time.Now(),
    })
}
```

---

### 3. Distributed Data Pipeline

**Problem**: ETL jobs that process data in stages need to handle partial failures and resume from last checkpoint.

**Solution**:

```go
type DataPipelineWorkflow struct{}

func (DataPipelineWorkflow) Name() string { return "daily_etl" }

func (DataPipelineWorkflow) Build(b *durablepg.Builder) {
    b.Step("extract", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input PipelineInput
        sc.DecodeInput(&input)
        manifest, err := extractService.Extract(ctx, input.SourceBucket, input.Date)
        return manifest, err
    }, durablepg.WithStepTimeout(30*time.Minute))
    
    b.Step("transform", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var manifest ExtractManifest
        sc.Value("extract", &manifest)
        return transformService.Transform(ctx, manifest.Files)
    }, durablepg.WithStepTimeout(2*time.Hour))
    
    b.Step("load", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var transformed TransformResult
        sc.Value("transform", &transformed)
        return loadService.Load(ctx, transformed.OutputFiles, "warehouse.analytics")
    }, durablepg.WithStepTimeout(1*time.Hour))
    
    b.Step("validate", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var loaded LoadResult
        sc.Value("load", &loaded)
        return validationService.RunChecks(ctx, loaded.TableName, loaded.RowCount)
    })
    
    b.Step("notify_completion", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input PipelineInput
        sc.DecodeInput(&input)
        return slack.Send(ctx, "#data-alerts", fmt.Sprintf("ETL for %s complete", input.Date))
    })
}
```

**Scheduled execution**:
```go
engine.Enqueue(ctx, "daily_etl", PipelineInput{
    Date:         "2026-02-12",
    SourceBucket: "s3://data-lake/raw",
}, 
    durablepg.WithScheduledAt(time.Date(2026, 2, 13, 2, 0, 0, 0, time.UTC)),
    durablepg.WithIdempotencyKey("etl:2026-02-12"),
)
```

---

### 4. Saga Pattern for Microservices

**Problem**: Distributed transactions across microservices require compensation logic when downstream services fail.

**Solution**:

```go
type BookingWorkflow struct{}

func (BookingWorkflow) Name() string { return "travel_booking" }

func (BookingWorkflow) Build(b *durablepg.Builder) {
    b.Step("reserve_flight", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input BookingInput
        sc.DecodeInput(&input)
        return flightService.Reserve(ctx, input.FlightID, input.Passengers)
    })
    
    b.Step("reserve_hotel", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input BookingInput
        sc.DecodeInput(&input)
        hotel, err := hotelService.Reserve(ctx, input.HotelID, input.CheckIn, input.CheckOut)
        if err != nil {
            var flightRes FlightReservation
            sc.Value("reserve_flight", &flightRes)
            flightService.Cancel(ctx, flightRes.ConfirmationID)
            return nil, err
        }
        return hotel, nil
    })
    
    b.Step("reserve_car", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input BookingInput
        sc.DecodeInput(&input)
        car, err := carService.Reserve(ctx, input.CarType, input.PickupDate, input.ReturnDate)
        if err != nil {
            var flightRes FlightReservation
            var hotelRes HotelReservation
            sc.Value("reserve_flight", &flightRes)
            sc.Value("reserve_hotel", &hotelRes)
            flightService.Cancel(ctx, flightRes.ConfirmationID)
            hotelService.Cancel(ctx, hotelRes.ConfirmationID)
            return nil, err
        }
        return car, nil
    })
    
    b.Step("charge_total", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var flight FlightReservation
        var hotel HotelReservation
        var car CarReservation
        sc.Value("reserve_flight", &flight)
        sc.Value("reserve_hotel", &hotel)
        sc.Value("reserve_car", &car)
        
        total := flight.Price + hotel.Price + car.Price
        return paymentService.Charge(ctx, total)
    })
    
    b.Step("confirm_all", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var flight FlightReservation
        var hotel HotelReservation  
        var car CarReservation
        sc.Value("reserve_flight", &flight)
        sc.Value("reserve_hotel", &hotel)
        sc.Value("reserve_car", &car)
        
        flightService.Confirm(ctx, flight.ConfirmationID)
        hotelService.Confirm(ctx, hotel.ConfirmationID)
        carService.Confirm(ctx, car.ConfirmationID)
        return map[string]bool{"confirmed": true}, nil
    })
}
```

---

### 5. Background Job Processing with Priorities

**Problem**: Different types of background jobs have different SLAs and should be processed with appropriate priority.

**Solution using multiple queues**:

```go
func setupEngines(pool *pgxpool.Pool) map[string]*durablepg.Engine {
    engines := make(map[string]*durablepg.Engine)
    
    highPriority, _ := durablepg.New(durablepg.Config{
        DB:             pool,
        Queue:          "high",
        MaxConcurrency: 10,
        PollInterval:   100 * time.Millisecond,
    })
    
    normalPriority, _ := durablepg.New(durablepg.Config{
        DB:             pool,
        Queue:          "normal",
        MaxConcurrency: 20,
        PollInterval:   500 * time.Millisecond,
    })
    
    lowPriority, _ := durablepg.New(durablepg.Config{
        DB:             pool,
        Queue:          "low",
        MaxConcurrency: 5,
        PollInterval:   2 * time.Second,
    })
    
    for _, e := range []*durablepg.Engine{highPriority, normalPriority, lowPriority} {
        e.Register(SendEmailWorkflow{})
        e.Register(GenerateReportWorkflow{})
        e.Register(CleanupWorkflow{})
    }
    
    engines["high"] = highPriority
    engines["normal"] = normalPriority
    engines["low"] = lowPriority
    
    return engines
}

func enqueueJob(engines map[string]*durablepg.Engine, priority string, workflow string, input any) {
    engine := engines[priority]
    engine.Enqueue(context.Background(), workflow, input, durablepg.WithQueue(priority))
}
```

---

### 6. Scheduled/Recurring Tasks

**Problem**: Cron-like scheduled tasks need durability and exactly-once execution.

**Solution**:

```go
type DailyReportWorkflow struct{}

func (DailyReportWorkflow) Name() string { return "daily_report" }

func (DailyReportWorkflow) Build(b *durablepg.Builder) {
    b.Step("generate", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var input ReportInput
        sc.DecodeInput(&input)
        return reportService.Generate(ctx, input.ReportType, input.Date)
    })
    
    b.Step("store", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var report Report
        sc.Value("generate", &report)
        return s3Service.Upload(ctx, report.Data, report.Filename)
    })
    
    b.Step("notify", func(ctx context.Context, sc *durablepg.StepContext) (any, error) {
        var stored S3Object
        sc.Value("store", &stored)
        return emailService.SendReportReady(ctx, stored.URL)
    })
}

func scheduleNextDayReport(engine *durablepg.Engine, reportType string) {
    tomorrow := time.Now().AddDate(0, 0, 1)
    scheduledAt := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 6, 0, 0, 0, time.UTC)
    
    engine.Enqueue(context.Background(), "daily_report", ReportInput{
        ReportType: reportType,
        Date:       tomorrow.Format("2006-01-02"),
    },
        durablepg.WithScheduledAt(scheduledAt),
        durablepg.WithIdempotencyKey(fmt.Sprintf("%s:%s", reportType, tomorrow.Format("2006-01-02"))),
    )
}
```

---

## Scalability

### Horizontal Scaling

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        HORIZONTAL SCALING TOPOLOGY                           │
└─────────────────────────────────────────────────────────────────────────────┘

              Load Balancer / API Gateway
                        │
    ┌───────────────────┼───────────────────┐
    │                   │                   │
    ▼                   ▼                   ▼
┌─────────┐       ┌─────────┐       ┌─────────┐
│ App Pod │       │ App Pod │       │ App Pod │
│         │       │         │       │         │
│ Engine  │       │ Engine  │       │ Engine  │
│ Worker  │       │ Worker  │       │ Worker  │
│(conc=10)│       │(conc=10)│       │(conc=10)│
└────┬────┘       └────┬────┘       └────┬────┘
     │                 │                 │
     │   pgxpool       │                 │
     │   connections   │                 │
     └─────────────────┼─────────────────┘
                       │
                       ▼
              ┌─────────────────┐
              │   PostgreSQL    │
              │                 │
              │  - PgBouncer    │
              │  - Read Replicas│
              │  - Partitioning │
              └─────────────────┘

Scaling Equation:
─────────────────
Total Concurrency = Pods × MaxConcurrency per Pod
                  = 3 × 10 = 30 parallel workflow executions
```

### Key Scaling Mechanisms

#### 1. `SELECT FOR UPDATE SKIP LOCKED`

```sql
WITH picked AS (
    SELECT id
    FROM durable.workflow_runs
    WHERE queue = 'default'
      AND state = 'ready'
      AND next_run_at <= now()
    ORDER BY next_run_at, id
    LIMIT 10
    FOR UPDATE SKIP LOCKED          -- Non-blocking concurrent access
)
UPDATE durable.workflow_runs wr
SET state = 'leased', lease_owner = 'worker-xyz'
FROM picked WHERE wr.id = picked.id;
```

**Why this scales**:
- Workers don't block each other when claiming work
- No central coordinator required
- PostgreSQL handles contention efficiently
- Each worker gets distinct subset of work

#### 2. Queue Isolation

```
Queue: "high_priority"              Queue: "batch_jobs"
┌─────────────────────┐             ┌─────────────────────┐
│ Workers: 20         │             │ Workers: 5          │
│ Poll: 100ms         │             │ Poll: 2s            │
│ Lease TTL: 30s      │             │ Lease TTL: 5min     │
│                     │             │                     │
│ Use case:           │             │ Use case:           │
│ - User requests     │             │ - Nightly ETL       │
│ - Payments          │             │ - Report generation │
│ - Real-time alerts  │             │ - Data migrations   │
└─────────────────────┘             └─────────────────────┘
```

#### 3. Event Log Partitioning

```sql
-- Automatic monthly partitions
CREATE TABLE durable.event_log_202602
PARTITION OF durable.event_log
FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

-- Benefits:
-- 1. Fast expiry cleanup (DROP old partitions)
-- 2. Smaller index sizes per partition
-- 3. Parallel query execution across partitions
-- 4. Hot/cold data separation
```

### Scaling Recommendations by Load

| Daily Workflows | Workers | DB Connections | PostgreSQL Setup |
|-----------------|---------|----------------|------------------|
| < 10,000 | 1-2 pods, 5-10 concurrency | 20-50 | Single instance |
| 10,000 - 100,000 | 3-5 pods, 10-20 concurrency | 50-100 | Primary + read replica |
| 100,000 - 1M | 5-20 pods, 20-50 concurrency | 100-500 | Primary + replicas + PgBouncer |
| > 1M | 20+ pods | 500+ with pooler | Citus/sharding or multiple clusters |

### Performance Tuning Parameters

```go
engine, _ := durablepg.New(durablepg.Config{
    DB:                pool,
    
    MaxConcurrency:    50,
    
    PollInterval:      100 * time.Millisecond,
    
    LeaseTTL:          30 * time.Second,
    
    HeartbeatInterval: 5 * time.Second,
})
```

### Bottleneck Analysis

```
Potential Bottleneck    │ Symptom                      │ Solution
────────────────────────┼──────────────────────────────┼─────────────────────────
PostgreSQL connections  │ "connection refused" errors  │ Add PgBouncer, increase
                        │ High p99 latency             │ max_connections
────────────────────────┼──────────────────────────────┼─────────────────────────
Lock contention         │ Long UPDATE times            │ Reduce batch size,
                        │ High lock_wait_time          │ add more queues
────────────────────────┼──────────────────────────────┼─────────────────────────
Index bloat             │ Slow ready_idx scans         │ Regular VACUUM,
                        │ Large table size             │ partition workflow_runs
────────────────────────┼──────────────────────────────┼─────────────────────────
Event log growth        │ Large table size             │ Reduce eventTTL,
                        │ Slow event lookups           │ ensure partitions exist
────────────────────────┼──────────────────────────────┼─────────────────────────
Checkpoint writes       │ High INSERT latency          │ Batch checkpoints,
                        │ WAL growth                   │ async checkpoints
```

---

## Comparison with Alternatives

### Feature Comparison Matrix

| Feature | DurablePG | Temporal | AWS Step Functions | Celery | pg-boss |
|---------|-----------|----------|-------------------|--------|---------|
| **Infrastructure** | PostgreSQL only | Temporal Server + DB | AWS managed | Redis + broker | PostgreSQL |
| **Code-first workflows** | Yes | Yes | No (JSON/YAML) | Partial | No |
| **Distributed execution** | Yes | Yes | Yes | Yes | Yes |
| **Exactly-once steps** | Yes | Yes | Yes | No | Yes |
| **Event-driven wait** | Yes | Yes | Yes | No | No |
| **Scheduled execution** | Yes | Yes | Yes | Yes | Yes |
| **Long-running (days/weeks)** | Yes | Yes | Yes | No | Limited |
| **Query workflow state** | SQL | Temporal API | CloudWatch | Backend-specific | SQL |
| **Complexity** | Low | High | Medium | Low | Low |
| **Vendor lock-in** | None | None | AWS | None | None |
| **Self-hosted cost** | PostgreSQL only | High (server cluster) | N/A | Moderate | PostgreSQL only |

### When to Choose DurablePG

**Choose DurablePG when**:
- You already have PostgreSQL in your stack
- You want simplicity over feature completeness
- Your workflows are moderate complexity (< 50 steps)
- You need SQL queryability for workflow state
- You want to avoid additional infrastructure
- Team is comfortable with Go
- Vendor lock-in is a concern

**Consider Temporal when**:
- You need child workflows and complex orchestration
- You require versioning and replay debugging
- You have very high throughput (millions/day)
- You need multi-language support
- You have dedicated platform team

**Consider AWS Step Functions when**:
- You're already heavily invested in AWS
- You want managed infrastructure
- You prefer visual workflow designer
- You need tight AWS service integration

**Consider Celery when**:
- You're in Python ecosystem
- You need simple task queues, not workflows
- You don't need exactly-once semantics
- You already have Redis/RabbitMQ

### Architectural Trade-offs

```
                    Simplicity ◀──────────────────────▶ Features
                          │
    DurablePG ────────────┼
                          │
    pg-boss ──────────────┼
                          │
    Celery ───────────────┼
                          │
    Step Functions ───────┼──────────────────────
                          │
    Temporal ─────────────┼──────────────────────────────
                          │
                          ▼
              Infrastructure Complexity
```

---

## API Reference

### Engine Configuration

```go
type Config struct {
    DB                *pgxpool.Pool
    Schema            string
    Queue             string
    MaxConcurrency    int
    PollInterval      time.Duration
    LeaseTTL          time.Duration
    HeartbeatInterval time.Duration
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `DB` | (required) | PostgreSQL connection pool |
| `Schema` | `"durable"` | Database schema name |
| `Queue` | `"default"` | Default queue for this engine |
| `MaxConcurrency` | `runtime.GOMAXPROCS(0)` | Max parallel workflow executions |
| `PollInterval` | `250ms` | How often to check for ready work |
| `LeaseTTL` | `30s` | How long a worker holds a run |
| `HeartbeatInterval` | `10s` | How often to renew lease (must be < LeaseTTL) |

### Engine Methods

```go
func New(cfg Config) (*Engine, error)

func (e *Engine) Init(ctx context.Context) error

func (e *Engine) ApplySchema(ctx context.Context) error

func (e *Engine) Register(wf Workflow)

func (e *Engine) Enqueue(ctx context.Context, name string, input any, opts ...EnqueueOption) (WorkflowID, error)

func (e *Engine) EmitEvent(ctx context.Context, key string, payload any) error

func (e *Engine) StartWorker(ctx context.Context) error

func (e *Engine) EnsurePartitions(ctx context.Context, monthsAhead int) error
```

### Enqueue Options

```go
durablepg.WithRunID(id WorkflowID)

durablepg.WithQueue(queue string)

durablepg.WithMaxAttempts(n int)

durablepg.WithIdempotencyKey(key string)

durablepg.WithScheduledAt(at time.Time)
```

### Builder Methods

```go
func (b *Builder) Step(name string, fn StepFunc, opts ...StepOption)

func (b *Builder) Sleep(d time.Duration)

func (b *Builder) WaitEvent(key string, timeout time.Duration)
```

### Step Options

```go
durablepg.WithStepTimeout(timeout time.Duration)
```

### StepContext Methods

```go
func (sc *StepContext) DecodeInput(dst any) error

func (sc *StepContext) Value(step string, dst any) (bool, error)

func (sc *StepContext) RawValue(step string) (json.RawMessage, bool)
```

---

## Production Considerations

### Monitoring Queries

```sql
SELECT state, COUNT(*) 
FROM durable.workflow_runs 
GROUP BY state;

SELECT workflow_name, state, COUNT(*) 
FROM durable.workflow_runs 
WHERE created_at > now() - INTERVAL '24 hours'
GROUP BY workflow_name, state;

SELECT id, workflow_name, step_index, attempt, last_error, created_at
FROM durable.workflow_runs
WHERE state = 'ready' 
  AND next_run_at < now() - INTERVAL '5 minutes';

SELECT workflow_name, 
       AVG(EXTRACT(EPOCH FROM (updated_at - created_at))) as avg_seconds
FROM durable.workflow_runs
WHERE state = 'completed'
  AND created_at > now() - INTERVAL '1 hour'
GROUP BY workflow_name;

SELECT id, workflow_name, last_error, attempt, max_attempts
FROM durable.workflow_runs
WHERE state = 'failed'
ORDER BY updated_at DESC
LIMIT 100;
```

### Maintenance Tasks

```sql
DELETE FROM durable.workflow_runs
WHERE state IN ('completed', 'failed')
  AND updated_at < now() - INTERVAL '30 days';

VACUUM ANALYZE durable.workflow_runs;
VACUUM ANALYZE durable.step_checkpoints;

SELECT pg_size_pretty(pg_total_relation_size('durable.workflow_runs'));
SELECT pg_size_pretty(pg_total_relation_size('durable.event_log'));
```

### Graceful Shutdown

```go
func main() {
    ctx, cancel := context.WithCancel(context.Background())
    
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    
    go func() {
        <-sigCh
        log.Println("Shutting down gracefully...")
        cancel()
    }()
    
    if err := engine.StartWorker(ctx); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatal(err)
    }
    
    log.Println("Worker stopped")
}
```

### Health Check Endpoint

```go
func healthHandler(engine *durablepg.Engine, pool *pgxpool.Pool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
        defer cancel()
        
        var one int
        if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
            http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
            return
        }
        
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
    }
}
```

---

## Summary

DurablePG provides a **pragmatic, PostgreSQL-native approach** to durable workflow execution. By leveraging PostgreSQL's ACID guarantees, row-level locking, and LISTEN/NOTIFY, it achieves:

1. **Simplicity**: No additional infrastructure beyond PostgreSQL
2. **Durability**: Step results survive crashes and restarts
3. **Scalability**: Horizontal scaling via SKIP LOCKED and queue isolation
4. **Observability**: Full SQL queryability of workflow state
5. **Flexibility**: Event-driven workflows with configurable timeouts

For teams already invested in PostgreSQL, DurablePG offers a compelling alternative to heavier workflow orchestration systems while maintaining production-grade reliability.
