// Package memstore is an in-memory ledger.Store with the same atomicity and
// optimistic-concurrency semantics as the Postgres store. Used for tests and demos.
package memstore

import (
	"context"
	"sort"
	"sync"

	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
)

type Store struct {
	mu       sync.RWMutex
	accounts map[string]ledger.Account
	txs      map[string]ledger.Transaction
	byKey    map[string]string
	byAcct   map[string][]string
}

func New() *Store {
	return &Store{
		accounts: map[string]ledger.Account{}, txs: map[string]ledger.Transaction{},
		byKey: map[string]string{}, byAcct: map[string][]string{},
	}
}

func (s *Store) CreateAccount(_ context.Context, a ledger.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
	return nil
}

func (s *Store) GetAccount(_ context.Context, id string) (ledger.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return ledger.Account{}, ledger.ErrNotFound
	}
	return a, nil
}

func (s *Store) ListAccounts(_ context.Context, ownerID string) ([]ledger.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []ledger.Account{}
	for _, a := range s.accounts {
		if a.OwnerID == ownerID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) GetTransaction(_ context.Context, id string) (ledger.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.txs[id]
	if !ok {
		return ledger.Transaction{}, ledger.ErrNotFound
	}
	return t, nil
}

func (s *Store) FindByIdempotencyKey(_ context.Context, key string) (ledger.Transaction, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byKey[key]
	if !ok {
		return ledger.Transaction{}, false, nil
	}
	return s.txs[id], true, nil
}

func (s *Store) ListTransactions(_ context.Context, accountID, before string, limit int) ([]ledger.Transaction, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Order by ID (newest first), not insertion order: concurrent commits can land
	// out of ID order, and keyset pagination must follow the same order as the cursor.
	ids := append([]string(nil), s.byAcct[accountID]...)
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := []ledger.Transaction{}
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		if before != "" && id >= before {
			continue
		}
		out = append(out, s.txs[id])
	}
	return out, nil
}

func (s *Store) Commit(_ context.Context, tx ledger.Transaction, updated []ledger.Account, expected map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byKey[tx.IdempotencyKey]; dup {
		return ledger.ErrConcurrentModified // a concurrent request with the same key won; caller re-reads it
	}
	for id, v := range expected {
		if s.accounts[id].Version != v {
			return ledger.ErrConcurrentModified
		}
	}
	for _, a := range updated {
		s.accounts[a.ID] = a
	}
	s.txs[tx.ID] = tx
	s.byKey[tx.IdempotencyKey] = tx.ID
	seen := map[string]bool{}
	for _, p := range tx.Postings {
		if !seen[p.AccountID] {
			s.byAcct[p.AccountID] = append(s.byAcct[p.AccountID], tx.ID)
			seen[p.AccountID] = true
		}
	}
	return nil
}

func (s *Store) MarkReversed(_ context.Context, originalID, reversalID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.txs[originalID]
	if !ok {
		return ledger.ErrNotFound
	}
	t.ReversedByID = reversalID
	s.txs[originalID] = t
	return nil
}
