// Command server runs the CodeDocs AI web application.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/quantamsage/ai-code-documentation-tool/internal/config"
	"github.com/quantamsage/ai-code-documentation-tool/internal/database"
	"github.com/quantamsage/ai-code-documentation-tool/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := database.Migrate(ctx, db)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	log.Info("database ready", slog.String("path", cfg.DatabasePath), slog.Any("migrations_applied", applied))

	srv, err := server.New(cfg, db, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
