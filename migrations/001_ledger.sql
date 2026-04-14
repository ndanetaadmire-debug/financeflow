-- FinanceFlow double-entry ledger schema (PostgreSQL 15+)

CREATE TABLE accounts (
  id               TEXT PRIMARY KEY,
  owner_id         TEXT        NOT NULL,
  name             TEXT        NOT NULL,
  type             TEXT        NOT NULL CHECK (type IN ('asset','liability','revenue','expense','equity')),
  currency         CHAR(3)     NOT NULL,
  balance          BIGINT      NOT NULL DEFAULT 0,      -- minor units, derived from postings
  allow_overdraft  BOOLEAN     NOT NULL DEFAULT FALSE,
  frozen           BOOLEAN     NOT NULL DEFAULT FALSE,
  version          BIGINT      NOT NULL DEFAULT 0,      -- optimistic concurrency
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Defence in depth: the database itself refuses an overdrawn customer wallet.
  CONSTRAINT wallet_no_overdraft CHECK (type <> 'liability' OR allow_overdraft OR balance >= 0)
);
CREATE INDEX accounts_owner ON accounts (owner_id);

CREATE TABLE transactions (
  id               TEXT PRIMARY KEY,                    -- time-sortable: keyset pagination
  idempotency_key  TEXT        NOT NULL UNIQUE,
  request_hash     TEXT        NOT NULL,
  kind             TEXT        NOT NULL,
  currency         CHAR(3)     NOT NULL,
  metadata         JSONB       NOT NULL DEFAULT '{}',
  reverses_id      TEXT        REFERENCES transactions(id),
  reversed_by_id   TEXT        REFERENCES transactions(id),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE postings (
  transaction_id   TEXT   NOT NULL REFERENCES transactions(id),
  seq              INT    NOT NULL,
  account_id       TEXT   NOT NULL REFERENCES accounts(id),
  amount           BIGINT NOT NULL CHECK (amount <> 0),
  PRIMARY KEY (transaction_id, seq)
);
CREATE INDEX postings_account ON postings (account_id, transaction_id DESC);

-- Every transaction must balance. Checked at COMMIT, after all postings are inserted.
CREATE FUNCTION assert_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT coalesce(sum(amount), 0) FROM postings WHERE transaction_id = NEW.transaction_id) <> 0 THEN
    RAISE EXCEPTION 'transaction % is unbalanced', NEW.transaction_id;
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER postings_balanced
  AFTER INSERT ON postings DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION assert_balanced();

-- Ledger history is immutable: corrections are reversals, never edits.
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'postings are append-only'; END $$;
CREATE TRIGGER postings_immutable BEFORE UPDATE OR DELETE ON postings
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Reconciliation view: stored balance must always equal the sum of postings.
CREATE VIEW balance_drift AS
SELECT a.id, a.balance AS stored, coalesce(sum(p.amount), 0) AS derived
FROM accounts a LEFT JOIN postings p ON p.account_id = a.id
GROUP BY a.id, a.balance
HAVING a.balance <> coalesce(sum(p.amount), 0);
