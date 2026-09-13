package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/taka/lifestyle-mapper/backend/internal/config"
	"github.com/taka/lifestyle-mapper/backend/internal/controller"
	"github.com/taka/lifestyle-mapper/backend/internal/prompt"
	"github.com/taka/lifestyle-mapper/backend/internal/router"
	dev "github.com/taka/lifestyle-mapper/backend/internal/runtime"
	planservice "github.com/taka/lifestyle-mapper/backend/internal/service/plan"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	logger := slog.Default()

	// 開発向けの軽量実装を注入する。
	// prompt builder
	builder := prompt.MustNew()

	// Collector: nil の依存は skipped 扱いになるため、外部呼び出しは行わない。
	collector := planservice.NewCollector(planservice.CollectorDeps{}, planservice.CollectorOptions{})

	repo := dev.NewInMemoryPlanRepository()
	b := dev.NewBroadcaster()

	svc := planservice.NewService(planservice.Deps{
		Collector:  collector,
		Repository: repo,
		LLM:        nil,
		Prompt:     builder,
	}, planservice.Options{LLMEnabled: false})

	orchestrator := dev.NewPlanOrchestrator(svc, repo, b)

	deps := router.Deps{
		Plan:   controller.NewPlanController(orchestrator, orchestrator, nil),
		Stream: controller.NewStreamController(b, nil, 0),
		Click:  controller.NewClickController(dev.NewClickRecorder(), nil, nil),
		Health: controller.NewHealthController(cfg.App.Version),
	}

	h := router.New(router.Config{
		CORSOrigins:        cfg.Server.CORSOrigins,
		RateLimitPerMinute: cfg.Server.RateLimitPerMinute,
		TrustProxy:         false,
		Logger:             logger,
	}, deps)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      h,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// 起動ログ
	logger.Info("starting api server", slog.Int("port", cfg.Server.Port), slog.String("env", string(cfg.App.Env)))

	// Listen in background so we can handle shutdown signals.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// シグナル待ち
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	case err := <-errCh:
		if err != nil {
			logger.Error("server error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("server stopped")
}
