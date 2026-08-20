package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"calendar-mcp/internal/migration"
	appruntime "calendar-mcp/internal/runtime"
	"calendar-mcp/internal/storage"
)

const usage = "Usage: calendar <serve|worker|import-legacy>"

var errUsage = errors.New("usage: calendar <serve|worker|import-legacy>")

type commandSet struct {
	serve        func(context.Context) error
	worker       func(context.Context) error
	importLegacy func(context.Context, []string, io.Writer) error
}

func main() {
	commands := commandSet{serve: appruntime.Serve, worker: appruntime.Worker, importLegacy: runLegacyImport}
	if err := run(context.Background(), os.Args[1:], commands, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, commands commandSet, out io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		if len(args) != 1 {
			return errUsage
		}
		_, err := fmt.Fprintln(out, usage)
		return err
	case "serve":
		if len(args) != 1 {
			return errUsage
		}
		return commands.serve(ctx)
	case "worker":
		if len(args) != 1 {
			return errUsage
		}
		return commands.worker(ctx)
	case "import-legacy":
		if commands.importLegacy == nil {
			return errors.New("legacy import command is unavailable")
		}
		return commands.importLegacy(ctx, args[1:], out)
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

func runLegacyImport(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("import-legacy", flag.ContinueOnError)
	flags.SetOutput(out)
	stateFile := flags.String("state-file", "", "path to calendar-sync JSON state")
	source := flags.String("source", "", "legacy source calendar, for example microsoft:id")
	target := flags.String("target", "", "legacy target calendar, for example google:id")
	databaseURL := flags.String("database-url", os.Getenv("DATABASE_URL"), "destination PostgreSQL or SQLite URL")
	preview := flags.Bool("preview", false, "validate and print the import summary without writing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unexpected import arguments", errUsage)
	}
	plan, err := migration.Load(*stateFile, *source, *target, time.Now().UTC())
	if err != nil {
		return err
	}
	if *preview {
		return jsonOutput(out, plan.Preview())
	}
	if *databaseURL == "" {
		return errors.New("DATABASE_URL or --database-url is required unless --preview is used")
	}
	store, err := storage.Open(ctx, *databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	resolvedSource, err := store.ResolveCalendarReference(ctx, plan.Rule.SourceCalendarID)
	if err != nil {
		return fmt.Errorf("resolve legacy source calendar: %w", err)
	}
	resolvedTarget, err := store.ResolveCalendarReference(ctx, plan.Rule.TargetCalendarID)
	if err != nil {
		return fmt.Errorf("resolve legacy target calendar: %w", err)
	}
	plan.Rule.SourceCalendarID = resolvedSource
	plan.Rule.TargetCalendarID = resolvedTarget
	if err := migration.Import(ctx, store, plan); err != nil {
		return err
	}
	return jsonOutput(out, struct {
		Status string `json:"status"`
		migration.Preview
	}{Status: "imported", Preview: plan.Preview()})
}

func jsonOutput(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
