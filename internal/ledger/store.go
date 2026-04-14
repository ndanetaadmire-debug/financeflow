package ledger

import "context"

// Store persists accounts and transactions. Commit MUST be atomic: either every
// posting and every balance update is applied, or none is. Implementations use
// optimistic concurrency on Account.Version (memstore: mutex; pgstore: SERIALIZABLE
// + version check), and return ErrConcurrentModified on conflict.
type Store interface {
	CreateAccount(ctx context.Context, a Account) error
	GetAccount(ctx context.Context, id string) (Account, error)
	ListAccounts(ctx context.Context, ownerID string) ([]Account, error)
	GetTransaction(ctx context.Context, id string) (Transaction, error)
	FindByIdempotencyKey(ctx context.Context, key string) (Transaction, bool, error)
	ListTransactions(ctx context.Context, accountID string, before string, limit int) ([]Transaction, error)

	// Commit applies tx. `expected` maps account ID → the Version the caller read;
	// the store must reject the commit if any account has moved on since.
	Commit(ctx context.Context, tx Transaction, updated []Account, expected map[string]int64) error
	MarkReversed(ctx context.Context, originalID, reversalID string) error
}
