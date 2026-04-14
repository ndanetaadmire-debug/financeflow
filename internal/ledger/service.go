package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Service is the only way money moves. It validates, enforces invariants and
// retries optimistic-concurrency conflicts a bounded number of times.
type Service struct {
	store      Store
	now        func() time.Time
	maxRetries int
}

func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now, maxRetries: 5}
}

type OpenAccountRequest struct {
	OwnerID        string
	Name           string
	Type           AccountType
	Currency       string
	AllowOverdraft bool
}

func (s *Service) OpenAccount(ctx context.Context, r OpenAccountRequest) (Account, error) {
	if r.OwnerID == "" || r.Name == "" || len(r.Currency) != 3 {
		return Account{}, fmt.Errorf("%w: owner, name and 3-letter currency required", ErrInvalid)
	}
	switch r.Type {
	case Asset, Liability, Revenue, Expense, Equity:
	default:
		return Account{}, fmt.Errorf("%w: unknown account type %q", ErrInvalid, r.Type)
	}
	a := Account{
		ID: newID("acc"), OwnerID: r.OwnerID, Name: r.Name, Type: r.Type, Currency: r.Currency,
		AllowOverdraft: r.AllowOverdraft, CreatedAt: s.now().UTC(),
	}
	return a, s.store.CreateAccount(ctx, a)
}

// TransferRequest moves Amount from one account to another, optionally charging a fee.
type TransferRequest struct {
	IdempotencyKey string
	FromAccountID  string
	ToAccountID    string
	Amount         Amount
	FeeAccountID   string
	Fee            Amount
	Kind           string
	Metadata       map[string]string
}

// Transfer is idempotent: replaying the same key with the same request returns the
// original transaction; the same key with a different request is rejected.
func (s *Service) Transfer(ctx context.Context, r TransferRequest) (Transaction, error) {
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > 128 {
		return Transaction{}, fmt.Errorf("%w: idempotency key required (≤128 chars)", ErrInvalid)
	}
	if r.Amount <= 0 || r.Fee < 0 || r.FromAccountID == r.ToAccountID || r.FromAccountID == "" || r.ToAccountID == "" {
		return Transaction{}, fmt.Errorf("%w: positive amount and two distinct accounts required", ErrInvalid)
	}
	if r.Fee > 0 && r.FeeAccountID == "" {
		return Transaction{}, fmt.Errorf("%w: fee account required when charging a fee", ErrInvalid)
	}
	if r.Kind == "" {
		r.Kind = "transfer"
	}
	total, err := add(r.Amount, r.Fee)
	if err != nil {
		return Transaction{}, err
	}
	postings := []Posting{{r.FromAccountID, -total}, {r.ToAccountID, r.Amount}}
	if r.Fee > 0 {
		postings = append(postings, Posting{r.FeeAccountID, r.Fee})
	}
	return s.post(ctx, r.IdempotencyKey, hashRequest(r), r.Kind, postings, r.Metadata, "")
}

// Reverse books an equal and opposite transaction. History is never edited.
func (s *Service) Reverse(ctx context.Context, idempotencyKey, txID, reason string) (Transaction, error) {
	orig, err := s.store.GetTransaction(ctx, txID)
	if err != nil {
		return Transaction{}, err
	}
	if orig.ReversedByID != "" {
		if existing, ok, _ := s.store.FindByIdempotencyKey(ctx, idempotencyKey); ok && existing.ReversesID == txID {
			return existing, nil
		}
		return Transaction{}, ErrAlreadyReversed
	}
	if orig.Kind == "reversal" {
		return Transaction{}, fmt.Errorf("%w: cannot reverse a reversal", ErrInvalid)
	}
	inverse := make([]Posting, len(orig.Postings))
	for i, p := range orig.Postings {
		inverse[i] = Posting{p.AccountID, -p.Amount}
	}
	tx, err := s.post(ctx, idempotencyKey, "reversal:"+txID, "reversal", inverse, map[string]string{"reason": reason}, txID)
	if err != nil {
		return Transaction{}, err
	}
	return tx, s.store.MarkReversed(ctx, txID, tx.ID)
}

func (s *Service) post(ctx context.Context, key, reqHash, kind string, postings []Posting, meta map[string]string, reverses string) (Transaction, error) {
	for attempt := 0; ; attempt++ {
		if existing, ok, err := s.store.FindByIdempotencyKey(ctx, key); err != nil {
			return Transaction{}, err
		} else if ok {
			if existing.RequestHash != reqHash {
				return Transaction{}, ErrIdempotencyReuse
			}
			return existing, nil
		}

		tx, updated, expected, err := s.prepare(ctx, key, reqHash, kind, postings, meta, reverses)
		if err != nil {
			return Transaction{}, err
		}
		err = s.store.Commit(ctx, tx, updated, expected)
		if errors.Is(err, ErrConcurrentModified) && attempt < s.maxRetries {
			continue // re-read balances and re-check invariants
		}
		if err != nil {
			return Transaction{}, err
		}
		return tx, nil
	}
}

func (s *Service) prepare(ctx context.Context, key, reqHash, kind string, postings []Posting, meta map[string]string, reverses string) (Transaction, []Account, map[string]int64, error) {
	var sum Amount
	currency := ""
	accounts := map[string]Account{}
	// Lock-order by ID so concurrent multi-account transactions can't deadlock in SQL stores.
	ordered := append([]Posting(nil), postings...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].AccountID < ordered[j].AccountID })

	for _, p := range ordered {
		a, ok := accounts[p.AccountID]
		if !ok {
			var err error
			if a, err = s.store.GetAccount(ctx, p.AccountID); err != nil {
				return Transaction{}, nil, nil, fmt.Errorf("account %s: %w", p.AccountID, err)
			}
		}
		if currency == "" {
			currency = a.Currency
		} else if a.Currency != currency {
			return Transaction{}, nil, nil, ErrCurrencyMismatch
		}
		if a.Frozen {
			return Transaction{}, nil, nil, fmt.Errorf("%w: %s", ErrAccountFrozen, a.ID)
		}
		nb, err := add(a.Balance, p.Amount)
		if err != nil {
			return Transaction{}, nil, nil, err
		}
		a.Balance = nb
		accounts[a.ID] = a
		if sum, err = add(sum, p.Amount); err != nil {
			return Transaction{}, nil, nil, err
		}
	}
	if sum != 0 {
		return Transaction{}, nil, nil, ErrUnbalanced
	}

	expected := map[string]int64{}
	updated := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		// Customer-facing liability accounts must not go negative (we'd be lending money).
		if a.Type == Liability && a.Balance < 0 && !a.AllowOverdraft {
			return Transaction{}, nil, nil, fmt.Errorf("%w in %s", ErrInsufficientFunds, a.ID)
		}
		expected[a.ID] = a.Version
		a.Version++
		updated = append(updated, a)
	}
	sort.Slice(updated, func(i, j int) bool { return updated[i].ID < updated[j].ID })

	tx := Transaction{
		ID: newID("txn"), IdempotencyKey: key, RequestHash: reqHash, Kind: kind, Currency: currency,
		Postings: postings, Metadata: meta, ReversesID: reverses, CreatedAt: s.now().UTC(),
	}
	return tx, updated, expected, nil
}

// Statement returns transactions affecting an account, newest first, keyset-paginated by ID.
func (s *Service) Statement(ctx context.Context, accountID, before string, limit int) ([]Transaction, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.ListTransactions(ctx, accountID, before, limit)
}

func (s *Service) Account(ctx context.Context, id string) (Account, error) {
	return s.store.GetAccount(ctx, id)
}

func (s *Service) Accounts(ctx context.Context, ownerID string) ([]Account, error) {
	return s.store.ListAccounts(ctx, ownerID)
}

func hashRequest(r TransferRequest) string {
	b, _ := json.Marshal(r) // map keys are sorted by encoding/json → stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newID returns a strictly increasing, sortable ID: millisecond timestamp, a per-process
// sequence that breaks ties within the same millisecond, then a random suffix for global
// uniqueness across processes. Keyset pagination relies on this ordering.
func newID(prefix string) string {
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= idLastMs {
		ms = idLastMs // clock went backwards or same millisecond: never go back in order
		idSeq++
	} else {
		idLastMs, idSeq = ms, 0
	}
	seq := idSeq
	idMu.Unlock()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%013d%06d%s", prefix, ms, seq, hex.EncodeToString(b[:]))
}

var (
	idMu     sync.Mutex
	idLastMs int64
	idSeq    int64
)
