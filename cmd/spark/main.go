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

	"github.com/projectbooth/booth-spark/internal/agent"
	"github.com/projectbooth/booth-spark/internal/api"
	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/config"
	"github.com/projectbooth/booth-spark/internal/dataaccess"
	"github.com/projectbooth/booth-spark/internal/db"
	"github.com/projectbooth/booth-spark/internal/identity"
	"github.com/projectbooth/booth-spark/internal/runs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const shutdownTimeout = 10 * time.Second

func main() {
	// In a data run's pods the same binary is the agent (internal/agent): no database, no config.
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := agent.Main(ctx, os.Args[2:])
		stop()
		os.Exit(code)
	}
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

	store := runs.NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrating the database: %w", err)
	}
	deps := api.Deps{DB: pool, Tokens: tokens, Iframe: iframe, SubmitMinRole: cfg.SubmitMinRole, Runs: api.StoreRuns{Store: store}}
	if rc := cfg.Runs; rc != nil {
		kcfg, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		kube, err := kubernetes.NewForConfig(kcfg)
		if err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		launcher := &runs.Launcher{Kube: kube, Cluster: runs.Cluster{
			Instance: rc.Instance, ReleaseNamespace: rc.Namespace, BackendServiceAccount: rc.ServiceAccount,
			BackendPodLabels: rc.BackendPodLabels, DriverClusterRole: rc.DriverClusterRole,
			RunControllerClusterRole: rc.RunControllerClusterRole, Image: rc.Image,
			ImagePullPolicy: corev1.PullPolicy(rc.ImagePullPolicy), Driver: rc.Driver.Placement, Executor: rc.Executor.Placement,
			DriverCPU: rc.Driver.CPU, ExecutorCPU: rc.Executor.CPU, Egress: rc.Egress,
		}}
		ctrl := &runs.Controller{Store: store, Cluster: launcher, Interval: 3 * time.Second,
			PendingTimeout: rc.PendingTimeoutD, LogTailBytes: 1 << 20, ResultRetention: cfg.SessionResultRetention, Now: time.Now}
		if d := cfg.Data; d != nil {
			// Data access (docs/design-v0.md item 4): one token per run, minted here and served to
			// the run's agents on the internal port, which run namespaces alone reach.
			tokens := &dataaccess.Tokens{Minter: &dataaccess.CoreMinter{URL: d.MintURL, Credential: d.MintCredential}, MaxAge: d.RefreshMaxD}
			ctrl.Data = &dataaccess.Planner{Tokens: tokens, CoreURL: d.CoreURL}
			launcher.Cluster.Data = &runs.DataCluster{
				AgentImage: d.AgentImage, SidecarImage: d.Sidecar.Image, InternalURL: d.InternalURL, InternalPort: d.InternalPort,
				Database: d.Database.Egress, ObjectStore: d.ObjectStore.Egress, RefreshMax: d.RefreshMax,
			}
			internal := &http.Server{Addr: d.InternalAddr, ReadHeaderTimeout: 10 * time.Second,
				Handler: (&dataaccess.Internal{Store: store, Tokens: tokens, CoreURL: d.CoreURL}).Router()}
			go func() {
				<-ctx.Done()
				_ = internal.Close()
			}()
			go func() {
				log.Printf("data access: internal port on %s; database=%t lakehouse=%t storage=%t", d.InternalAddr, d.Database.Enabled, d.Lakehouse.Enabled, d.Storage.Enabled)
				if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Fatalf("internal port: %v", err)
				}
			}()
		}
		go ctrl.Run(ctx)
		deps.Applications = &api.Applications{
			Store: store,
			Limits: runs.Limits{MaxExecutors: rc.MaxExecutors, DefaultExecutors: rc.DefaultExecutors,
				DriverMemory: rc.Driver.Memory, ExecutorMemory: rc.Executor.Memory, MaxMemory: rc.MaxMemory, MaxDuration: rc.MaxDurationD,
				SessionIdleTimeout: cfg.SessionIdleTimeout, SessionMaxLifetime: cfg.SessionMaxLifetime, Data: cfg.Data.Limits()},
			Admission: runs.Admission{MaxRunning: rc.MaxRunning, MaxRunningPerWorkspace: rc.MaxRunningPerWorkspace, MemoryBudgetMi: rc.MemoryBudgetMi},
			LiveLogs: func(ctx context.Context, ns string, tail int64) (string, error) {
				return launcher.Logs(ctx, ns, tail, 1<<20)
			},
		}
		log.Printf("runs: instance=%s image=%s maxRunning=%d memoryBudget=%dMi egress=%s sessions: idle=%s lifetime=%s",
			rc.Instance, rc.Image, rc.MaxRunning, rc.MemoryBudgetMi, rc.Egress.Mode, cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
	} else {
		log.Printf("runs: BOOTH_RUNS is not set; no runs can be submitted")
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(deps),
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
