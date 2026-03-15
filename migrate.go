package durablepg

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ApplySchema creates all durable workflow tables and indexes.
func (e *Engine) ApplySchema(ctx context.Context) error {
	_, err := e.db.Exec(ctx, e.SchemaSQL())
	if err != nil {
		return fmt.Errorf("durablepg: apply schema: %w", err)
	}
	_ = e.postgresVersion(ctx)
	return nil
}

// EnsurePartitions calls the SQL function to create monthly event_log partitions.
// monthsAhead must be between 0 and 120 (10 years).
func (e *Engine) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	if monthsAhead < 0 || monthsAhead > 120 {
		return fmt.Errorf("durablepg: monthsAhead must be between 0 and 120, got %d", monthsAhead)
	}
	query := fmt.Sprintf("SELECT %s.ensure_event_log_partitions($1)", e.qSchema)
	_, err := e.db.Exec(ctx, query, monthsAhead)
	if err != nil {
		return fmt.Errorf("durablepg: ensure partitions: %w", err)
	}
	return nil
}

// Init applies the schema and creates event_log partitions for the next 12 months.
func (e *Engine) Init(ctx context.Context) error {
	if err := e.ApplySchema(ctx); err != nil {
		return err
	}
	return e.EnsurePartitions(ctx, 12)
}

// Migrate is kept for compatibility.
//
// Deprecated: use ApplySchema.
func (e *Engine) Migrate(ctx context.Context) error {
	return e.ApplySchema(ctx)
}

// SchemaSQL returns the schema DDL used by ApplySchema.
func (e *Engine) SchemaSQL() string {
	return fmt.Sprintf(`
CREATE SCHEMA IF NOT EXISTS %[1]s;

CREATE TABLE IF NOT EXISTS %[2]s (
	id TEXT PRIMARY KEY,
	workflow_name TEXT NOT NULL,
	queue TEXT NOT NULL,
	state TEXT NOT NULL,
	step_index INTEGER NOT NULL DEFAULT 0,
	attempt INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 25,
	next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	lease_owner TEXT,
	lease_until TIMESTAMPTZ,
	waiting_event_key TEXT,
	waiting_deadline TIMESTAMPTZ,
	input_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	output_json JSONB,
	idempotency_key TEXT,
	last_error TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS %[3]s (
	run_id TEXT NOT NULL REFERENCES %[2]s(id) ON DELETE CASCADE,
	step_key TEXT NOT NULL,
	value_json JSONB NOT NULL,
	error_text TEXT,
	completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (run_id, step_key)
);

CREATE TABLE IF NOT EXISTS %[4]s (
	id BIGINT GENERATED ALWAYS AS IDENTITY,
	event_key TEXT NOT NULL,
	payload_json JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS %[6]s
PARTITION OF %[4]s DEFAULT;

CREATE TABLE IF NOT EXISTS %[5]s (
	event_key TEXT NOT NULL,
	run_id TEXT NOT NULL REFERENCES %[2]s(id) ON DELETE CASCADE,
	deadline TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (event_key, run_id)
);

CREATE INDEX IF NOT EXISTS workflow_runs_ready_idx
	ON %[2]s (queue, next_run_at, id)
	WHERE state = 'ready';

CREATE INDEX IF NOT EXISTS workflow_runs_leased_idx
	ON %[2]s (lease_until)
	WHERE state = 'leased';

CREATE UNIQUE INDEX IF NOT EXISTS workflow_runs_idempotency_uidx
	ON %[2]s (workflow_name, idempotency_key)
	WHERE idempotency_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS step_checkpoints_run_completed_idx
	ON %[3]s (run_id, completed_at DESC);

CREATE INDEX IF NOT EXISTS event_log_lookup_idx
	ON %[4]s (event_key, created_at DESC);

CREATE INDEX IF NOT EXISTS event_log_expires_idx
	ON %[4]s (expires_at);

CREATE INDEX IF NOT EXISTS waiters_deadline_idx
	ON %[5]s (deadline);

CREATE OR REPLACE FUNCTION %[1]s.ensure_event_log_partitions(months_ahead INT DEFAULT 3)
RETURNS void AS $fn$
DECLARE
	partition_date DATE;
	partition_name TEXT;
	start_bound TIMESTAMPTZ;
	end_bound TIMESTAMPTZ;
BEGIN
	FOR i IN 0..months_ahead LOOP
		partition_date := date_trunc('month', CURRENT_DATE + (i || ' months')::interval);
		partition_name := 'event_log_' || to_char(partition_date, 'YYYYMM');
		start_bound := partition_date;
		end_bound := partition_date + '1 month'::interval;

		IF NOT EXISTS (
			SELECT 1 FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = '%[7]s' AND c.relname = partition_name
		) THEN
			EXECUTE format(
				'CREATE TABLE %%I.%%I PARTITION OF %%I.event_log FOR VALUES FROM (%%L) TO (%%L)',
				'%[7]s', partition_name, '%[7]s', start_bound, end_bound
			);
		END IF;
	END LOOP;
END;
$fn$ LANGUAGE plpgsql;
`,
		e.qSchema,
		e.table("workflow_runs"),
		e.table("step_checkpoints"),
		e.table("event_log"),
		e.table("waiters"),
		e.table("event_log_default"),
		e.schema,
	)
}

// EventLogMonthlyPartitionsSQL returns DDL to create monthly event_log partitions.
func (e *Engine) EventLogMonthlyPartitionsSQL(from time.Time, months int) (string, error) {
	if months <= 0 {
		return "", nil
	}
	from = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)

	var ddl strings.Builder
	for i := 0; i < months; i++ {
		start := from.AddDate(0, i, 0)
		end := start.AddDate(0, 1, 0)
		name := fmt.Sprintf("event_log_%04d%02d", start.Year(), int(start.Month()))
		stmt := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s
PARTITION OF %s
FOR VALUES FROM (%s) TO (%s);
`, e.table(name), e.table("event_log"), pgTimestampLiteral(start), pgTimestampLiteral(end))
		ddl.WriteString(stmt)
	}
	return ddl.String(), nil
}

// CreateEventLogMonthlyPartitions executes DDL for monthly event_log partitions.
func (e *Engine) CreateEventLogMonthlyPartitions(ctx context.Context, from time.Time, months int) error {
	ddl, err := e.EventLogMonthlyPartitionsSQL(from, months)
	if err != nil {
		return err
	}
	if ddl == "" {
		return nil
	}
	if _, err := e.db.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("durablepg: create event partitions: %w", err)
	}
	return nil
}

// EnsureEventLogMonthlyPartitions is kept for compatibility.
//
// Deprecated: use EventLogMonthlyPartitionsSQL or CreateEventLogMonthlyPartitions.
func (e *Engine) EnsureEventLogMonthlyPartitions(ctx context.Context, from time.Time, months int) error {
	if err := e.CreateEventLogMonthlyPartitions(ctx, from, months); err != nil {
		return err
	}
	return nil
}

func pgTimestampLiteral(ts time.Time) string {
	// Timestamp values are generated internally and rendered as SQL literals
	// because PostgreSQL does not support bind parameters in partition bounds.
	v := ts.UTC().Format("2006-01-02 15:04:05.999999999Z07:00")
	v = strings.ReplaceAll(v, "'", "''")
	return "'" + v + "'::timestamptz"
}
