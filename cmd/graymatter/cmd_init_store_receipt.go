package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type initStorePlan struct {
	path string
	root *os.Root // Nearest existing ancestor for an absent store.
	rel  string
	data *os.Root // Physical store selected at preflight.
	info fs.FileInfo
}

func (p *initStorePlan) close() error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.data != nil {
		errs = append(errs, p.data.Close())
		p.data = nil
	}
	if p.root != nil {
		errs = append(errs, p.root.Close())
		p.root = nil
	}
	return errors.Join(errs...)
}

type initStoreReceipt struct {
	changed   bool
	uncertain bool
	status    string
}

var initStoreOpsFactory = defaultStoreInitOps

// This private wrapper records operations rather than inferring ownership
// from a later directory listing, which could include concurrent creations.
// The published store-only path and its output contract remain untouched.
func prepareInitStoreWithReceipt(plan *initStorePlan) (receipt initStoreReceipt, err error) {
	base := initStoreOpsFactory()
	ops := base
	ops.openFile = func(root *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
		file, err := base.openFile(root, name, flag, perm)
		if err == nil {
			receipt.changed = true
		} else if !errors.Is(err, fs.ErrExist) {
			receipt.uncertain = true
		}
		return file, err
	}
	ops.link = func(root *os.Root, oldname, newname string) error {
		err := base.link(root, oldname, newname)
		if err == nil {
			receipt.changed = true
		} else if !errors.Is(err, fs.ErrExist) {
			receipt.uncertain = true
		}
		return err
	}
	ops.remove = func(root *os.Root, name string) error {
		err := base.remove(root, name)
		if err != nil {
			receipt.uncertain = true
		}
		return err
	}
	data := plan.data
	if data != nil {
		current, statErr := os.Stat(plan.path)
		if statErr != nil || !os.SameFile(current, plan.info) {
			return receipt, errors.New("publication_conflict")
		}
	} else {
		created, uncertain, mkdirErr := initCreateStoreDirs(plan.root, plan.rel)
		receipt.changed = created
		receipt.uncertain = uncertain
		if mkdirErr != nil {
			return receipt, mkdirErr
		}
		data, err = plan.root.OpenRoot(plan.rel)
		if err != nil {
			return receipt, err
		}
		defer func() {
			if closeErr := data.Close(); closeErr != nil {
				if receipt.status == "created" {
					closeErr = errors.Join(errors.New("store prepared; close failed; marker retained"), closeErr)
				}
				err = errors.Join(err, closeErr)
			}
		}()
	}
	prepared, err := prepareStoreRootWithOps(data, plan.path, ops)
	receipt.status = prepared.status
	return receipt, err
}

func initCreateStoreDirs(root *os.Root, name string) (bool, bool, error) {
	created := false
	current := ""
	for _, part := range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := initMakeDir(root, current, 0o755); err != nil {
			// Each component was absent at preflight. Existing entries may be
			// another initializer's work or a new alias; never follow them.
			if errors.Is(err, fs.ErrExist) {
				return created, false, errors.Join(errInitPublicationConflict, err)
			}
			return created, true, err
		}
		created = true
	}
	return created, false, nil
}
