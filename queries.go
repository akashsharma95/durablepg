package durablepg

import "fmt"

// Every statement that changes a leased run is fenced by
// state = 'leased' AND lease_token = $token AND lease_until > clock_timestamp(),
// so a worker whose lease expired or was replaced cannot write progress.
const fence = "id = $1 AND state = 'leased' AND lease_token = $2 AND lease_until > clock_timestamp()"

const (
	// Rows handled by one maintenance statement.
	maintenanceBatch = 1024
	// Rows deleted by one event-retention statement.
	pruneBatch = 2048
)

// queries holds every statement, rendered once per engine for its schema.
// Schema names are validated identifiers and every value is a bind parameter.
type queries struct {
	lockEventKey    string
	insertRun       string
	emitEvent       string
	claimSingle     string
	claimMulti      string
	renewLeases     string
	commitStep      string
	completeRun     string
	parkSleep       string
	latestEvent     string
	parkWait        string
	failOrRetry     string
	recoverLeases   string
	promoteTimedOut string
	pruneEvents     string
	runStatus       string
	runOutput       string
	cancelRun       string
	runState        string
}

func newQueries(qSchema string) queries {
	runs := qSchema + ".workflow_runs"
	checkpoints := qSchema + ".step_checkpoints"
	events := qSchema + ".event_log"
	claim := func(predicate string) string {
		return fmt.Sprintf(`
WITH picked AS (
	SELECT id FROM %[1]s
	WHERE queue = $1 AND state = 'ready' AND next_run_at <= now() AND %[3]s
	ORDER BY next_run_at, id
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
UPDATE %[1]s wr
SET state = 'leased',
	lease_token = gen_random_uuid(),
	lease_until = clock_timestamp() + $3::bigint * INTERVAL '1 millisecond',
	updated_at = now()
FROM picked
WHERE wr.id = picked.id
RETURNING wr.id::text, wr.workflow_name, wr.workflow_version, wr.step_index, wr.attempt,
	wr.lease_token::text, wr.input_json,
	(SELECT COALESCE(jsonb_agg(jsonb_build_array(c.step_index, c.value_json)), '[]'::jsonb)
	 FROM %[2]s c WHERE c.run_id = wr.id) AS checkpoints`, runs, checkpoints, predicate)
	}
	return queries{
		lockEventKey: "SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))",
		// ON CONFLICT DO UPDATE, unlike DO NOTHING, returns the existing row
		// even when it committed after this statement's snapshot.
		insertRun: fmt.Sprintf(`
WITH ins AS (
	INSERT INTO %[1]s (id, workflow_name, workflow_version, queue, state, max_attempts,
		next_run_at, input_json, dedup_key)
	VALUES ($1, $2, $3, $4, 'ready', $5, COALESCE($6, now()), $7, $8)
	ON CONFLICT (workflow_name, dedup_key) WHERE dedup_key IS NOT NULL
	DO UPDATE SET updated_at = now()
	RETURNING id, queue, next_run_at, (xmax = 0) AS inserted
), notified AS (
	SELECT pg_notify($9, queue) FROM ins WHERE inserted AND next_run_at <= now()
)
SELECT id::text, (SELECT count(*) FROM notified) FROM ins`, runs),
		// Runs after the per-key advisory lock, in a statement whose snapshot
		// includes any waiter registered before the lock.
		emitEvent: fmt.Sprintf(`
WITH event AS (
	INSERT INTO %[3]s (event_key, payload_json, expires_at)
	VALUES ($1, $2, now() + $3::bigint * INTERVAL '1 millisecond')
), woken AS (
	UPDATE %[1]s
	SET state = 'ready', next_run_at = now(), waiting_event_key = NULL,
		waiting_deadline = NULL, last_error = NULL, updated_at = now()
	WHERE state = 'waiting_event' AND waiting_event_key = $1 AND waiting_deadline > now()
	RETURNING id, step_index, queue
), recorded AS (
	INSERT INTO %[2]s (run_id, step_index, value_json)
	SELECT id, step_index, jsonb_build_object('received', $2::jsonb) FROM woken
	ON CONFLICT (run_id, step_index) DO NOTHING
), notified AS (
	SELECT pg_notify($4, queue) FROM (SELECT DISTINCT queue FROM woken) q
)
SELECT (SELECT count(*) FROM woken), (SELECT count(*) FROM notified)`, runs, checkpoints, events),
		claimSingle: claim("workflow_name = $4 AND workflow_version = $5"),
		claimMulti:  claim("(workflow_name, workflow_version) IN (SELECT * FROM unnest($4::text[], $5::integer[]))"),
		renewLeases: fmt.Sprintf(`
UPDATE %[1]s wr
SET lease_until = clock_timestamp() + $3::bigint * INTERVAL '1 millisecond', updated_at = now()
FROM unnest($1::uuid[], $2::uuid[]) AS claim(id, token)
WHERE wr.id = claim.id AND wr.lease_token = claim.token
  AND wr.state = 'leased' AND wr.lease_until > clock_timestamp()
RETURNING wr.lease_token::text`, runs),
		// Records the first result at this position and advances the cursor
		// atomically. A checkpoint left by an earlier claim wins and is
		// returned. With $5, the same statement completes the run.
		commitStep: fmt.Sprintf(`
WITH owned AS MATERIALIZED (
	SELECT id FROM %[1]s WHERE %[3]s AND step_index <= $3 FOR UPDATE
), inserted AS (
	INSERT INTO %[2]s (run_id, step_index, value_json)
	SELECT id, $3, $4 FROM owned
	ON CONFLICT (run_id, step_index) DO NOTHING
	RETURNING 1
), existing AS (
	SELECT value_json FROM %[2]s
	WHERE run_id = $1 AND step_index = $3 AND NOT EXISTS (SELECT 1 FROM inserted)
), advanced AS (
	UPDATE %[1]s wr
	SET step_index = $3 + 1, last_error = NULL, lease_failures = 0, updated_at = now(),
		state = CASE WHEN $5 THEN 'completed' ELSE wr.state END,
		output_json = CASE WHEN $5 THEN COALESCE((SELECT value_json FROM existing), $4) END,
		lease_token = CASE WHEN $5 THEN NULL ELSE wr.lease_token END,
		lease_until = CASE WHEN $5 THEN NULL ELSE wr.lease_until END
	FROM owned
	WHERE wr.id = owned.id
	  AND (EXISTS (SELECT 1 FROM inserted) OR EXISTS (SELECT 1 FROM existing))
	RETURNING wr.id
)
SELECT (SELECT value_json FROM existing) FROM advanced`, runs, checkpoints, fence),
		completeRun: fmt.Sprintf(`
UPDATE %[1]s
SET state = 'completed', step_index = $3, output_json = $4, lease_token = NULL,
	lease_until = NULL, last_error = NULL, updated_at = now()
WHERE %[2]s`, runs, fence),
		parkSleep: fmt.Sprintf(`
UPDATE %[1]s
SET state = 'ready', step_index = $3,
	next_run_at = now() + $4::bigint * INTERVAL '1 millisecond',
	lease_token = NULL, lease_until = NULL, last_error = NULL, lease_failures = 0,
	updated_at = now()
WHERE %[2]s`, runs, fence),
		latestEvent: fmt.Sprintf(`
SELECT payload_json FROM %[1]s
WHERE event_key = $1 AND expires_at > now()
ORDER BY created_at DESC, id DESC
LIMIT 1`, events),
		// The cursor stays on the wait; delivery or timeout records its
		// outcome as the checkpoint at that position.
		parkWait: fmt.Sprintf(`
UPDATE %[1]s
SET state = 'waiting_event', step_index = $3, waiting_event_key = $4,
	waiting_deadline = now() + $5::bigint * INTERVAL '1 millisecond',
	lease_token = NULL, lease_until = NULL, last_error = NULL, lease_failures = 0,
	updated_at = now()
WHERE %[2]s`, runs, fence),
		failOrRetry: fmt.Sprintf(`
UPDATE %[1]s
SET state = CASE WHEN $3 >= max_attempts THEN 'failed' ELSE 'ready' END,
	attempt = $3,
	next_run_at = now() + $4::bigint * INTERVAL '1 millisecond',
	last_error = $5, lease_token = NULL, lease_until = NULL, updated_at = now()
WHERE %[2]s`, runs, fence),
		recoverLeases: fmt.Sprintf(`
WITH picked AS (
	SELECT id FROM %[1]s
	WHERE state = 'leased' AND lease_until < statement_timestamp()
	ORDER BY lease_until, id
	LIMIT %[2]d
	FOR UPDATE SKIP LOCKED
)
UPDATE %[1]s wr
SET state = CASE WHEN wr.lease_failures + 1 >= wr.max_attempts THEN 'failed' ELSE 'ready' END,
	lease_failures = wr.lease_failures + 1,
	next_run_at = clock_timestamp()
		+ LEAST(60000, 250 * (1 << LEAST(wr.lease_failures, 8))) * INTERVAL '1 millisecond',
	lease_token = NULL, lease_until = NULL, last_error = 'lease expired', updated_at = now()
FROM picked
WHERE wr.id = picked.id`, runs, maintenanceBatch),
		promoteTimedOut: fmt.Sprintf(`
WITH picked AS (
	SELECT id FROM %[1]s
	WHERE state = 'waiting_event' AND waiting_deadline <= statement_timestamp()
	ORDER BY waiting_deadline, id
	LIMIT %[3]d
	FOR UPDATE SKIP LOCKED
), promoted AS (
	UPDATE %[1]s wr
	SET state = 'ready', next_run_at = now(), waiting_event_key = NULL,
		waiting_deadline = NULL, updated_at = now()
	FROM picked
	WHERE wr.id = picked.id
	RETURNING wr.id, wr.step_index, wr.queue
), recorded AS (
	INSERT INTO %[2]s (run_id, step_index, value_json)
	SELECT id, step_index, '"timed_out"'::jsonb FROM promoted
	ON CONFLICT (run_id, step_index) DO NOTHING
), notified AS (
	SELECT pg_notify($1, queue) FROM (SELECT DISTINCT queue FROM promoted) q
)
SELECT (SELECT count(*) FROM promoted), (SELECT count(*) FROM notified)`, runs, checkpoints, maintenanceBatch),
		pruneEvents: fmt.Sprintf(`
DELETE FROM %[1]s
WHERE id IN (
	SELECT id FROM %[1]s
	WHERE expires_at <= statement_timestamp()
	ORDER BY expires_at
	LIMIT %[2]d
	FOR UPDATE SKIP LOCKED
)`, events, pruneBatch),
		runStatus: fmt.Sprintf(`
SELECT id::text, workflow_name, workflow_version, queue, state, step_index, attempt, max_attempts,
	lease_failures, next_run_at, waiting_event_key, waiting_deadline, last_error,
	created_at, updated_at
FROM %[1]s WHERE id = $1`, runs),
		runOutput: fmt.Sprintf("SELECT state, output_json FROM %s WHERE id = $1", runs),
		cancelRun: fmt.Sprintf(`
UPDATE %[1]s
SET state = 'cancelled', lease_token = NULL, lease_until = NULL,
	waiting_event_key = NULL, waiting_deadline = NULL, updated_at = now()
WHERE id = $1 AND state IN ('ready', 'leased', 'waiting_event')`, runs),
		runState: fmt.Sprintf("SELECT state FROM %s WHERE id = $1", runs),
	}
}
