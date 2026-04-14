package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ndanetaadmire-debug/financeflow/internal/api"
	"github.com/ndanetaadmire-debug/financeflow/internal/auth"
	"github.com/ndanetaadmire-debug/financeflow/internal/ledger"
	"github.com/ndanetaadmire-debug/financeflow/internal/store/memstore"
	"github.com/ndanetaadmire-debug/financeflow/internal/store/pgstore"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database driver
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		log.Error("JWT_SECRET must be at least 32 bytes")
		os.Exit(1)
	}

	var store ledger.Store = memstore.New()
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		db, err := sql.Open(envOr("DATABASE_DRIVER", "pgx"), dsn)
		if err != nil {
			log.Error("open database", "err", err)
			os.Exit(1)
		}
		db.SetMaxOpenConns(20)
		db.SetConnMaxIdleTime(5 * time.Minute)
		store = pgstore.New(db)
	} else {
		log.Warn("DATABASE_URL not set: using in-memory store (data is lost on restart)")
	}

	svc := ledger.NewService(store, nil)
	handler := api.New(svc, auth.Verifier{Secret: []byte(secret), Issuer: envOr("JWT_ISSUER", "financeflow"), Audience: envOr("JWT_AUDIENCE", "financeflow-api"), Leeway: 30 * time.Second}, log)

	srv := &http.Server{
		Addr: ":" + envOr("PORT", "8080"), Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Info("stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
