package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantErr    error
		wantStdout string
	}{
		{
			name:       "version prints the build version",
			args:       []string{"version"},
			wantStdout: "charon " + version,
		},
		{
			name:       "no arguments prints usage",
			args:       nil,
			wantStdout: "Usage:",
		},
		{
			name:       "help prints usage",
			args:       []string{"help"},
			wantStdout: "Usage:",
		},
		{
			name:    "unknown command is an error",
			args:    []string{"nope"},
			wantErr: errUnknownCommand,
		},
		{
			name:    "serve without a database url is an error",
			args:    []string{"serve"},
			wantErr: errMissingDatabaseURL,
		},
		{
			name:    "migrate without a database url is an error",
			args:    []string{"migrate"},
			wantErr: errMissingDatabaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(context.Background(), tt.args, &stdout, &stderr)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("run() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() unexpected error: %v", err)
			}
			if got := stdout.String(); !strings.Contains(got, tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", got, tt.wantStdout)
			}
		})
	}
}
