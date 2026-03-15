package durablepg

import (
	"context"
	"testing"
)

func TestNewRequiresDB(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error when DB is nil")
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if got := quoteIdentifier(`abc`); got != `"abc"` {
		t.Fatalf("quoteIdentifier() = %q", got)
	}
}

func TestNewUUIDFormat(t *testing.T) {
	id := newUUID()
	if len(id) != 36 {
		t.Fatalf("newUUID() len = %d, want 36", len(id))
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("newUUID() = %q, invalid format", id)
	}
}

func TestBeginWorkerGuardsDoubleStart(t *testing.T) {
	e := &Engine{}
	if err := e.beginWorker(); err != nil {
		t.Fatalf("beginWorker() first call error = %v", err)
	}
	if err := e.beginWorker(); err == nil {
		t.Fatal("beginWorker() second call expected error")
	}
	e.endWorker()
	if err := e.beginWorker(); err != nil {
		t.Fatalf("beginWorker() after endWorker error = %v", err)
	}
}

func TestRegisterWorkflowStoresDefinition(t *testing.T) {
	e := &Engine{workflows: make(map[string]*compiledWorkflow)}
	e.RegisterWorkflow("signup", func(b *Builder) {
		b.Step("create_user", func(context.Context, *StepContext) (any, error) { return nil, nil })
	})

	wf, ok := e.workflow("signup")
	if !ok {
		t.Fatal("expected workflow to be registered")
	}
	if wf == nil || wf.name != "signup" {
		t.Fatalf("unexpected workflow = %#v", wf)
	}
}
