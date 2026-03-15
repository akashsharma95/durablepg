package durablepg

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPGTimestampLiteral(t *testing.T) {
	ts := time.Date(2026, 2, 11, 16, 45, 30, 123456000, time.UTC)
	got := pgTimestampLiteral(ts)
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'::timestamptz") {
		t.Fatalf("pgTimestampLiteral() = %q", got)
	}
	if !strings.Contains(got, "2026-02-11 16:45:30.123456") {
		t.Fatalf("pgTimestampLiteral() unexpected value = %q", got)
	}
}

func TestEventLogMonthlyPartitionsSQL(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	ddl, err := e.EventLogMonthlyPartitionsSQL(time.Date(2026, 2, 11, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatalf("EventLogMonthlyPartitionsSQL() error = %v", err)
	}
	if !strings.Contains(ddl, `CREATE TABLE IF NOT EXISTS "durable"."event_log_202602"`) {
		t.Fatalf("missing first partition statement:\n%s", ddl)
	}
	if !strings.Contains(ddl, `CREATE TABLE IF NOT EXISTS "durable"."event_log_202603"`) {
		t.Fatalf("missing second partition statement:\n%s", ddl)
	}
}

func TestEventLogMonthlyPartitionsSQLNoop(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	ddl, err := e.EventLogMonthlyPartitionsSQL(time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("EventLogMonthlyPartitionsSQL() error = %v", err)
	}
	if ddl != "" {
		t.Fatalf("expected empty DDL for zero months, got: %q", ddl)
	}
}

func TestSchemaSQLContainsPartitionFunction(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	ddl := e.SchemaSQL()
	if !strings.Contains(ddl, `CREATE OR REPLACE FUNCTION "durable".ensure_event_log_partitions`) {
		t.Fatal("SchemaSQL() missing ensure_event_log_partitions function")
	}
	if !strings.Contains(ddl, "months_ahead INT DEFAULT 3") {
		t.Fatal("SchemaSQL() missing months_ahead parameter")
	}
}

func TestSchemaSQLUsesUnquotedSchemaInFunction(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	ddl := e.SchemaSQL()
	if !strings.Contains(ddl, `n.nspname = 'durable'`) {
		t.Fatal("SchemaSQL() should use unquoted schema in nspname comparison")
	}
	if strings.Contains(ddl, `n.nspname = '"durable"'`) {
		t.Fatal("SchemaSQL() should not use quoted schema in nspname comparison")
	}
}

func TestEnsurePartitionsBoundsValidation(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	tests := []struct {
		months int
	}{
		{-1},
		{-100},
		{121},
		{1000},
	}
	for _, tc := range tests {
		err := e.EnsurePartitions(context.Background(), tc.months)
		if err == nil {
			t.Errorf("EnsurePartitions(%d) expected error for out-of-bounds value, got nil", tc.months)
		}
	}
}
