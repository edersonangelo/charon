package delivery

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Where a signing secret is kept. The database holds one of these and never
// the secret itself, so a dump of it forges nothing.
const (
	inEnvironment = "env:"
	inFile        = "file:"
)

var errUnlocatedSecret = errors.New(
	"a signing secret is named env:VARIABLE or file:/path")

// CanRead reports whether a reference resolves here, which is what the
// process that delivers knows and the panel cannot: they run apart, with
// different environments and different filesystems.
func CanRead(reference string) error {
	_, err := resolve(reference)
	return err
}

// resolve reads the secret a reference points at.
//
// A file is read on every attempt rather than held: a secret manager that
// projects one into the filesystem rewrites it in place, and re-reading is
// what makes a rotation take effect without restarting anything here.
func resolve(reference string) ([]byte, error) {
	switch {
	case strings.HasPrefix(reference, inEnvironment):
		name := strings.TrimPrefix(reference, inEnvironment)
		value, present := os.LookupEnv(name)
		if !present {
			return nil, fmt.Errorf("%s is not set in this process", name)
		}
		if value == "" {
			return nil, fmt.Errorf("%s is set but empty", name)
		}
		return []byte(value), nil

	case strings.HasPrefix(reference, inFile):
		path := strings.TrimPrefix(reference, inFile)
		value, err := os.ReadFile(path) //nolint:gosec // the path is what an operator configured
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		trimmed := strings.TrimRight(string(value), "\r\n")
		if trimmed == "" {
			return nil, fmt.Errorf("%s is empty", path)
		}
		return []byte(trimmed), nil

	default:
		return nil, fmt.Errorf("%w: %q", errUnlocatedSecret, reference)
	}
}

// resolveAll reads every secret a destination signs with. One that cannot be
// read fails the whole attempt: delivering unsigned to a receiver that checks
// signatures is the outage this feature exists to prevent, and delivering
// unsigned to one that does not is a silent downgrade nobody would notice.
func resolveAll(references []string) ([][]byte, error) {
	secrets := make([][]byte, 0, len(references))
	for _, reference := range references {
		secret, err := resolve(reference)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}
