package durablepg

import (
	"context"
	"sync"
	"time"
)

// leaseTable tracks one worker's claims so a single heartbeat renews every
// active lease in one statement instead of one statement per claim.
type leaseTable struct {
	mu      sync.Mutex
	byToken map[string]*leaseEntry
}

type leaseEntry struct {
	runID string
	// Last moment the lease is known to be valid.
	deadline time.Time
	// Canceled when the lease is lost, expires, or the worker abandons work.
	ctx    context.Context
	cancel context.CancelFunc
}

func newLeaseTable() *leaseTable {
	return &leaseTable{byToken: make(map[string]*leaseEntry)}
}

// insert registers a claim and returns its context, canceled when the lease
// is lost, and a release function to call when the claim finishes.
func (t *leaseTable) insert(parent context.Context, runID, token string, deadline time.Time) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	t.mu.Lock()
	t.byToken[token] = &leaseEntry{runID: runID, deadline: deadline, ctx: ctx, cancel: cancel}
	t.mu.Unlock()
	return ctx, func() {
		cancel()
		t.mu.Lock()
		delete(t.byToken, token)
		t.mu.Unlock()
	}
}

// snapshot returns run IDs and tokens of leases still worth renewing.
func (t *leaseTable) snapshot() (runIDs, tokens []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for token, entry := range t.byToken {
		if entry.ctx.Err() == nil {
			runIDs = append(runIDs, entry.runID)
			tokens = append(tokens, token)
		}
	}
	return runIDs, tokens
}

// applyRenewal gives renewed tokens the new deadline. Sent tokens missing from
// renewed were canceled, recovered, or replaced, so their claims stop.
func (t *leaseTable) applyRenewal(sent []string, renewed map[string]struct{}, deadline time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, token := range sent {
		entry, ok := t.byToken[token]
		if !ok {
			continue
		}
		if _, ok := renewed[token]; ok {
			entry.deadline = deadline
		} else {
			entry.cancel()
		}
	}
}

// expire cancels claims whose last confirmed deadline has passed and returns
// the earliest deadline among the rest.
func (t *leaseTable) expire(now time.Time) (earliest time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, entry := range t.byToken {
		if entry.ctx.Err() != nil {
			continue
		}
		if !entry.deadline.After(now) {
			entry.cancel()
		} else if !ok || entry.deadline.Before(earliest) {
			earliest, ok = entry.deadline, true
		}
	}
	return earliest, ok
}
