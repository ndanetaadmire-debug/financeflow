# FinanceFlow

[![CI](https://github.com/ndanetaadmire-debug/financeflow/actions/workflows/ci.yml/badge.svg)](https://github.com/ndanetaadmire-debug/financeflow/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![Dependencies](https://img.shields.io/badge/runtime%20deps-1%20(pgx)-brightgreen)
![Race tested](https://img.shields.io/badge/tests-race%20detector-blue)
![License](https://img.shields.io/badge/license-MIT-blue)

**A double-entry ledger service for wallets, transfers and statements** — the part of a fintech product that must never be wrong. Written in Go using only the standard library, plus the pgx Postgres driver.

Every movement of money is a balanced set of postings. Balances are derived, history is immutable, retries are safe, and concurrent spends cannot overdraw an account.

## Guarantees and how they're enforced

| Guarantee | Application | Database |
|---|---|---|
| **Every transaction balances** (Σ postings = 0) | `Service.prepare` rejects with `ErrUnbalanced` | Deferred constraint trigger checks Σ at `COMMIT` |
| **No overdrawn customer wallets** | Liability accounts checked after applying postings | `CHECK (balance >= 0)` for non-overdraft wallets |
| **No lost updates under concurrency** | Optimistic locking on `accounts.version`, bounded retry | `SERIALIZABLE` + `UPDATE … WHERE version = $n` |
| **Exactly-once effects under retries** | `Idempotency-Key` per caller; same key + different body → `409` | `UNIQUE (idempotency_key)` |
| **History is immutable** | Corrections are reversals (equal and opposite), reversals can't be reversed or doubled | `UPDATE/DELETE` on `postings` raise |
| **Money never floats or overflows** | `int64` minor units, checked addition, strict decimal parsing | `BIGINT` |
| **Stored balance = derived balance** | Single write path through `Service` | `balance_drift` reconciliation view |
| **No deadlocks** | Accounts locked in ID order | — |

These are proven by tests, not just claimed: a **200-goroutine race** to spend from one wallet (run under `-race`) and a **2,000-step randomised property test** that asserts the ledger always nets to zero and no wallet goes negative. CI also applies the schema to Postgres 16 and asserts the database itself rejects an unbalanced transaction.

## Architecture

```mermaid
flowchart LR
    client[Web / mobile dashboard] -->|JWT + Idempotency-Key| api
    subgraph svc[financeflow]
      api[internal/api<br/>net/http, RBAC, ownership checks] --> ledger[internal/ledger<br/>Service: invariants, retries]
      ledger --> port[[ledger.Store]]
    end
    port --> mem[memstore<br/>tests & demo]
    port --> pg[pgstore<br/>database/sql · SERIALIZABLE]
    pg --> db[(PostgreSQL)]
```

- **`internal/ledger`** — domain and invariants. Knows nothing about HTTP or SQL.
- **`internal/store`** — two implementations of one `Store` port with identical atomicity semantics.
- **`internal/auth`** — HS256 JWT verification in ~100 lines of stdlib: algorithm pinned (defeats `alg: none`), `exp`/`nbf`/`iss`/`aud` enforced, roles allow-listed.
- **`internal/api`** — Go 1.22 pattern routing, strict JSON decoding (`DisallowUnknownFields`, 64 KB cap), ownership enforced on every account access, 404 instead of 403 so account IDs can't be probed, panic recovery, structured `slog` logs, hardened server timeouts, graceful shutdown.

More in **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** and **[docs/adr](docs/adr)**.

## API

| Method | Path | Who | Notes |
|---|---|---|---|
| `POST` | `/v1/accounts` | customer, admin | Customers can only open their own non-overdraft wallets |
| `GET` | `/v1/accounts` | any | Own accounts; ops/admin may pass `owner_id` |
| `GET` | `/v1/accounts/{id}` | owner, ops | |
| `GET` | `/v1/accounts/{id}/transactions?before=&limit=` | owner, ops | Keyset pagination on time-sortable IDs |
| `POST` | `/v1/transfers` | owner of source | **Requires `Idempotency-Key`** |
| `POST` | `/v1/transactions/{id}/reverse` | ops, admin | Requires `Idempotency-Key` and a reason |

```bash
make run    # in-memory store on :8080
make test   # go test -race -cover ./...
```

Errors are typed: `400 invalid_request`, `401`, `403`, `404`, `409 conflict|retry`, `422 insufficient_funds`, `423 account_frozen`.

## Run against PostgreSQL

The pgx driver is built in. Apply the schema, then point the service at your database:

```bash
psql "$DATABASE_URL" -f migrations/001_ledger.sql
DATABASE_URL="postgres://user:pass@localhost:5432/financeflow?sslmode=disable" \
JWT_SECRET="at-least-32-characters-of-secret" \
go run ./cmd/financeflow
```

Without `DATABASE_URL` it falls back to the in-memory store, which is useful for demos and tests. The service runs as a static binary on distroless as a non-root user.

---

Built by **[Admire T. Ndaneta](https://admire-portfolio.vercel.app)** — senior full-stack engineer. MIT licensed.
