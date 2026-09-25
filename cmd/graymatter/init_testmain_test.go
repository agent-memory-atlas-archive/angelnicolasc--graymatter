package main

import (
	"os"
	"testing"
)

// Legacy init tests exercise PATH setup. Keep every in-process test away from
// the real user PATH, including tests that run the interactive wizard.
func TestMain(m *testing.M) {
	initAddExeDirToUserPath = func() (bool, error) { return false, nil }
	os.Exit(m.Run())
}
