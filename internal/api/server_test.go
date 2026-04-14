package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ndanetaadmire-debug/financeflow/internal/api"
	"github.com/ndanetaadmire-debug/financeflow/internal/auth"
	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
	"github.com/ndanetaadmire-debug/financeflow/internal/store/memstore"
)

type harness struct {
	srv *httptest.Server
	v   auth.Verifier
	t   *testing.T
}

func newHarness(t *testing.T) *harness {
	v := auth.Verifier{Secret: []byte(strings.Repeat("k", 32)), Issuer: "financeflow", Audience: "financeflow-api"}
	h := api.New(ledger.NewService(memstore.New(), nil), v, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, v: v, t: t}
}

func (h *harness) token(sub string, role auth.Role) string {
	return h.v.Sign(auth.Claims{Subject: sub, Role: role, Issuer: "financeflow", Audience: "financeflow-api", ExpiresAt: time.Now().Add(time.Hour).Unix()})
}

func (h *harness) do(method, path, tok string, body any, headers ...string) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	admin, alice, bob, ops := h.token("admin", auth.RoleAdmin), h.token("alice", auth.RoleCustomer), h.token("bob", auth.RoleCustomer), h.token("ops-1", auth.RoleOps)

	if code, _ := h.do("GET", "/v1/accounts", "", nil); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}

	_, bank := h.do("POST", "/v1/accounts", admin, map[string]any{"owner_id": "system", "name": "Settlement", "type": "asset", "currency": "zar", "allow_overdraft": true})
	_, aw := h.do("POST", "/v1/accounts", alice, map[string]any{"name": "Main", "type": "liability", "currency": "ZAR"})
	_, bw := h.do("POST", "/v1/accounts", bob, map[string]any{"name": "Main", "type": "liability", "currency": "ZAR"})
	if aw["owner_id"] != "alice" {
		t.Fatalf("customer account should be owned by caller: %v", aw)
	}
	if code, _ := h.do("POST", "/v1/accounts", alice, map[string]any{"name": "Credit", "type": "liability", "currency": "ZAR", "allow_overdraft": true}); code != 403 {
		t.Fatalf("customer opened overdraft account: %d", code)
	}

	// Admin funds Alice from the settlement account.
	if code, body := h.do("POST", "/v1/transfers", admin, map[string]any{"from_account_id": bank["id"], "to_account_id": aw["id"], "amount": "500.00"}, "Idempotency-Key", "fund-1"); code != 201 {
		t.Fatalf("funding: %d %v", code, body)
	}

	transfer := map[string]any{"from_account_id": aw["id"], "to_account_id": bw["id"], "amount": "120.50"}
	code, tx := h.do("POST", "/v1/transfers", alice, transfer, "Idempotency-Key", "t-1")
	if code != 201 {
		t.Fatalf("transfer: %d %v", code, tx)
	}
	if code, replay := h.do("POST", "/v1/transfers", alice, transfer, "Idempotency-Key", "t-1"); code != 201 || replay["id"] != tx["id"] {
		t.Fatalf("idempotent replay: %d", code)
	}
	if code, _ := h.do("POST", "/v1/transfers", alice, transfer); code != 400 {
		t.Fatalf("missing idempotency key: %d", code)
	}

	// Bob cannot spend from or even see Alice's account.
	if code, _ := h.do("POST", "/v1/transfers", bob, map[string]any{"from_account_id": aw["id"], "to_account_id": bw["id"], "amount": "1.00"}, "Idempotency-Key", "steal"); code != 404 {
		t.Fatalf("cross-account spend: %d", code)
	}
	if code, _ := h.do("GET", "/v1/accounts/"+aw["id"].(string), bob, nil); code != 404 {
		t.Fatalf("cross-account read: %d", code)
	}

	if code, body := h.do("POST", "/v1/transfers", alice, map[string]any{"from_account_id": aw["id"], "to_account_id": bw["id"], "amount": "10000.00"}, "Idempotency-Key", "big"); code != 422 {
		t.Fatalf("overdraft: %d %v", code, body)
	}

	_, acct := h.do("GET", "/v1/accounts/"+bw["id"].(string), bob, nil)
	if acct["balance"].(float64) != 12050 {
		t.Fatalf("bob balance %v", acct["balance"])
	}

	// Only ops can reverse.
	if code, _ := h.do("POST", "/v1/transactions/"+tx["id"].(string)+"/reverse", alice, map[string]any{"reason": "x"}, "Idempotency-Key", "r"); code != 403 {
		t.Fatalf("customer reversed: %d", code)
	}
	if code, _ := h.do("POST", "/v1/transactions/"+tx["id"].(string)+"/reverse", ops, map[string]any{"reason": "chargeback"}, "Idempotency-Key", "r"); code != 201 {
		t.Fatalf("ops reverse: %d", code)
	}
	_, stmt := h.do("GET", "/v1/accounts/"+bw["id"].(string)+"/transactions", bob, nil)
	if n := len(stmt["data"].([]any)); n != 2 {
		t.Fatalf("statement should show transfer + reversal, got %d", n)
	}

	if code, _ := h.do("POST", "/v1/transfers", alice, map[string]any{"from_account_id": aw["id"], "unknown": 1}, "Idempotency-Key", "u"); code != 400 {
		t.Fatalf("unknown fields must be rejected: %d", code)
	}
}
