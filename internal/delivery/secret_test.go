package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveReadsTheEnvironment(t *testing.T) {
	t.Setenv("CHARON_TEST_SIGNING", "s3cret")

	got, err := resolve("env:CHARON_TEST_SIGNING")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if string(got) != "s3cret" {
		t.Errorf("got %q, want %q", got, "s3cret")
	}
}

func TestResolveReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	// Trailing newline because every editor and every `echo` adds one, and a
	// secret that differs from the receiver's by an invisible byte is the
	// worst possible way to spend an afternoon.
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("writing the secret: %v", err)
	}

	got, err := resolve("file:" + path)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if string(got) != "s3cret" {
		t.Errorf("got %q, want %q", got, "s3cret")
	}
}

func TestResolveRefusesWhatItCannotRead(t *testing.T) {
	t.Setenv("CHARON_TEST_EMPTY", "")

	for name, reference := range map[string]string{
		"a variable that is not set": "env:CHARON_TEST_ABSENT",
		"a variable that is empty":   "env:CHARON_TEST_EMPTY",
		"a file that is not there":   "file:/nonexistent/charon/secret",
		"no location at all":         "CHARON_TEST_SIGNING",
		"an empty reference":         "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolve(reference); err == nil {
				t.Errorf("%q resolved to something", reference)
			}
		})
	}
}

func TestResolveAllStopsAtTheFirstItCannotRead(t *testing.T) {
	t.Setenv("CHARON_TEST_SIGNING", "s3cret")

	_, err := resolveAll([]string{"env:CHARON_TEST_SIGNING", "env:CHARON_TEST_ABSENT"})
	if err == nil {
		t.Fatal("a secret that cannot be read has to fail the attempt")
	}
	if !strings.Contains(err.Error(), "CHARON_TEST_ABSENT") {
		t.Errorf("the error should name what is missing, got %v", err)
	}
}
