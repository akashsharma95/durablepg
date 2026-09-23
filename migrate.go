package durablepg

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ApplySchema advances the schema through ordered, transactional migrations.
// A pre-versioned installation is treated as version 0; the baseline migration
// uses idempotent DDL so existing rows are retained.
func (e *Engine) ApplySchema(ctx context.Context) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("durablepg: begin schema migration: %w", err)
	}
	defer tx.Rollback(context.Background())

	// Serialize even the first installation, before the migrations table exists.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "durablepg:schema:"+e.schema); err != nil {
		return fmt.Errorf("durablepg: lock schema migration: %w", err)
	}
	if _, err = tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+e.qSchema); err != nil {
		return fmt.Errorf("durablepg: create schema: %w", err)
	}
	migrations := e.table("schema_migrations")
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+migrations+" (version INTEGER PRIMARY KEY)"); err != nil {
		return fmt.Errorf("durablepg: create migration history: %w", err)
	}
	migrationSQL := []string{e.schemaBaselineSQL(), e.waitingDeadlineIndexSQL(), e.workflowVersionSQL(), e.selectiveClaimIndexSQL()}
	rows, err := tx.Query(ctx, "SELECT version FROM "+migrations+" ORDER BY version")
	if err != nil {
		return fmt.Errorf("durablepg: read migration history: %w", err)
	}
	version := 0
	for rows.Next() {
		var recorded int
		if err = rows.Scan(&recorded); err != nil {
			break
		}
		if recorded != version+1 || recorded > len(migrationSQL) {
			err = fmt.Errorf("durablepg: unsupported schema migration version %d after %d", recorded, version)
			break
		}
		version = recorded
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return fmt.Errorf("durablepg: read migration history: %w", err)
	}
	for version < len(migrationSQL) {
		if _, err = tx.Exec(ctx, migrationSQL[version]); err != nil {
			return fmt.Errorf("durablepg: apply schema migration %d: %w", version+1, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+migrations+" (version) VALUES ($1)", version+1); err != nil {
			return fmt.Errorf("durablepg: record schema migration %d: %w", version+1, err)
		}
		version++
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("durablepg: commit schema migration: %w", err)
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

// SchemaSQL returns the complete fresh-install DDL, including all migrations.
// ApplySchema separately records each migration in schema_migrations.
func (e *Engine) SchemaSQL() string {
	return e.schemaBaselineSQL() + e.waitingDeadlineIndexSQL() + e.workflowVersionSQL() + e.selectiveClaimIndexSQL()
}

func (e *Engine) waitingDeadlineIndexSQL() string {
	return fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS workflow_runs_waiting_deadline_idx
	ON %s (waiting_deadline)
	WHERE state = 'waiting_event';
`, e.table("workflow_runs"))
}

func (e *Engine) workflowVersionSQL() string {
	return fmt.Sprintf(`
ALTER TABLE %s ADD COLUMN IF NOT EXISTS workflow_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE %s ADD COLUMN IF NOT EXISTS lease_failures INTEGER NOT NULL DEFAULT 0;
`, e.table("workflow_runs"), e.table("workflow_runs")) + e.partitionFunctionsSQL()
}

func (e *Engine) selectiveClaimIndexSQL() string {
	return fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS workflow_runs_selective_ready_idx
	ON %s (queue, workflow_name, workflow_version, next_run_at, id)
	WHERE state = 'ready';
`, e.table("workflow_runs"))
}

func (e *Engine) schemaBaselineSQL() string {
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

`,
		e.qSchema,
		e.table("workflow_runs"),
		e.table("step_checkpoints"),
		e.table("event_log"),
		e.table("waiters"),
		e.table("event_log_default"),
	) + e.partitionFunctionsSQL()
}

func (e *Engine) partitionFunctionsSQL() string {
	return fmt.Sprintf(`
-- A common path keeps function-driven and generated partition DDL equivalent.
-- Parent and default locks exclude both routed and direct writes while rows
-- are moved; a failure rolls the entire move and partition creation back.
CREATE OR REPLACE FUNCTION %[1]s.create_event_log_partition(start_bound TIMESTAMPTZ, end_bound TIMESTAMPTZ)
RETURNS void AS $fn$
DECLARE
	partition_name TEXT := 'event_log_' || to_char(start_bound AT TIME ZONE 'UTC', 'YYYYMM');
BEGIN
	-- A catalog check avoids table-wide locks for the usual already-created month.
	-- The locked check below handles concurrent creators.
	IF EXISTS (
		SELECT 1 FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_inherits inh ON inh.inhrelid = c.oid
		WHERE n.nspname = '%[4]s' AND c.relname = partition_name
		  AND inh.inhparent = '%[2]s'::regclass
	) THEN
		RETURN;
	END IF;

	LOCK TABLE %[2]s IN ACCESS EXCLUSIVE MODE;
	LOCK TABLE %[3]s IN ACCESS EXCLUSIVE MODE;
	IF EXISTS (
		SELECT 1 FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_inherits inh ON inh.inhrelid = c.oid
		WHERE n.nspname = '%[4]s' AND c.relname = partition_name
		  AND inh.inhparent = '%[2]s'::regclass
	) THEN
		RETURN;
	END IF;

	-- DDL cannot carve a range out of a populated default partition. Staging
	-- in a transaction-local table preserves identity values and all row data.
	CREATE TEMP TABLE durablepg_repartition_rows ON COMMIT DROP AS
		SELECT id, event_key, payload_json, created_at, expires_at
		FROM %[3]s WITH NO DATA;
	WITH moved AS (
		DELETE FROM %[3]s
		WHERE created_at >= start_bound AND created_at < end_bound
		RETURNING id, event_key, payload_json, created_at, expires_at
	)
	INSERT INTO pg_temp.durablepg_repartition_rows
	SELECT * FROM moved;
	EXECUTE format(
		'CREATE TABLE %%I.%%I PARTITION OF %%I.event_log FOR VALUES FROM (%%L) TO (%%L)',
		'%[4]s', partition_name, '%[4]s', start_bound, end_bound
	);
	INSERT INTO %[2]s (id, event_key, payload_json, created_at, expires_at)
		OVERRIDING SYSTEM VALUE
		SELECT id, event_key, payload_json, created_at, expires_at
		FROM pg_temp.durablepg_repartition_rows;
	DROP TABLE pg_temp.durablepg_repartition_rows;
END;
$fn$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION %[1]s.ensure_event_log_partitions(months_ahead INT DEFAULT 3)
RETURNS void AS $fn$
DECLARE
	partition_date DATE;
	start_bound TIMESTAMPTZ;
BEGIN
	FOR i IN 0..months_ahead LOOP
		partition_date := date_trunc('month', now() AT TIME ZONE 'UTC')::date + make_interval(months => i);
		start_bound := partition_date::timestamp AT TIME ZONE 'UTC';
		IF NOT EXISTS (
			SELECT 1 FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_inherits inh ON inh.inhrelid = c.oid
			WHERE n.nspname = '%[4]s'
			  AND c.relname = 'event_log_' || to_char(start_bound AT TIME ZONE 'UTC', 'YYYYMM')
			  AND inh.inhparent = '%[2]s'::regclass
		) THEN
			PERFORM %[1]s.create_event_log_partition(
				start_bound,
				(partition_date + INTERVAL '1 month')::timestamp AT TIME ZONE 'UTC'
			);
		END IF;
	END LOOP;
END;
$fn$ LANGUAGE plpgsql;
`,
		e.qSchema,
		e.table("event_log"),
		e.table("event_log_default"),
		e.schema,
	)
}

// EventLogMonthlyPartitionsSQL returns calls to the same transactional helper
// used by EnsurePartitions. Execute the DDL only after applying the schema.
func (e *Engine) EventLogMonthlyPartitionsSQL(from time.Time, months int) (string, error) {
	if months <= 0 {
		return "", nil
	}
	from = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)

	var ddl strings.Builder
	for i := range months {
		start := from.AddDate(0, i, 0)
		end := start.AddDate(0, 1, 0)
		stmt := fmt.Sprintf("SELECT %s.create_event_log_partition(%s, %s);\n",
			e.qSchema, pgTimestampLiteral(start), pgTimestampLiteral(end))
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
