package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/collector/kube"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/demo"
	"github.com/yousafkhamza/changefeed/internal/server"
	"github.com/yousafkhamza/changefeed/internal/store"
	"github.com/yousafkhamza/changefeed/internal/version"
)

func serve(args []string, stderr io.Writer) error {
	fs := newFlags("serve")
	cfgPath := fs.String("config", os.Getenv("CHANGEFEED_CONFIG"), "config file (env CHANGEFEED_CONFIG)")
	demoMode := fs.Bool("demo", false, "load a sample incident into an in-memory database (read-only UI, no auth)")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig for running outside a cluster")
	logFormat := fs.String("log-format", "json", "json or text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var handler slog.Handler = slog.NewJSONHandler(stderr, nil)
	if *logFormat == "text" {
		handler = slog.NewTextHandler(stderr, nil)
	}
	log := slog.New(handler).With("app", "changefeed")

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *demoMode {
		cfg.Storage.Path = ":memory:"
		cfg.Secrets.DatabaseURL = ""
		cfg.Auth.AnonymousRead = true
		cfg.Collectors.Kubernetes.Enabled = false
		demo.Config(&cfg)
		log.Warn("demo mode: in-memory sample data, anyone who can reach this port can read it")
	}

	st, err := store.Open(cfg.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	var gh *auth.GitHubVerifier
	if cfg.Auth.GitHub.Enabled {
		gh = auth.NewGitHubVerifier(cfg.Auth.GitHub.Issuer, cfg.Auth.GitHub.Audience, cfg.Auth.GitHub.Repositories, cfg.Auth.GitHub.Environments)
	}
	var sso *auth.SSO
	if cfg.Auth.OIDC.Enabled {
		sso = auth.NewSSO(cfg.Auth.OIDC, cfg.Auth.Teams, cfg.Secrets.OIDCClientSecret, cfg.Secrets.SessionKey)
	}
	if len(cfg.Auth.Tokens) == 0 && gh == nil && sso == nil && !cfg.Auth.AnonymousRead {
		log.Warn("no tokens, GitHub OIDC or SSO configured: nobody can read or write; run `changefeed token` to create one")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := server.New(cfg, st, auth.New(cfg.Auth, gh, sso), log, reg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *demoMode {
		if err := demo.Seed(ctx, srv, time.Now()); err != nil {
			return err
		}
		if err := demo.SeedIncidents(ctx, st, time.Now()); err != nil {
			return err
		}
	}
	go func() {
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for {
			srv.RefreshMetrics(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	errc := make(chan error, 2)
	if cfg.Collectors.Kubernetes.Enabled {
		rc, err := restConfig(*kubeconfig)
		if err != nil {
			return fmt.Errorf("kubernetes collector: %w", err)
		}
		rc.UserAgent = "changefeed/" + version.Version
		client, err := kubernetes.NewForConfig(rc)
		if err != nil {
			return err
		}
		meta, err := metadata.NewForConfig(rc)
		if err != nil {
			return err
		}
		col := kube.New(cfg.Collectors.Kubernetes, client, meta, srv, log.With("collector", "kubernetes"))
		go func() {
			if err := col.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errc <- fmt.Errorf("kubernetes collector: %w", err)
			}
		}()
	}

	go retention(ctx, st, cfg.Storage.Retention.Duration, log)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version.Version, "storage", st.Backend(), "slack", cfg.Slack.Enabled, "sso", sso != nil)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		stop()
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// retention deletes expired events hourly.
func retention(ctx context.Context, st *store.Store, keep time.Duration, log *slog.Logger) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		n, err := st.Purge(ctx, time.Now().Add(-keep))
		if err != nil && ctx.Err() == nil {
			log.Error("retention purge failed", "err", err)
		} else if n > 0 {
			log.Info("retention purge", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
