package durablepg

import "testing"

func TestWithWorkflowIDAlias(t *testing.T) {
	var opts enqueueOptions
	WithWorkflowID("run-123")(&opts)
	if opts.runID != "run-123" {
		t.Fatalf("runID = %q, want run-123", opts.runID)
	}
}

func TestWithDeduplicationKeyAlias(t *testing.T) {
	var opts enqueueOptions
	WithDeduplicationKey("signup:user@example.com")(&opts)
	if opts.idempotencyKey != "signup:user@example.com" {
		t.Fatalf("idempotencyKey = %q, want signup:user@example.com", opts.idempotencyKey)
	}
}
