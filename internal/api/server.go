// Package api exposes the ledger over HTTP using Go 1.22+ pattern routing.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ndanetaadmire-debug/financeflow/internal/auth"
	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
)

type Server struct {
	svc      *ledger.Service
	verifier auth.Verifier
	log      *slog.Logger
}

func New(svc *ledger.Service, v auth.Verifier, log *slog.Logger) http.Handler {
	s := &Server{svc: svc, verifier: v, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.Handle("POST /v1/accounts", s.authed(s.openAccount))
	mux.Handle("GET /v1/accounts", s.authed(s.listAccounts))
	mux.Handle("GET /v1/accounts/{id}", s.authed(s.getAccount))
	mux.Handle("GET /v1/accounts/{id}/transactions", s.authed(s.statement))
	mux.Handle("POST /v1/transfers", s.authed(s.transfer))
	mux.Handle("POST /v1/transactions/{id}/reverse", s.authed(s.reverse))
	return s.recoverer(s.requestLog(securityHeaders(mux)))
}

type ctxKey struct{}

func claimsFrom(r *http.Request) auth.Claims { return r.Context().Value(ctxKey{}).(auth.Claims) }

func (s *Server) authed(h func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeErr(w, 401, "unauthorized", "missing bearer token")
			return
		}
		c, err := s.verifier.Verify(token)
		if err != nil {
			writeErr(w, 401, "unauthorized", "invalid token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	})
}

// canAccess enforces ownership: customers see only their own accounts.
func (s *Server) canAccess(r *http.Request, a ledger.Account) bool {
	c := claimsFrom(r)
	return c.Can(auth.RoleOps, auth.RoleAdmin) || a.OwnerID == c.Subject
}

func (s *Server) openAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerID        string `json:"owner_id"`
		Name           string `json:"name"`
		Type           string `json:"type"`
		Currency       string `json:"currency"`
		AllowOverdraft bool   `json:"allow_overdraft"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	c := claimsFrom(r)
	if !c.Can(auth.RoleAdmin) {
		// Customers may only open their own wallet accounts, never overdraft-enabled.
		if body.Type != string(ledger.Liability) || body.AllowOverdraft {
			writeErr(w, 403, "forbidden", "customers can open wallet accounts only")
			return
		}
		body.OwnerID = c.Subject
	}
	a, err := s.svc.OpenAccount(r.Context(), ledger.OpenAccountRequest{
		OwnerID: body.OwnerID, Name: body.Name, Type: ledger.AccountType(body.Type), Currency: strings.ToUpper(body.Currency), AllowOverdraft: body.AllowOverdraft,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	owner := claimsFrom(r).Subject
	if q := r.URL.Query().Get("owner_id"); q != "" && claimsFrom(r).Can(auth.RoleOps, auth.RoleAdmin) {
		owner = q
	}
	as, err := s.svc.Accounts(r.Context(), owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": as})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.Account(r.Context(), r.PathValue("id"))
	if err != nil || !s.canAccess(r, a) {
		writeErr(w, 404, "not_found", "account not found") // don't leak existence
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) statement(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.Account(r.Context(), r.PathValue("id"))
	if err != nil || !s.canAccess(r, a) {
		writeErr(w, 404, "not_found", "account not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	txs, err := s.svc.Statement(r.Context(), a.ID, r.URL.Query().Get("before"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	next := ""
	if len(txs) > 0 && (limit <= 0 || len(txs) == limit) {
		next = txs[len(txs)-1].ID
	}
	writeJSON(w, 200, map[string]any{"data": txs, "next_before": next})
}

func (s *Server) transfer(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, 400, "invalid_request", "Idempotency-Key header is required")
		return
	}
	var body struct {
		From     string            `json:"from_account_id"`
		To       string            `json:"to_account_id"`
		Amount   string            `json:"amount"`
		Metadata map[string]string `json:"metadata"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	amount, err := ledger.ParseAmount(body.Amount)
	if err != nil {
		writeErr(w, 400, "invalid_request", err.Error())
		return
	}
	from, err := s.svc.Account(r.Context(), body.From)
	if err != nil || !s.canAccess(r, from) {
		writeErr(w, 404, "not_found", "source account not found")
		return
	}
	// Idempotency keys are scoped per caller so two users can't collide or probe each other.
	tx, err := s.svc.Transfer(r.Context(), ledger.TransferRequest{
		IdempotencyKey: claimsFrom(r).Subject + ":" + key, FromAccountID: body.From, ToAccountID: body.To, Amount: amount, Metadata: body.Metadata,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, tx)
}

func (s *Server) reverse(w http.ResponseWriter, r *http.Request) {
	if !claimsFrom(r).Can(auth.RoleOps, auth.RoleAdmin) {
		writeErr(w, 403, "forbidden", "ops role required")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if key == "" || strings.TrimSpace(body.Reason) == "" {
		writeErr(w, 400, "invalid_request", "Idempotency-Key and reason are required")
		return
	}
	tx, err := s.svc.Reverse(r.Context(), claimsFrom(r).Subject+":"+key, r.PathValue("id"), body.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, tx)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeErr(w, 404, "not_found", err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeErr(w, 422, "insufficient_funds", "insufficient funds")
	case errors.Is(err, ledger.ErrIdempotencyReuse), errors.Is(err, ledger.ErrAlreadyReversed):
		writeErr(w, 409, "conflict", err.Error())
	case errors.Is(err, ledger.ErrConcurrentModified):
		writeErr(w, 409, "retry", err.Error())
	case errors.Is(err, ledger.ErrInvalid), errors.Is(err, ledger.ErrCurrencyMismatch), errors.Is(err, ledger.ErrUnbalanced):
		writeErr(w, 400, "invalid_request", err.Error())
	case errors.Is(err, ledger.ErrAccountFrozen):
		writeErr(w, 423, "account_frozen", "account frozen")
	default:
		s.log.Error("internal error", "err", err)
		writeErr(w, 500, "internal", "internal error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeErr(w, 400, "invalid_request", "malformed JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "value", v)
				writeErr(w, 500, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
