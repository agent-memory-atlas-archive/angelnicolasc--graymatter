package memory

import (
	"context"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Revise writes newText and retires every fact in victims, pointing each
// tombstone at the fact that replaced it. It returns the replacement's ID.
//
// The ordering is the contract, not an implementation detail: the replacement
// is written first, so a failure part-way through leaves the agent with both
// values rather than with a retired fact and nothing in its place. Retiring
// first and crashing before the write loses the belief entirely.
//
// Every victim is retired in the same call because the alternative — retiring
// one copy of a belief that was stored twice — leaves the other live, which is
// the stale-fact failure the tombstone exists to prevent.
//
// Nothing is deleted. Victims keep their text, their receipt and their decay
// curve (ADR-007); what changes is that Recall drops them before scoring, and
// the live fact's receipt names them under Provenance.Supersedes.
//
// The revision benchmark calls this compatible library method directly.
// Daemon callers use the optional ReviseFactsWithOptions RPC capability.
func (s *Store) Revise(ctx context.Context, agentID, newText string, victims ...Fact) (string, error) {
	f, err := s.ReviseFactsWithOptions(ctx, agentID, newText, WriteOptions{}, victims...)
	return f.ID, err
}

// ReviseWithOptions revises every live exact-text match captured by this call.
// The replacement's confidence is explicit or conservatively derived from the
// validated sources. The replacement never inherits a pin.
func (s *Store) ReviseWithOptions(ctx context.Context, agentID, target, newText string, options WriteOptions) (Fact, error) {
	if err := options.Validate(); err != nil {
		return Fact{}, err
	}
	facts, err := s.List(agentID)
	if err != nil {
		return Fact{}, err
	}
	var victims []Fact
	for _, f := range facts {
		if f.Text == target && !f.IsSuperseded() {
			victims = append(victims, f)
		}
	}
	if len(victims) == 0 {
		return Fact{}, fmt.Errorf("revise: no live target matches %q", target)
	}
	return s.ReviseFactsWithOptions(ctx, agentID, newText, options, victims...)
}

// ReviseFactsWithOptions preserves exact target IDs for callers that select by
// ID or substring. Source state is refreshed before embedding and validated
// again under the replacement write transaction. If a required source changes,
// no replacement is committed. Retirement failures return the committed fact
// alongside the error: replacement and retirement remain separate phases.
func (s *Store) ReviseFactsWithOptions(ctx context.Context, agentID, newText string, options WriteOptions, victims ...Fact) (Fact, error) {
	if err := options.Validate(); err != nil {
		return Fact{}, err
	}
	if newText == "" {
		return Fact{}, fmt.Errorf("revise: the replacement text is required")
	}
	for _, v := range victims {
		if v.AgentID != agentID || v.IsSuperseded() {
			return Fact{}, fmt.Errorf("revise: %q is invalid or already superseded", v.Text)
		}
	}
	sources := make([]Fact, 0, len(victims))
	seen := make(map[string]bool, len(victims))
	if err := s.db.View(func(tx *bolt.Tx) error {
		for _, v := range victims {
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
			current, err := factInTx(tx, agentID, v.ID)
			if err != nil {
				return err
			}
			if current.IsSuperseded() || current.Text != v.Text || current.Kind != v.Kind {
				return fmt.Errorf("%w: source %s", ErrFactChanged, v.ID)
			}
			sources = append(sources, current)
		}
		return nil
	}); err != nil {
		return Fact{}, err
	}

	// The replacement inherits the victim's kind: a revised alias stays an
	// alias — and stays non-injectable — instead of leaking back into the
	// result set as a content fact whose text happens to start with
	// "alias:". With mixed victims the first one wins; revising across kinds
	// has no coherent meaning and callers should not do it.
	kind := KindFact
	if len(sources) > 0 {
		kind = sources[0].Kind
	}
	replacement, err := s.putReturningFactPrepared(ctx, agentID, newText, kind, "", options, func(tx *bolt.Tx, replacement *Fact) error {
		if err := validateSourcesTx(tx, agentID, sources, false); err != nil {
			return err
		}
		if options.Confidence == nil {
			replacement.Confidence = derivedConfidence(sources)
		}
		return nil
	})
	if err != nil {
		return Fact{}, fmt.Errorf("revise: write the replacement: %w", err)
	}
	var errs []error
	for _, v := range sources {
		if err := s.retireDerivedFact(agentID, v, replacement.ID, false); err != nil {
			errs = append(errs, fmt.Errorf("revise: retire %q: %w", v.Text, err))
		}
	}
	return replacement, errors.Join(errs...)
}

// Retire tombstones facts that have nothing to replace them. Recall stops
// returning them at once; the facts themselves stay in the store with a
// receipt recording that an agent dropped them.
func (s *Store) Retire(agentID string, victims ...Fact) error {
	for _, v := range victims {
		if v.AgentID != agentID || v.IsSuperseded() {
			return fmt.Errorf("retire: %q is already superseded", v.Text)
		}
	}
	var errs []error
	seen := make(map[string]bool, len(victims))
	for _, v := range victims {
		if seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		if err := s.retireFact(agentID, v, SupersededByAgent, false); err != nil {
			errs = append(errs, fmt.Errorf("retire %q: %w", v.Text, err))
		}
	}
	return errors.Join(errs...)
}
