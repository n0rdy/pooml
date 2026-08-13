package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/n0rdy/pooml/api"
	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/services"
	"github.com/n0rdy/pooml/ui"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	minSecretLength = 32
)

func main() {
	env := getEnv()
	setupLogging(env)

	uiSecret := getRequiredSecret("POOML_UI_SECRET")
	_ = getRequiredSecret("POOML_ENCRYPTION_KEY") // FIXME: pass to encryption service when meta.db secrets are wired
	dbDir := getRequiredDBDir()
	apiAddr, uiAddr := getServerAddrs()
	trustProxyHeaders := getTrustProxyHeaders()
	shutdownTimeout := getShutdownTimeout()

	log.Info().
		Str("env", env).
		Str("api_addr", apiAddr).
		Str("ui_addr", uiAddr).
		Str("db_dir", dbDir).
		Bool("trust_proxy_headers", trustProxyHeaders).
		Msg("starting pooml")

	// Migrations run synchronously on startup (idempotent - no-op when already up to date).
	// Must run before OpenPools: the read-only logs pool needs the file to already exist.
	db.MigrateAll(dbDir)

	pools, err := db.OpenPools(dbDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open database pools")
	}

	// Shutdown spine for long-lived background goroutines. Ingestion pipelines
	// are deliberately not on it - see stage 2 below.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	var jobsWG sync.WaitGroup

	// Services
	monitoringService := services.NewMonitoringService(pools)
	sessionsService := services.NewSessionsService()
	throttlingService := services.NewThrottlingService()
	apiKeysService := services.NewApiKeysService(pools.Meta)

	startJob(appCtx, &jobsWG, "sessions-sweeper", sessionsService.RunSweeper)
	startJob(appCtx, &jobsWG, "throttling-sweeper", throttlingService.RunSweeper)

	// Log ingestion pipeline (broadcaster is shared with the SSE handlers in M6)
	broadcaster := ingestion.NewBroadcaster()
	pipeline := ingestion.NewPipeline(pools.LogsWrite, broadcaster)
	pipeline.Start()

	// Routers
	apiRouter := api.NewRouter(monitoringService, throttlingService, apiKeysService, pipeline, env, trustProxyHeaders)
	uiRouter := ui.NewRouter(sessionsService, throttlingService, apiKeysService, uiSecret, env, trustProxyHeaders)

	apiServer := newAPIServer(apiAddr, apiRouter.NewRouter())
	uiServer := newUIServer(uiAddr, uiRouter.NewRouter())

	// Start servers; capture failures so an unexpected listener exit triggers shutdown.
	serverErrCh := make(chan error, 2)
	go func() {
		log.Info().Str("addr", apiAddr).Msg("API server listening")
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()
	go func() {
		log.Info().Str("addr", uiAddr).Msg("UI server listening")
		if err := uiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	// Wait for SIGTERM/SIGINT or a server failure.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info().Str("signal", sig.String()).Msg("shutdown signal received")
	case err := <-serverErrCh:
		log.Error().Err(err).Msg("HTTP server failed unexpectedly; initiating shutdown")
	}

	// --- Staged graceful shutdown ---

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	// Stage 1: HTTP servers in parallel.
	// Server.Shutdown stops accepting new connections and waits for in-flight
	// requests to finish, bounded by shutdownCtx. SSE handlers exit when their
	// request goroutines are cancelled by the deadline.
	var httpWG sync.WaitGroup
	httpWG.Add(2)
	go func() {
		defer httpWG.Done()
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			log.Warn().Err(err).Msg("API server shutdown returned an error")
		}
	}()
	go func() {
		defer httpWG.Done()
		if err := uiServer.Shutdown(shutdownCtx); err != nil {
			log.Warn().Err(err).Msg("UI server shutdown returned an error")
		}
	}()
	httpWG.Wait()
	log.Info().Msg("HTTP servers shut down")

	// Stage 2: drain the log ingestion pipeline. Channel close, not appCtx
	// cancellation - the batch writer must survive to flush its final batch.
	// Bounded by the shutdown deadline; a stuck flush loses its rows by design.
	drained := make(chan struct{})
	go func() {
		pipeline.Shutdown()
		close(drained)
	}()
	select {
	case <-drained:
		log.Info().Msg("ingestion pipeline drained")
	case <-shutdownCtx.Done():
		log.Warn().Msg("shutdown deadline reached with ingestion pipeline still draining; proceeding anyway")
	}

	// Stage 3 (M9): drain the metrics ingestion pipeline the same way.

	// Stage 4: stop background jobs. Only safe once the pipelines above have
	// drained, since later jobs (retention, backup, optimize) share the pools.
	appCancel()
	if waitWithContext(&jobsWG, shutdownCtx) {
		log.Info().Msg("background jobs stopped")
	} else {
		log.Warn().Msg("shutdown deadline reached with background jobs still running; proceeding anyway")
	}

	// Stage 5: close DB pools.
	pools.Close()

	log.Info().Msg("clean shutdown complete")
}

// --- shutdown spine ---

// startJob runs fn on its own goroutine; fn must return when ctx is cancelled.
// A panicking job stays dead rather than taking the process with it: a log
// server that exits on a sweeper bug loses the logs explaining the bug.
func startJob(ctx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("job", name).Msg("background job panicked and will not restart")
			}
		}()
		fn(ctx)
	}()
}

// waitWithContext reports whether wg finished before ctx was done.
func waitWithContext(wg *sync.WaitGroup, ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func newAPIServer(addr string, handler http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)

	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		Protocols:         &protocols,
	}
}

func newUIServer(addr string, handler http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	protocols.SetHTTP1(true)

	// SSE handlers stream for hours; WriteTimeout and IdleTimeout left at zero
	// so a long-lived live tail isn't killed by the server. Per-request timeouts
	// for non-SSE pages can be added inside handlers via context.WithTimeout
	// where they actually matter.
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		Protocols:         &protocols,
	}
}

// --- env loading ---

func getEnv() string {
	env := os.Getenv("POOML_ENV")
	if env == "" {
		env = common.ProEnv
	}
	if !common.SupportedEnvs[env] {
		log.Fatal().Str("env", env).Msg("unsupported POOML_ENV value (allowed: local, pro)")
	}
	return env
}

func setupLogging(env string) {
	level := strings.ToLower(strings.TrimSpace(os.Getenv("POOML_LOG_LEVEL")))
	if level == "" {
		level = "info"
	}
	parsed, err := zerolog.ParseLevel(level)
	if err != nil {
		log.Fatal().Err(err).Str("level", level).Msg("invalid POOML_LOG_LEVEL")
	}
	zerolog.SetGlobalLevel(parsed)

	if env == common.LocalEnv {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}
}

func getRequiredSecret(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatal().Msgf("%s is required", name)
	}
	if len(v) < minSecretLength {
		log.Fatal().Msgf("%s is too short: must be at least %d characters", name, minSecretLength)
	}
	return v
}

func getRequiredDBDir() string {
	v := os.Getenv("POOML_DB_DIR")
	if v == "" {
		log.Fatal().Msg("POOML_DB_DIR is required")
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		log.Fatal().Err(err).Str("path", v).Msg("failed to resolve absolute path for POOML_DB_DIR")
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Fatal().Err(err).Str("path", abs).Msg("failed to create POOML_DB_DIR")
	}
	return abs
}

func getServerAddrs() (string, string) {
	apiAddr := os.Getenv("POOML_API_ADDR")
	if apiAddr == "" {
		apiAddr = "localhost:8080"
	}
	uiAddr := os.Getenv("POOML_UI_ADDR")
	if uiAddr == "" {
		uiAddr = "localhost:8081"
	}
	return apiAddr, uiAddr
}

func getTrustProxyHeaders() bool {
	v := os.Getenv("POOML_TRUST_PROXY_HEADERS")
	if v == "" {
		return false
	}
	trust, err := strconv.ParseBool(v)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse POOML_TRUST_PROXY_HEADERS")
	}
	if trust {
		log.Warn().Msg("POOML_TRUST_PROXY_HEADERS=true: client IP will be read from X-Forwarded-For. " +
			"Pooml MUST be behind a reverse proxy that strips or replaces incoming X-Forwarded-For from clients, " +
			"otherwise IPs are spoofable and throttling can be bypassed.")
	}
	return trust
}

func getShutdownTimeout() time.Duration {
	v := os.Getenv("POOML_SHUTDOWN_TIMEOUT_SECONDS")
	if v == "" {
		return 30 * time.Second
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse POOML_SHUTDOWN_TIMEOUT_SECONDS")
	}
	if secs < 1 {
		log.Fatal().Msg("POOML_SHUTDOWN_TIMEOUT_SECONDS must be at least 1")
	}
	return time.Duration(secs) * time.Second
}
