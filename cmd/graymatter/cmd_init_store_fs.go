package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const storeInitializationMarker = "# GrayMatter Memory\n\nThis directory is managed by GrayMatter.\nDo not edit gray.db manually.\n"

type storePreparationResult struct {
	status string
	marker string
}

// These narrow seams allow tests to simulate failures at the publication and
// finalization boundaries without changing production behavior or user state.
type storeInitOps struct {
	mkdirAll  func(*os.Root, string, fs.FileMode) error
	openFile  func(*os.Root, string, int, fs.FileMode) (*os.File, error)
	writeFile func(*os.File, []byte) (int, error)
	syncFile  func(*os.File) error
	closeFile func(*os.File) error
	link      func(*os.Root, string, string) error
	remove    func(*os.Root, string) error
	lstat     func(*os.Root, string) (fs.FileInfo, error)
	closeRoot func(*os.Root) error
	random    io.Reader
}

func defaultStoreInitOps() storeInitOps {
	return storeInitOps{
		mkdirAll:  (*os.Root).MkdirAll,
		openFile:  (*os.Root).OpenFile,
		writeFile: (*os.File).Write,
		syncFile:  (*os.File).Sync,
		closeFile: (*os.File).Close,
		link:      (*os.Root).Link,
		remove:    (*os.Root).Remove,
		lstat:     (*os.Root).Lstat,
		closeRoot: (*os.Root).Close,
		random:    rand.Reader,
	}
}

type storeRoots struct {
	ancestor *os.Root
	data     *os.Root
}

func (r storeRoots) close(ops storeInitOps) error {
	if r.data == r.ancestor {
		return ops.closeRoot(r.data)
	}
	return errors.Join(
		wrapStoreClose("data directory", ops.closeRoot(r.data)),
		wrapStoreClose("ancestor directory", ops.closeRoot(r.ancestor)),
	)
}

func wrapStoreClose(what string, err error) error {
	if err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}
	return nil
}

func openStoreDirectoryRoot(dataDir string, ops storeInitOps) (storeRoots, error) {
	ancestor := dataDir
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return storeRoots{}, fmt.Errorf("inspect data directory ancestor %s: %w", ancestor, err)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return storeRoots{}, fmt.Errorf("find existing data directory ancestor for %s: %w", dataDir, err)
		}
		ancestor = parent
	}

	root, err := os.OpenRoot(ancestor)
	if err != nil {
		return storeRoots{}, fmt.Errorf("open data directory ancestor %s: %w", ancestor, err)
	}
	suffix, err := filepath.Rel(ancestor, dataDir)
	if err != nil || !filepath.IsLocal(suffix) {
		if err == nil {
			err = fmt.Errorf("relative path %q is not local", suffix)
		}
		return storeRoots{}, errors.Join(fmt.Errorf("resolve data directory beneath ancestor: %w", err), wrapStoreClose("ancestor directory", ops.closeRoot(root)))
	}
	if suffix == "." {
		return storeRoots{ancestor: root, data: root}, nil
	}
	if err := ops.mkdirAll(root, suffix, 0o755); err != nil {
		return storeRoots{}, errors.Join(fmt.Errorf("create data directory %s: %w", dataDir, err), wrapStoreClose("ancestor directory", ops.closeRoot(root)))
	}
	dataRoot, err := root.OpenRoot(suffix)
	if err != nil {
		return storeRoots{}, errors.Join(fmt.Errorf("open data directory %s: %w", dataDir, err), wrapStoreClose("ancestor directory", ops.closeRoot(root)))
	}
	return storeRoots{ancestor: root, data: dataRoot}, nil
}

type storeLeafKind uint8

const (
	storeLeafAbsent storeLeafKind = iota
	storeLeafRegular
	storeLeafInvalid
	storeLeafInspectionError
)

type storeLeaf struct {
	kind storeLeafKind
	mode fs.FileMode
	err  error
}

func inspectStoreLeaf(root *os.Root, name string, ops storeInitOps) storeLeaf {
	info, err := ops.lstat(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return storeLeaf{kind: storeLeafAbsent}
	}
	if err != nil {
		return storeLeaf{kind: storeLeafInspectionError, err: err}
	}
	if info.Mode().IsRegular() {
		return storeLeaf{kind: storeLeafRegular, mode: info.Mode()}
	}
	return storeLeaf{kind: storeLeafInvalid, mode: info.Mode()}
}

type storeInspection struct {
	marker    string
	absent    bool
	uncertain bool
	err       error
}

func inspectStorePreparation(root *os.Root, ops storeInitOps) storeInspection {
	db := inspectStoreLeaf(root, "gray.db", ops)
	memory := inspectStoreLeaf(root, "MEMORY.md", ops)
	if db.kind == storeLeafRegular {
		return storeInspection{marker: "gray.db"}
	}
	if memory.kind == storeLeafRegular {
		return storeInspection{marker: "MEMORY.md"}
	}
	if db.kind == storeLeafAbsent && memory.kind == storeLeafAbsent {
		return storeInspection{absent: true}
	}
	return storeInspection{
		uncertain: db.kind == storeLeafInspectionError || memory.kind == storeLeafInspectionError,
		err: errors.Join(
			storeLeafError("gray.db", db),
			storeLeafError("MEMORY.md", memory),
		),
	}
}

func storeLeafError(name string, leaf storeLeaf) error {
	switch leaf.kind {
	case storeLeafInvalid:
		return fmt.Errorf("%s is not a regular file (%s); select the real data directory with --dir if this is an alias", name, leaf.mode)
	case storeLeafInspectionError:
		return fmt.Errorf("inspect %s: %w", name, leaf.err)
	default:
		return nil
	}
}

func prepareStoreDirectory(dataDir string) (storePreparationResult, error) {
	return prepareStoreDirectoryWithOps(dataDir, defaultStoreInitOps())
}

func prepareStoreDirectoryWithOps(dataDir string, ops storeInitOps) (result storePreparationResult, err error) {
	roots, err := openStoreDirectoryRoot(dataDir, ops)
	if err != nil {
		return result, err
	}
	committed := false
	defer func() {
		if closeErr := roots.close(ops); closeErr != nil {
			if committed {
				closeErr = fmt.Errorf("store prepared; close failed; marker retained: %w", closeErr)
			}
			err = errors.Join(err, closeErr)
		}
	}()

	initial := inspectStorePreparation(roots.data, ops)
	if initial.marker != "" {
		return storePreparationResult{status: "already_prepared", marker: initial.marker}, nil
	}
	if !initial.absent {
		return result, fmt.Errorf("data directory is not prepared and cannot be initialized: %w", initial.err)
	}

	tempName, err := writeStoreInitializationTemp(roots.data, ops)
	if err != nil {
		return result, err
	}
	cleanTemp := func(cause error) error {
		if removeErr := ops.remove(roots.data, tempName); removeErr != nil {
			return errors.Join(cause, fmt.Errorf("remove initialization temporary file %s in %s: %w", tempName, dataDir, removeErr))
		}
		return cause
	}

	beforeLink := inspectStorePreparation(roots.data, ops)
	if beforeLink.marker != "" {
		return storePreparationResult{status: "already_prepared", marker: beforeLink.marker}, cleanTemp(nil)
	}
	if !beforeLink.absent {
		return result, cleanTemp(fmt.Errorf("data directory changed before initialization publication: %w", beforeLink.err))
	}

	if linkErr := ops.link(roots.data, tempName, "MEMORY.md"); linkErr != nil {
		afterLink := inspectStorePreparation(roots.data, ops)
		if afterLink.marker != "" {
			return storePreparationResult{status: "already_prepared", marker: afterLink.marker}, cleanTemp(nil)
		}
		if afterLink.uncertain {
			return result, cleanTemp(fmt.Errorf("initialization publication could not be confirmed; marker may exist; retry is safe: %w", errors.Join(linkErr, afterLink.err)))
		}
		return result, cleanTemp(errors.Join(
			fmt.Errorf("could not publish initialization marker without replacing existing files: %w", linkErr),
			afterLink.err,
		))
	}
	committed = true
	result = storePreparationResult{status: "created", marker: "MEMORY.md"}
	if cleanupErr := cleanTemp(nil); cleanupErr != nil {
		return result, fmt.Errorf("store prepared; cleanup failed; marker retained: %w", cleanupErr)
	}
	return result, nil
}

func writeStoreInitializationTemp(root *os.Root, ops storeInitOps) (string, error) {
	var file *os.File
	var name string
	for attempt := 0; attempt < 10; attempt++ {
		var random [16]byte
		if _, err := io.ReadFull(ops.random, random[:]); err != nil {
			return "", fmt.Errorf("generate initialization temporary name: %w", err)
		}
		name = fmt.Sprintf(".init-store-%x.tmp", random)
		var err error
		file, err = ops.openFile(root, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create initialization temporary file %s: %w", name, err)
		}
		break
	}
	if file == nil {
		return "", fmt.Errorf("create initialization temporary file: exhausted 10 name collisions")
	}
	content := []byte(storeInitializationMarker)
	n, writeErr := ops.writeFile(file, content)
	if writeErr == nil && n != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		if syncErr := ops.syncFile(file); syncErr != nil {
			writeErr = fmt.Errorf("sync initialization temporary file: %w", syncErr)
		}
	}
	closeErr := ops.closeFile(file)
	if closeErr != nil {
		closeErr = fmt.Errorf("close initialization temporary file: %w", closeErr)
	}
	if writeErr != nil || closeErr != nil {
		var failure error
		if writeErr != nil {
			failure = fmt.Errorf("write initialization temporary file: %w", writeErr)
		}
		failure = errors.Join(failure, closeErr)
		if removeErr := ops.remove(root, name); removeErr != nil {
			failure = errors.Join(failure, fmt.Errorf("remove initialization temporary file %s: %w", name, removeErr))
		}
		return "", failure
	}
	return name, nil
}
