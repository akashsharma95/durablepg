package durablepg

import (
	"context"
	"sync"
	"time"
)

// leaseTable lets one heartbeat statement renew all of a worker's claims.
type leaseTable struct {
	mu      sync.Mutex
	byToken map[string]*leaseEntry
}

type leaseEntry struct {
	runID    string
	deadline time.Time
	ctx      context.Context
	cancel   context.CancelFunc
}

func newLeaseTable() *leaseTable {
	return &leaseTable{byToken: make(map[string]*leaseEntry)}
}

// insert returns the claim's context, canceled when the lease is lost.
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

// applyRenewal stops claims whose sent tokens are missing from renewed.
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

// expire cancels claims past their confirmed deadline and returns the earliest remaining one.
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
