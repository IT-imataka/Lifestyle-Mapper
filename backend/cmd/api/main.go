package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/lib/pq"
	"github.com/taka/lifestyle-mapper/backend/internal/config"
	"github.com/taka/lifestyle-mapper/backend/internal/controller"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleplaces"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/googleroutes"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/llm"
	"github.com/taka/lifestyle-mapper/backend/internal/infrastructure/rakutentravel"
	"github.com/taka/lifestyle-mapper/backend/internal/prompt"
	"github.com/taka/lifestyle-mapper/backend/internal/repository"
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

	db, err := sql.Open("postgres", cfg.Database.URL.Value())
	if err != nil {
		logger.Error("database open failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		logger.Error("database ping failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

	builder := prompt.MustNew()
	httpClient := &http.Client{}

	collector := planservice.NewCollector(planservice.CollectorDeps{
		Places: googleplaces.New(cfg.Google.MapsAPIKey.Value(), cfg.Google.PlacesTimeout, httpClient),
		Hotels: rakutentravel.New(rakutentravel.Options{
			ApplicationID: cfg.Rakuten.ApplicationID.Value(),
			AffiliateID:   cfg.Rakuten.AffiliateID,
			Timeout:       cfg.Rakuten.Timeout,
			QPS:           cfg.Rakuten.QPS,
			HTTPClient:    httpClient,
		}),
		Routes: googleroutes.New(cfg.Google.MapsAPIKey.Value(), cfg.Google.RoutesTimeout, httpClient),
	}, planservice.CollectorOptions{
		Timeout:                  cfg.Plan.CollectTimeout,
		MaxCandidatesPerCategory: cfg.Plan.MaxCandidatesPerCategory,
	})

	repo := repository.NewPlanRepository(db)
	b := dev.NewBroadcaster()
	chain := llm.NewChain()

	svc := planservice.NewService(planservice.Deps{
		Collector:  collector,
		Repository: repo,
		LLM:        chain,
		Prompt:     builder,
	}, planservice.Options{
		TTL:               cfg.Plan.TTL,
		BaseURL:           cfg.App.BaseURL,
		MaxRepairAttempts: cfg.LLM.MaxRepairAttempts,
		LLMEnabled:        cfg.LLM.Enabled,
		Compose: planservice.ComposeOptions{
			MaxTokens: cfg.LLM.Anthropic.MaxTokens,
			Effort:    cfg.LLM.Anthropic.Effort,
		},
	})

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
