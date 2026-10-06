package durablepg

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupportedSchema reports a schema migrated by a newer library version.
var ErrUnsupportedSchema = errors.New("durablepg: unsupported schema version")

// ApplySchema creates or upgrades the schema through ordered, transactional
// migrations. It is safe to call on every start and from concurrent processes.
// A schema migrated by a newer library version is refused.
func (e *Engine) ApplySchema(ctx context.Context) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("durablepg: begin schema migration: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck

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
	migrationSQL := e.migrations()
	var version int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version), 0) FROM "+migrations).Scan(&version); err != nil {
		return fmt.Errorf("durablepg: read migration history: %w", err)
	}
	if version > len(migrationSQL) {
		return fmt.Errorf("%w: schema %q is at migration %d; this version supports up to %d", ErrUnsupportedSchema, e.schema, version, len(migrationSQL))
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
	return nil
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
	return "CREATE SCHEMA IF NOT EXISTS " + e.qSchema + ";\n" + strings.Join(e.migrations(), "")
}

// migrations returns ordered DDL; index i is version i+1.
func (e *Engine) migrations() []string {
	return []string{fmt.Sprintf(`
CREATE TABLE %[1]s.workflow_runs (
	id UUID PRIMARY KEY,
	workflow_name TEXT NOT NULL,
	workflow_version INTEGER NOT NULL CHECK (workflow_version > 0),
	queue TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('ready', 'leased', 'waiting_event', 'completed', 'failed', 'cancelled')),
	step_index INTEGER NOT NULL DEFAULT 0,
	attempt INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
	lease_failures INTEGER NOT NULL DEFAULT 0,
	next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	lease_token UUID,
	lease_until TIMESTAMPTZ,
	waiting_event_key TEXT,
	waiting_deadline TIMESTAMPTZ,
	input_json JSONB NOT NULL,
	output_json JSONB,
	dedup_key TEXT,
	last_error TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Multi-definition workers scan by queue and due time.
CREATE INDEX workflow_runs_ready_idx
	ON %[1]s.workflow_runs (queue, next_run_at, id)
	WHERE state = 'ready';

-- Single-definition workers use equality predicates on this index.
CREATE INDEX workflow_runs_selective_ready_idx
	ON %[1]s.workflow_runs (queue, workflow_name, workflow_version, next_run_at, id)
	WHERE state = 'ready';

CREATE INDEX workflow_runs_leased_idx
	ON %[1]s.workflow_runs (lease_until)
	WHERE state = 'leased';

CREATE INDEX workflow_runs_waiting_key_idx
	ON %[1]s.workflow_runs (waiting_event_key)
	WHERE state = 'waiting_event';

CREATE INDEX workflow_runs_waiting_deadline_idx
	ON %[1]s.workflow_runs (waiting_deadline)
	WHERE state = 'waiting_event';

CREATE UNIQUE INDEX workflow_runs_dedup_uidx
	ON %[1]s.workflow_runs (workflow_name, dedup_key)
	WHERE dedup_key IS NOT NULL;

CREATE TABLE %[1]s.step_checkpoints (
	run_id UUID NOT NULL REFERENCES %[1]s.workflow_runs (id) ON DELETE CASCADE,
	step_index INTEGER NOT NULL,
	value_json JSONB NOT NULL,
	completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (run_id, step_index)
);

CREATE TABLE %[1]s.event_log (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	event_key TEXT NOT NULL,
	payload_json JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX event_log_key_idx ON %[1]s.event_log (event_key, created_at DESC, id DESC);
CREATE INDEX event_log_expires_idx ON %[1]s.event_log (expires_at);
`, e.qSchema)}
}
