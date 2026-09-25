//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDoctorD01_LegacyFIFOIsUntouched(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, ".doctor_probe")
	if err := syscall.Mkfifo(probe, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorTree(t, dir)
	if c := checkDataDir(dir); c.Status != "ok" {
		t.Fatalf("data dir: %+v", c)
	}
	if c := checkStore(dir); c.Status != "info" {
		t.Fatalf("store: %+v", c)
	}
	assertDoctorTreeUnchanged(t, dir, before)
	if info, err := os.Lstat(probe); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO removed or replaced: info=%v err=%v", info, err)
	}
}
