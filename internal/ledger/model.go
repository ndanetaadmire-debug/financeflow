package ledger

import (
	"errors"
	"time"
)

// AccountType determines the normal balance side and overdraft rules.
type AccountType string

const (
	Asset     AccountType = "asset"     // e.g. bank settlement account (debit-normal)
	Liability AccountType = "liability" // e.g. customer wallet (credit-normal)
	Revenue   AccountType = "revenue"   // e.g. fee income
	Expense   AccountType = "expense"
	Equity    AccountType = "equity"
)

// Account balances are signed: positive = net credit. Customer wallets are
// liabilities, so a positive balance is money we owe the customer.
type Account struct {
	ID             string      `json:"id"`
	OwnerID        string      `json:"owner_id"`
	Name           string      `json:"name"`
	Type           AccountType `json:"type"`
	Currency       string      `json:"currency"`
	Balance        Amount      `json:"balance"`
	AllowOverdraft bool        `json:"allow_overdraft"`
	Frozen         bool        `json:"frozen"`
	Version        int64       `json:"version"`
	CreatedAt      time.Time   `json:"created_at"`
}

// Posting moves Amount into (positive) or out of (negative) an account.
type Posting struct {
	AccountID string `json:"account_id"`
	Amount    Amount `json:"amount"`
}

type Transaction struct {
	ID             string            `json:"id"`
	IdempotencyKey string            `json:"idempotency_key"`
	RequestHash    string            `json:"-"`
	Kind           string            `json:"kind"` // transfer | deposit | withdrawal | reversal | fee
	Currency       string            `json:"currency"`
	Postings       []Posting         `json:"postings"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ReversesID     string            `json:"reverses_id,omitempty"`
	ReversedByID   string            `json:"reversed_by_id,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

var (
	ErrNotFound           = errors.New("not found")
	ErrUnbalanced         = errors.New("postings do not sum to zero")
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrCurrencyMismatch   = errors.New("currency mismatch")
	ErrAccountFrozen      = errors.New("account frozen")
	ErrIdempotencyReuse   = errors.New("idempotency key reused with a different request")
	ErrAlreadyReversed    = errors.New("transaction already reversed")
	ErrInvalid            = errors.New("invalid request")
	ErrConcurrentModified = errors.New("concurrent modification, retry")
	ErrForbidden          = errors.New("forbidden")
)
