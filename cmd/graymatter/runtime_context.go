package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type runtimeTransport uint8

const (
	runtimeHook runtimeTransport = iota
	runtimeMCPStdio
	runtimeMCPHTTP
)

type runtimeContextInput struct {
	configuredDir           string
	dirChanged              bool
	capturedCWD             string
	cwdErr                  error
	claudeProjectDir        string
	claudeProjectDirPresent bool
	payloadCWD              string
	transport               runtimeTransport
}

type runtimeContext struct {
	projectRoot       string
	projectRootSource string
	storeDir          string
	storeDirSource    string
	agentID           string
}

// resolveRuntimeContext chooses the project and store once per invocation.
// The caller supplies its captured cwd and environment snapshot so later
// phases never obtain a different route after daemon startup or a cwd change.
func resolveRuntimeContext(in runtimeContextInput) (runtimeContext, error) {
	var out runtimeContext
	selectionCWD := in.capturedCWD
	if in.cwdErr != nil {
		selectionCWD = ""
	}
	if in.transport == runtimeMCPHTTP {
		selection, err := resolveStoreSelection(in.configuredDir, in.dirChanged, selectionCWD)
		if err != nil {
			return out, err
		}
		if err := validateRuntimeStoreDirectory(selection.Path); err != nil {
			return out, err
		}
		out.storeDir = selection.Path
		out.storeDirSource = "service_cwd"
		if filepath.IsAbs(in.configuredDir) {
			out.storeDirSource = "explicit_absolute"
		}
		return out, nil
	}

	var envRoot, payloadRoot string
	if in.claudeProjectDirPresent {
		var err error
		envRoot, err = validateRuntimeRoot("CLAUDE_PROJECT_DIR", in.claudeProjectDir)
		if err != nil {
			return out, err
		}
	}
	if in.transport == runtimeHook && in.payloadCWD != "" {
		var err error
		payloadRoot, err = validateRuntimeRoot("hook payload cwd", in.payloadCWD)
		if err != nil {
			return out, err
		}
	}
	if envRoot != "" && payloadRoot != "" {
		within, err := runtimeDirectoryWithin(envRoot, payloadRoot)
		if err != nil {
			return out, fmt.Errorf("compare Claude project and hook payload roots: %w", err)
		}
		if !within {
			return out, fmt.Errorf("hook payload cwd is outside CLAUDE_PROJECT_DIR; select one project root before using memory")
		}
	}
	switch {
	case envRoot != "":
		out.projectRoot, out.projectRootSource = envRoot, "claude_project_dir"
	case payloadRoot != "":
		out.projectRoot, out.projectRootSource = payloadRoot, "hook_payload_cwd"
	default:
		if in.cwdErr != nil {
			return out, fmt.Errorf("resolve process cwd: %w", in.cwdErr)
		}
		var err error
		out.projectRoot, err = validateRuntimeRoot("process cwd", in.capturedCWD)
		if err != nil {
			return out, err
		}
		out.projectRootSource = "process_cwd"
	}
	out.agentID = deriveAgentID(out.projectRoot)
	if in.dirChanged {
		selection, err := resolveStoreSelection(in.configuredDir, true, selectionCWD)
		if err != nil {
			return runtimeContext{}, err
		}
		if err := validateRuntimeStoreDirectory(selection.Path); err != nil {
			return runtimeContext{}, err
		}
		out.storeDir = selection.Path
		out.storeDirSource = "explicit"
		return out, nil
	}

	selection, err := resolveStoreSelection(filepath.Join(out.projectRoot, ".graymatter"), false, selectionCWD)
	if err != nil {
		return runtimeContext{}, err
	}
	out.storeDir = selection.Path
	out.storeDirSource = "project_root"
	if err := validateRuntimeStoreDirectory(out.storeDir); err != nil {
		return runtimeContext{}, err
	}
	if in.cwdErr != nil || in.capturedCWD == "" {
		return out, nil // no process cwd exists from which to infer a legacy candidate
	}
	legacy := filepath.Join(in.capturedCWD, ".graymatter")
	same, err := runtimeSameDirectory(out.storeDir, legacy)
	if err != nil {
		return runtimeContext{}, fmt.Errorf("compare project and legacy stores: %w", err)
	}
	if same {
		return out, nil
	}
	present, evidenceErr := storeEvidence(legacy)
	if evidenceErr != nil {
		return runtimeContext{}, fmt.Errorf("legacy store at %s cannot be inspected safely; pass --dir explicitly: %w", legacy, evidenceErr)
	}
	if present {
		return runtimeContext{}, fmt.Errorf("project store %s differs from existing legacy store %s; pass --dir explicitly to choose one", out.storeDir, legacy)
	}
	return out, nil
}

func validateRuntimeRoot(source, value string) (string, error) {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be a non-empty absolute directory", source)
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", fmt.Errorf("inspect %s %s: %w", source, value, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %s is not a directory", source, value)
	}
	return filepath.Clean(value), nil
}

// runtimeDirectoryWithin compares filesystem identity along the resolved
// child's ancestor chain. Text prefixes and case folding are insufficient on
// case-sensitive directories and when a logical child is a symlink escape.
func runtimeDirectoryWithin(parent, child string) (bool, error) {
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	physicalChild, err := filepath.EvalSymlinks(child)
	if err != nil {
		return false, err
	}
	for current := physicalChild; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if os.SameFile(parentInfo, info) {
			return true, nil
		}
		if next := filepath.Dir(current); next == current {
			return false, nil
		}
	}
}

func runtimeSameDirectory(a, b string) (bool, error) {
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	if aErr != nil && !errors.Is(aErr, fs.ErrNotExist) {
		return false, aErr
	}
	if bErr != nil && !errors.Is(bErr, fs.ErrNotExist) {
		return false, bErr
	}
	if aErr != nil || bErr != nil {
		return false, nil
	}
	return os.SameFile(aInfo, bInfo), nil
}

// validateRuntimeStoreDirectory allows a missing tail and real directory
// aliases, but refuses a dangling/cyclic alias or a non-directory component.
// It only observes; publication/opening must still handle later changes.
func validateRuntimeStoreDirectory(dir string) error {
	for current := dir; ; current = filepath.Dir(current) {
		_, err := os.Lstat(current)
		if err == nil {
			info, statErr := os.Stat(current)
			if statErr != nil {
				return fmt.Errorf("resolve data directory ancestor %s: %w", current, statErr)
			}
			if !info.IsDir() {
				return fmt.Errorf("data directory ancestor %s is not a directory", current)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect data directory ancestor %s: %w", current, err)
		}
		if parent := filepath.Dir(current); parent == current {
			return fmt.Errorf("no existing ancestor for data directory %s", dir)
		}
	}
}
