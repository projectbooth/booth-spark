package dataaccess

import (
	"context"
	"sync"
	"time"
)

// RefusalMemory is how long a refused mint is remembered, so a burst of requests for a run whose
// submitter lost access costs one call to core (booth-api's rule).
const RefusalMemory = 30 * time.Second

// Run is what minting needs to know about a run.
type Run struct {
	ID        string
	Workspace string
	// Owner is whom the run reads and writes data as (runs.Run.DataOwnerID): its submitter, or for
	// a run a pipeline submitted, the pipeline's owner (ADR 0119). Opaque: core resolves it.
	Owner string
	// Ceiling caps the token: editor, or viewer for a run a viewer-role pipeline submitted.
	Ceiling string
	// Pipeline is true for a run a pipeline submitted; refusals then name the owner, not the
	// submitter.
	Pipeline bool
}

// Who names the run's owner in a refusal.
func (r Run) Who() string {
	if r.Pipeline {
		return "the run's owner (" + r.Owner + ", the owner of the pipeline that submitted it)"
	}
	return "the run's submitter"
}

func (r Run) ceiling() string {
	if r.Ceiling != "" {
		return r.Ceiling
	}
	return RoleCeiling
}

// Tokens keeps each live run's current workload token in memory, re-minting once RemintAfter of
// its life has passed (default two thirds: about every 6.7 minutes for core's 10-minute tokens).
// Nothing else ever holds a token: no database row, no Kubernetes object.
type Tokens struct {
	Minter Minter
	// RemintAfter is the fraction of a token's life after which it is re-minted (0 < x <= 1).
	RemintAfter float64
	// MaxAge, if set, re-mints at least this often whatever the token's life (a test knob, so a
	// lost submitter is noticed in seconds instead of minutes).
	MaxAge time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	entries map[string]entry // run id
}

type entry struct {
	tok      Token
	mintedAt time.Time
	refused  time.Time
	err      error
}

// Get returns the run's token, minting if there is none or it is due. A refusal (see Refused) is
// remembered for RefusalMemory; a transient failure keeps serving a still-valid token.
func (t *Tokens) Get(ctx context.Context, r Run) (Token, error) {
	now := t.now()
	t.mu.Lock()
	if t.entries == nil {
		t.entries = map[string]entry{}
	}
	e, ok := t.entries[r.ID]
	t.mu.Unlock()
	if ok {
		if e.err != nil && now.Sub(e.refused) < RefusalMemory {
			return Token{}, e.err
		}
		if e.err == nil && now.Before(t.due(e)) {
			return e.tok, nil
		}
	}

	ceiling := r.ceiling()
	tok, err := t.Minter.Mint(ctx, r.Workspace, Subject(r.Workspace, r.ID), r.Owner, ceiling)
	if err == nil && (tok.Role != "editor" && tok.Role != "viewer" || ceiling == "viewer" && tok.Role != "viewer") {
		err = ErrRoleExceeded
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case Refused(err):
		t.entries[r.ID] = entry{refused: now, err: err}
		return Token{}, err
	case err != nil:
		if ok && e.err == nil && now.Before(e.tok.ExpiresAt) {
			return e.tok, nil
		}
		return Token{}, err
	}
	t.entries[r.ID] = entry{tok: tok, mintedAt: now}
	return tok, nil
}

// Forget drops a run's token (the run ended).
func (t *Tokens) Forget(id string) {
	t.mu.Lock()
	delete(t.entries, id)
	t.mu.Unlock()
}

func (t *Tokens) due(e entry) time.Time {
	f := t.RemintAfter
	if f <= 0 || f > 1 {
		f = 2.0 / 3
	}
	d := e.mintedAt.Add(time.Duration(float64(e.tok.ExpiresAt.Sub(e.mintedAt)) * f))
	if t.MaxAge > 0 {
		if m := e.mintedAt.Add(t.MaxAge); m.Before(d) {
			d = m
		}
	}
	return d
}

func (t *Tokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
