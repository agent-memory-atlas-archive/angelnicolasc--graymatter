package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// storeEvidence reports whether a legacy candidate contains a real
// preparation leaf. An inaccessible or nonregular leaf is uncertain, never
// evidence of absence; callers require an explicit --dir in that case.
func storeEvidence(dir string) (present bool, err error) {
	if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	} else if statErr != nil {
		return false, fmt.Errorf("inspect legacy store directory %s: %w", dir, statErr)
	}
	root, openErr := os.OpenRoot(dir)
	if openErr != nil {
		return false, fmt.Errorf("inspect legacy store directory %s: %w", dir, openErr)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for _, name := range []string{"gray.db", "MEMORY.md"} {
		info, statErr := root.Lstat(name)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return false, fmt.Errorf("inspect legacy store %s: %w", filepath.Join(dir, name), statErr)
		}
		if info.Mode().IsRegular() {
			present = true
			continue
		}
		return false, fmt.Errorf("legacy store entry %s is not regular (%s)", filepath.Join(dir, name), info.Mode())
	}
	return present, nil
}
