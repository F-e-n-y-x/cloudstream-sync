// Command server runs the CloudStream self-hosted sync service.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/F-e-n-y-x/cloudstream-sync/internal/api"
	"github.com/F-e-n-y-x/cloudstream-sync/internal/store"
)

func main() {
	var (
		addr             = flag.String("addr", envOr("SYNC_ADDR", ":9909"), "listen address")
		dbPath           = flag.String("db", envOr("SYNC_DB", "/data/cloudstream-sync.db"), "path to the SQLite database")
		openRegistration = flag.Bool("open-registration", envBool("SYNC_OPEN_REGISTRATION", false),
			"allow anyone who can reach the server to create an account")
		healthcheck = flag.Bool("healthcheck", false,
			"probe a running server and exit 0 if healthy; used by the container healthcheck")
	)
	flag.Parse()

	// The runtime image is scratch, so there is no shell, curl or wget for a healthcheck to
	// use. The binary probes itself instead.
	if *healthcheck {
		os.Exit(runHealthcheck(*addr))
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if dir := filepath.Dir(*dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("create data directory", "dir", dir, "err", err)
			os.Exit(1)
		}
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open store", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	if *openRegistration {
		// Worth saying out loud: this is the one setting that decides whether a stranger
		// who finds the URL can use the server.
		log.Warn("open registration is enabled; anyone who can reach this server can create an account")
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, log, *openRegistration).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go purgeExpiredCodes(ctx, st, log)
	go runDiscoveryResponder(ctx, *addr, log)

	go func() {
		log.Info("listening", "addr", *addr, "db", *dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}

// purgeExpiredCodes clears out spent pairing codes so the table does not grow without bound
// on a long-running instance.
func purgeExpiredCodes(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.PurgeExpiredPairCodes(ctx); err != nil {
				log.Warn("purge pairing codes", "err", err)
			}
		}
	}
}

// runHealthcheck returns a process exit code: 0 when the server answers /healthz.
func runHealthcheck(addr string) int {
	// The listen address may omit the host, as ":8080" does.
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}
