package main

import (
	"os"
	"strings"
	"testing"
)

// Legacy init tests exercise PATH setup. Keep every in-process test away from
// the real user PATH, including tests that run the interactive wizard.
func TestMain(m *testing.M) {
	// A detached child must never execute the test suite as if it were the CLI.
	// The CLI's daemon and consolidate commands can outlive go test on Windows,
	// keeping graymatter.test.exe locked and recursively starting more tests.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(2)
	}
	// In-process command tests use a direct store. Daemon E2E tests compile
	// and launch the real CLI binary with their own isolated environment.
	noDaemon = true
	initAddExeDirToUserPath = func() (bool, error) { return false, nil }
	initPreflightPath = func() error { return nil }
	os.Exit(m.Run())
}
