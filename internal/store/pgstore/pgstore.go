// Package pgstore implements ledger.Store on PostgreSQL via database/sql.
// The pgx driver is registered in cmd/financeflow via a blank import.
//
// Commit runs in a SERIALIZABLE transaction and updates each account with
// `WHERE version = $expected`; a zero row count or a serialization failure
// (SQLSTATE 40001) becomes ledger.ErrConcurrentModified so the service retries.
package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
)

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

const accountCols = `id, owner_id, name, type, currency, balance, allow_overdraft, frozen, version, created_at`

func scanAccount(row interface{ Scan(...any) error }) (ledger.Account, error) {
	var a ledger.Account
	err := row.Scan(&a.ID, &a.OwnerID, &a.Name, &a.Type, &a.Currency, &a.Balance, &a.AllowOverdraft, &a.Frozen, &a.Version, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ledger.ErrNotFound
	}
	return a, err
}

func (s *Store) CreateAccount(ctx context.Context, a ledger.Account) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts (`+accountCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.ID, a.OwnerID, a.Name, a.Type, a.Currency, a.Balance, a.AllowOverdraft, a.Frozen, a.Version, a.CreatedAt)
	return err
}

func (s *Store) GetAccount(ctx context.Context, id string) (ledger.Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
}

func (s *Store) ListAccounts(ctx context.Context, ownerID string) ([]ledger.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE owner_id = $1 ORDER BY id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const txCols = `id, idempotency_key, request_hash, kind, currency, metadata, coalesce(reverses_id,''), coalesce(reversed_by_id,''), created_at`

func (s *Store) loadTx(ctx context.Context, where string, arg any) (ledger.Transaction, error) {
	var t ledger.Transaction
	var meta []byte
	err := s.db.QueryRowContext(ctx, `SELECT `+txCols+` FROM transactions WHERE `+where, arg).
		Scan(&t.ID, &t.IdempotencyKey, &t.RequestHash, &t.Kind, &t.Currency, &meta, &t.ReversesID, &t.ReversedByID, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ledger.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	_ = json.Unmarshal(meta, &t.Metadata)
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, amount FROM postings WHERE transaction_id = $1 ORDER BY seq`, t.ID)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ledger.Posting
		if err := rows.Scan(&p.AccountID, &p.Amount); err != nil {
			return t, err
		}
		t.Postings = append(t.Postings, p)
	}
	return t, rows.Err()
}

func (s *Store) GetTransaction(ctx context.Context, id string) (ledger.Transaction, error) {
	return s.loadTx(ctx, "id = $1", id)
}

func (s *Store) FindByIdempotencyKey(ctx context.Context, key string) (ledger.Transaction, bool, error) {
	t, err := s.loadTx(ctx, "idempotency_key = $1", key)
	if errors.Is(err, ledger.ErrNotFound) {
		return t, false, nil
	}
	return t, err == nil, err
}

func (s *Store) ListTransactions(ctx context.Context, accountID, before string, limit int) ([]ledger.Transaction, error) {
	q := `SELECT DISTINCT t.id FROM transactions t JOIN postings p ON p.transaction_id = t.id
	      WHERE p.account_id = $1 AND ($2 = '' OR t.id < $2) ORDER BY t.id DESC LIMIT $3`
	rows, err := s.db.QueryContext(ctx, q, accountID, before, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]ledger.Transaction, 0, len(ids))
	for _, id := range ids {
		t, err := s.GetTransaction(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) Commit(ctx context.Context, tx ledger.Transaction, updated []ledger.Account, expected map[string]int64) (err error) {
	dbtx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = dbtx.Rollback()
			err = mapErr(err)
		}
	}()
	for _, a := range updated { // already sorted by ID → consistent lock order
		res, err := dbtx.ExecContext(ctx, `UPDATE accounts SET balance = $1, version = $2 WHERE id = $3 AND version = $4`,
			a.Balance, a.Version, a.ID, expected[a.ID])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ledger.ErrConcurrentModified
		}
	}
	meta, _ := json.Marshal(tx.Metadata)
	if _, err = dbtx.ExecContext(ctx, `INSERT INTO transactions (id, idempotency_key, request_hash, kind, currency, metadata, reverses_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, tx.ID, tx.IdempotencyKey, tx.RequestHash, tx.Kind, tx.Currency, meta, tx.ReversesID, tx.CreatedAt); err != nil {
		return err
	}
	for i, p := range tx.Postings {
		if _, err = dbtx.ExecContext(ctx, `INSERT INTO postings (transaction_id, seq, account_id, amount) VALUES ($1,$2,$3,$4)`,
			tx.ID, i, p.AccountID, p.Amount); err != nil {
			return err
		}
	}
	return dbtx.Commit()
}

func (s *Store) MarkReversed(ctx context.Context, originalID, reversalID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transactions SET reversed_by_id = $1 WHERE id = $2 AND reversed_by_id IS NULL`, reversalID, originalID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ledger.ErrAlreadyReversed
	}
	return nil
}

// mapErr turns serialization failures and idempotency-key races into retryable conflicts.
func mapErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "40001") || strings.Contains(msg, "could not serialize") ||
		strings.Contains(msg, "transactions_idempotency_key_key") {
		return ledger.ErrConcurrentModified
	}
	return err
}
