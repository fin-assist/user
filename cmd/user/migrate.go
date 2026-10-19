package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/config"
)

func runMigrate(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usageError(usage)
	}
	cfg, err := migrateConfig()
	if err != nil {
		return err
	}
	all := app.MigrationSources()

	switch args[0] {
	case "up":
		if len(args) != 1 {
			return usageError(usage)
		}
		return migrate.Up(ctx, cfg, all)

	case "version":
		if len(args) != 1 {
			return usageError(usage)
		}
		st, err := migrate.Versions(ctx, cfg, all)
		if err != nil {
			return err
		}
		for _, s := range st {
			dirty := ""
			if s.Dirty {
				dirty = " (dirty)"
			}
			_, _ = fmt.Fprintf(out, "%s\t%d%s\n", s.Source, s.Version, dirty)
		}
		return nil

	case "down", "force":
		if len(args) != 3 {
			return usageError(usage)
		}
		src, err := migrate.Find(all, args[1])
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return usageError(fmt.Sprintf("%s: %q is not a number\n%s", args[0], args[2], usage))
		}
		if args[0] == "down" {
			return migrate.Down(ctx, cfg, src, n)
		}
		return migrate.Force(ctx, cfg, src, n)

	default:
		return usageError(usage)
	}
}

// migrateConfig reads the configuration from the environment: DB_PRIMARY_DSN (or _FILE) and
// the MIGRATE_* timeouts, declared with their defaults in internal/config (docs/config.md).
func migrateConfig() (migrate.Config, error) {
	c, err := config.LoadMigrate()
	if err != nil {
		return migrate.Config{}, err
	}
	cfg := migrate.DefaultConfig(c.PrimaryDSN)
	cfg.Logger = slog.Default()
	cfg.LockTimeout, cfg.StatementTimeout, cfg.WaitTimeout = c.LockTimeout, c.StatementTimeout, c.WaitTimeout
	return cfg, nil
}
