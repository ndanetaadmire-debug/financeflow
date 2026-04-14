package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
	"github.com/ndanetaadmire-debug/financeflow/internal/store/memstore"
)

type fixture struct {
	svc                    *ledger.Service
	store                  *memstore.Store
	bank, alice, bob, fees ledger.Account
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	svc := ledger.NewService(st, nil)
	open := func(owner, name string, typ ledger.AccountType, overdraft bool) ledger.Account {
		a, err := svc.OpenAccount(ctx, ledger.OpenAccountRequest{OwnerID: owner, Name: name, Type: typ, Currency: "ZAR", AllowOverdraft: overdraft})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f := fixture{svc: svc, store: st,
		bank:  open("system", "Settlement", ledger.Asset, true),
		alice: open("alice", "Alice wallet", ledger.Liability, false),
		bob:   open("bob", "Bob wallet", ledger.Liability, false),
		fees:  open("system", "Fee income", ledger.Revenue, false),
	}
	// Deposit R1,000 into Alice: bank asset goes down (debit, signed negative), wallet up.
	if _, err := svc.Transfer(ctx, ledger.TransferRequest{IdempotencyKey: "seed", FromAccountID: f.bank.ID, ToAccountID: f.alice.ID, Amount: 100_000, Kind: "deposit"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func balance(t *testing.T, f fixture, id string) ledger.Amount {
	t.Helper()
	a, err := f.svc.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func TestParseAmount(t *testing.T) {
	cases := map[string]ledger.Amount{"10": 1000, "10.5": 1050, "0.01": 1, "-3.20": -320, "1234567.89": 123456789}
	for in, want := range cases {
		got, err := ledger.ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1.234", "abc", ".5", "99999999999999999999"} {
		if _, err := ledger.ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
	if s := ledger.Amount(-1050).String(); s != "-10.50" {
		t.Errorf("String() = %s", s)
	}
}

func TestTransferWithFeeIsBalanced(t *testing.T) {
	f := setup(t)
	tx, err := f.svc.Transfer(context.Background(), ledger.TransferRequest{
		IdempotencyKey: "k1", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 25_000, FeeAccountID: f.fees.ID, Fee: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sum ledger.Amount
	for _, p := range tx.Postings {
		sum += p.Amount
	}
	if sum != 0 {
		t.Fatalf("postings sum to %v", sum)
	}
	if got := balance(t, f, f.alice.ID); got != 74_500 {
		t.Errorf("alice = %v", got)
	}
	if got := balance(t, f, f.bob.ID); got != 25_000 {
		t.Errorf("bob = %v", got)
	}
	if got := balance(t, f, f.fees.ID); got != 500 {
		t.Errorf("fees = %v", got)
	}
}

func TestInsufficientFundsLeavesNoTrace(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Transfer(context.Background(), ledger.TransferRequest{IdempotencyKey: "k", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 100_001})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("err = %v", err)
	}
	if balance(t, f, f.alice.ID) != 100_000 || balance(t, f, f.bob.ID) != 0 {
		t.Fatal("failed transfer changed balances")
	}
}

func TestIdempotency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	req := ledger.TransferRequest{IdempotencyKey: "pay-42", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 1_000}
	a, err := f.svc.Transfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.svc.Transfer(ctx, req)
	if err != nil || a.ID != b.ID {
		t.Fatalf("replay should return original: %v %v %v", a.ID, b.ID, err)
	}
	if balance(t, f, f.bob.ID) != 1_000 {
		t.Fatal("replay moved money twice")
	}
	req.Amount = 2_000
	if _, err := f.svc.Transfer(ctx, req); !errors.Is(err, ledger.ErrIdempotencyReuse) {
		t.Fatalf("different payload under same key: %v", err)
	}
}

func TestValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	usd, _ := f.svc.OpenAccount(ctx, ledger.OpenAccountRequest{OwnerID: "carol", Name: "USD", Type: ledger.Liability, Currency: "USD"})
	cases := []struct {
		name string
		req  ledger.TransferRequest
		want error
	}{
		{"no key", ledger.TransferRequest{FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 1}, ledger.ErrInvalid},
		{"zero", ledger.TransferRequest{IdempotencyKey: "a", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID}, ledger.ErrInvalid},
		{"self", ledger.TransferRequest{IdempotencyKey: "b", FromAccountID: f.alice.ID, ToAccountID: f.alice.ID, Amount: 1}, ledger.ErrInvalid},
		{"fx", ledger.TransferRequest{IdempotencyKey: "c", FromAccountID: f.alice.ID, ToAccountID: usd.ID, Amount: 1}, ledger.ErrCurrencyMismatch},
		{"missing", ledger.TransferRequest{IdempotencyKey: "d", FromAccountID: f.alice.ID, ToAccountID: "acc_nope", Amount: 1}, ledger.ErrNotFound},
		{"fee no acct", ledger.TransferRequest{IdempotencyKey: "e", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 1, Fee: 1}, ledger.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.svc.Transfer(ctx, c.req); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
}

func TestReversal(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tx, _ := f.svc.Transfer(ctx, ledger.TransferRequest{IdempotencyKey: "r1", FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 4_000})
	rev, err := f.svc.Reverse(ctx, "rev-1", tx.ID, "customer dispute")
	if err != nil {
		t.Fatal(err)
	}
	if rev.ReversesID != tx.ID || balance(t, f, f.alice.ID) != 100_000 || balance(t, f, f.bob.ID) != 0 {
		t.Fatal("reversal did not restore balances")
	}
	again, err := f.svc.Reverse(ctx, "rev-1", tx.ID, "customer dispute")
	if err != nil || again.ID != rev.ID {
		t.Fatalf("idempotent reversal replay: %v", err)
	}
	if _, err := f.svc.Reverse(ctx, "rev-2", tx.ID, "again"); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("double reversal: %v", err)
	}
	if _, err := f.svc.Reverse(ctx, "rev-3", rev.ID, "nope"); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("reversing a reversal: %v", err)
	}
}

// Concurrency: 200 goroutines race to spend from one wallet. Optimistic locking
// must ensure no overdraft and exactly the affordable number of transfers succeed.
func TestConcurrentSpendNeverOverdraws(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	var ok, insufficient atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.svc.Transfer(context.Background(), ledger.TransferRequest{
				IdempotencyKey: fmt.Sprintf("c-%d", i), FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 1_000,
			})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficientFunds):
				insufficient.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if a := balance(t, f, f.alice.ID); a < 0 {
		t.Fatalf("overdrawn: %v", a)
	}
	if got := balance(t, f, f.alice.ID) + balance(t, f, f.bob.ID); got != 100_000 {
		t.Fatalf("money created or destroyed: %v", got)
	}
	if int(ok.Load())*1_000 != int(balance(t, f, f.bob.ID)) {
		t.Fatalf("successes %d don't match bob balance", ok.Load())
	}
}

// Property: after any sequence of random transfers, the ledger nets to zero.
func TestRandomTransfersConserveMoney(t *testing.T) {
	f := setup(t)
	ids := []string{f.alice.ID, f.bob.ID, f.fees.ID}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 2_000; i++ {
		from, to := ids[r.IntN(3)], ids[r.IntN(3)]
		_, _ = f.svc.Transfer(context.Background(), ledger.TransferRequest{
			IdempotencyKey: fmt.Sprintf("p-%d", i), FromAccountID: from, ToAccountID: to, Amount: ledger.Amount(r.IntN(5_000) + 1),
		})
	}
	total := balance(t, f, f.bank.ID)
	for _, id := range ids {
		total += balance(t, f, id)
		if b := balance(t, f, id); b < 0 && id != f.fees.ID {
			t.Fatalf("wallet %s negative: %v", id, b)
		}
	}
	if total != 0 {
		t.Fatalf("ledger does not net to zero: %v", total)
	}
}

func TestStatementPagination(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := f.svc.Transfer(ctx, ledger.TransferRequest{IdempotencyKey: fmt.Sprintf("s%d", i), FromAccountID: f.alice.ID, ToAccountID: f.bob.ID, Amount: 100}); err != nil {
			t.Fatal(err)
		}
	}
	page1, _ := f.svc.Statement(ctx, f.bob.ID, "", 3)
	page2, _ := f.svc.Statement(ctx, f.bob.ID, page1[2].ID, 3)
	if len(page1) != 3 || len(page2) != 2 || page2[0].ID >= page1[2].ID {
		t.Fatalf("pagination broken: %d %d", len(page1), len(page2))
	}
}
