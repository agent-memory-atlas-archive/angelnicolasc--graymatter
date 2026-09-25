package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

var errRuntimeStoreUnprepared = errors.New("store is not prepared")

// runtimeStoreInspection describes only the on-disk evidence needed before a
// hook or MCP server opens a store. It does not validate database health.
type runtimeStoreInspection struct {
	preparedBy string
}

// inspectRuntimeStore never creates directories, opens database leaves, or
// follows a database leaf alias. The data directory itself may be an alias to
// a real directory selected by the caller.
func inspectRuntimeStore(dir string, requirePrepared bool) (runtimeStoreInspection, error) {
	return inspectRuntimeStoreWithLstat(dir, requirePrepared, (*os.Root).Lstat)
}

func inspectRuntimeStoreWithLstat(dir string, requirePrepared bool, lstat func(*os.Root, string) (fs.FileInfo, error)) (result runtimeStoreInspection, err error) {
	root, openErr := os.OpenRoot(dir)
	if errors.Is(openErr, fs.ErrNotExist) {
		if requirePrepared {
			return result, fmt.Errorf("%w: %s has no gray.db or MEMORY.md; run graymatter init --store-only --dir %q", errRuntimeStoreUnprepared, dir, dir)
		}
		return result, nil
	}
	if openErr != nil {
		return result, fmt.Errorf("inspect data directory %s: %w", dir, openErr)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close data directory %s: %w", dir, closeErr))
		}
	}()

	db, dbErr := inspectRuntimeLeaf(root, "gray.db", lstat)
	if dbErr != nil {
		return result, fmt.Errorf("inspect database entry in %s: %w", dir, dbErr)
	}
	if db {
		return runtimeStoreInspection{preparedBy: "gray.db"}, nil
	}
	if !requirePrepared {
		return result, nil
	}
	marker, markerErr := inspectRuntimeLeaf(root, "MEMORY.md", lstat)
	if markerErr != nil {
		return result, fmt.Errorf("inspect initialization marker in %s: %w", dir, markerErr)
	}
	if marker {
		return runtimeStoreInspection{preparedBy: "MEMORY.md"}, nil
	}
	return result, fmt.Errorf("%w: %s has no gray.db or MEMORY.md; run graymatter init --store-only --dir %q", errRuntimeStoreUnprepared, dir, dir)
}

func inspectRuntimeLeaf(root *os.Root, name string, lstat func(*os.Root, string) (fs.FileInfo, error)) (bool, error) {
	info, err := lstat(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file (%s); select a real store with --dir or correct this entry", name, info.Mode())
	}
	return true, nil
}
