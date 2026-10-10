// Command spark is booth-spark's module backend: the always-on controller (docs/design-v0.md
// item 1). It serves the published /v1 API and the module's UI, and from later steps runs each
// Spark application or session in its own namespace and proxies the Spark UI. Spark itself never
// runs in this process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/projectbooth/booth-spark/internal/api"
	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/config"
	"github.com/projectbooth/booth-spark/internal/db"
	"github.com/projectbooth/booth-spark/internal/identity"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	// /v1's verifier is built on first use, so the module starts while the identity provider is
	// still coming up (auth.Lazy); an unset issuer leaves /v1 answering 503.
	var tokens auth.TokenVerifier
	if cfg.OIDC.IssuerURL != "" {
		lazy, err := auth.NewLazy(ctx, cfg.OIDC)
		if err != nil {
			return err
		}
		tokens = lazy
	} else {
		log.Printf("oidc: no issuer configured; /v1 answers 503")
	}
	iframe, err := identity.New(ctx, identity.Config{IssuerURL: cfg.IframeIssuerURL, GroupsClaim: cfg.OIDC.GroupsClaim})
	if err != nil {
		return err
	}
	log.Printf("iframe identity: trusting issuer=%s audience=%s", cfg.IframeIssuerURL, identity.ModuleID)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(api.Deps{DB: pool, Tokens: tokens, Iframe: iframe, SubmitMinRole: cfg.SubmitMinRole}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-spark listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
