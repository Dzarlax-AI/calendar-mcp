package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"calendar-mcp/internal/config"
	"calendar-mcp/internal/connections"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/providers"
	"calendar-mcp/internal/storage"
)

func runAppleTimezoneRefresh(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("refresh-apple-timezones", flag.ContinueOnError)
	flags.SetOutput(out)
	connectionID := flags.String("connection", "", "stored Apple connection ID")
	flags.Usage = func() {
		fmt.Fprintln(out, "Usage: calendar refresh-apple-timezones --connection ID CALENDAR_ID [CALENDAR_ID ...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *connectionID == "" || flags.NArg() == 0 || flags.NArg() > 16 {
		flags.Usage()
		return errUsage
	}

	cfg := config.Load()
	cipher, err := credentials.NewCipher(cfg.EncryptionKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	store, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.CheckSchema(ctx); err != nil {
		return err
	}
	service := connections.New(store, cipher)
	factory := providers.NewFactory(cfg, store, service)
	changed, err := service.RefreshAppleCalendarTimezones(ctx, *connectionID, flags.Args(), factory.BuildNonGoogleVerification)
	if err != nil {
		return err
	}
	return jsonOutput(out, map[string]int{"verified": flags.NArg(), "updated": changed})
}
