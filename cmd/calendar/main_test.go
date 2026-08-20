package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestRunDispatchesCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "serve", args: []string{"serve"}, want: "serve"},
		{name: "worker", args: []string{"worker"}, want: "worker"},
		{name: "import legacy", args: []string{"import-legacy", "--preview"}, want: "import-legacy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := ""
			commands := commandSet{
				serve:  func(context.Context) error { called = "serve"; return nil },
				worker: func(context.Context) error { called = "worker"; return nil },
				importLegacy: func(_ context.Context, args []string, _ io.Writer) error {
					called = "import-legacy"
					if len(args) != 1 || args[0] != "--preview" {
						t.Fatalf("import args = %#v", args)
					}
					return nil
				},
			}

			if err := run(context.Background(), tt.args, commands, &bytes.Buffer{}); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if called != tt.want {
				t.Fatalf("called = %q, want %q", called, tt.want)
			}
		})
	}
}

func TestRunRejectsMissingAndUnknownCommands(t *testing.T) {
	commands := commandSet{
		serve:  func(context.Context) error { return nil },
		worker: func(context.Context) error { return nil },
	}

	for _, args := range [][]string{nil, {"unknown"}, {"serve", "extra"}} {
		if err := run(context.Background(), args, commands, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Fatalf("run(%v) error = %v, want errUsage", args, err)
		}
	}
}

func TestRunPropagatesCommandError(t *testing.T) {
	wantErr := errors.New("serve failed")
	commands := commandSet{
		serve:  func(context.Context) error { return wantErr },
		worker: func(context.Context) error { return nil },
	}

	if err := run(context.Background(), []string{"serve"}, commands, &bytes.Buffer{}); !errors.Is(err, wantErr) {
		t.Fatalf("run() error = %v, want %v", err, wantErr)
	}
}

func TestRunPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	commands := commandSet{}

	if err := run(context.Background(), []string{"--help"}, commands, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got := out.String(); got != usage+"\n" {
		t.Fatalf("help = %q, want %q", got, usage+"\n")
	}
}
