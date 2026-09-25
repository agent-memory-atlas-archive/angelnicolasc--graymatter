package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

type initOptions struct {
	selection        storeSelection
	projectDir       string
	selected         map[string]bool
	interactive      bool
	replaceMCP       bool
	bestEffort       bool
	skipInstructions bool
	global           bool
	hooks            bool
	kg               bool
	path             bool
	quiet            bool
	json             bool
}

var applyInitFile = (*initFilePlan).apply
var closeInitFile = (*initFilePlan).close
var initPreflightPath = preflightUserPath

type setupAction struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Client    *string `json:"client"`
	Path      *string `json:"path"`
	Selected  bool    `json:"selected"`
	Required  bool    `json:"required"`
	Changed   bool    `json:"changed"`
	Status    string  `json:"status"`
	ErrorCode *string `json:"error_code"`
	file      *initFilePlan
	store     *initStorePlan
	depends   []string
	aliasOf   string
}

type setupError struct {
	ActionID *string `json:"action_id"`
	Code     string  `json:"code"`
	Message  string  `json:"message"`
}

type setupResult struct {
	SchemaVersion    int           `json:"schema_version"`
	Mode             string        `json:"mode"`
	Phase            string        `json:"phase"`
	Status           string        `json:"status"`
	Complete         bool          `json:"complete"`
	Changed          bool          `json:"changed"`
	EffectsUncertain bool          `json:"effects_uncertain"`
	RuntimeVerified  bool          `json:"runtime_verified"`
	BestEffort       bool          `json:"best_effort"`
	Actions          []setupAction `json:"actions"`
	Errors           []setupError  `json:"errors"`
}

func newSetupResult(bestEffort bool) setupResult {
	return setupResult{SchemaVersion: 1, Mode: "setup", Phase: "validation", Status: "failed", BestEffort: bestEffort,
		Actions: []setupAction{}, Errors: []setupError{}}
}

func setupString(s string) *string { return &s }

func (r *setupResult) addError(action *setupAction, code string) {
	if action != nil {
		if action.Status != "publication_unknown" && action.Status != "applied_cleanup_failed" {
			action.Status = "failed"
		}
		action.ErrorCode = setupString(code)
		r.Errors = append(r.Errors, setupError{ActionID: setupString(action.ID), Code: code, Message: "Selected action could not be completed."})
		return
	}
	r.Errors = append(r.Errors, setupError{Code: code, Message: "Initialization request could not be completed."})
}

func initErrorCode(err error, fallback string) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "invalid_document", "invalid_root", "invalid_parent", "invalid_target", "unsupported_encoding", "unsupported_edit", "unsupported_metadata", "unsafe_leaf", "config_too_large", "publication_conflict", "publication_unknown", "initialization_busy", "stage_cleanup_uncertain":
		return err.Error()
	}
	return fallback
}

func setupSatisfied(a setupAction) bool {
	switch a.Status {
	case "created", "updated", "unchanged", "preserved":
		return true
	}
	return false
}

func (r *setupResult) finalize() {
	required, satisfied := 0, 0
	for _, a := range r.Actions {
		if a.Changed {
			r.Changed = true
		}
		if a.Required {
			required++
			if setupSatisfied(a) {
				satisfied++
			}
		}
	}
	r.Complete = required == satisfied && len(r.Errors) == 0
	switch {
	case r.Complete:
		r.Status = "complete"
	case r.Phase == "validation" || r.Phase == "preflight" || satisfied == 0:
		r.Status = "failed"
	default:
		r.Status = "partial"
	}
}

func renderSetupResult(cmd *cobra.Command, result *setupResult, quiet, jsonMode bool) error {
	result.finalize()
	if jsonMode {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return fmt.Errorf("write init JSON result: %w", err)
		}
	} else if !quiet || !result.Complete {
		for _, a := range result.Actions {
			if !a.Selected || (quiet && a.ErrorCode == nil && a.Status != "skipped_dependency" && a.Status != "publication_unknown" && a.Status != "applied_cleanup_failed") {
				continue
			}
			line := a.ID + ": " + a.Status
			if a.ErrorCode != nil {
				line += " (" + *a.ErrorCode + ")"
				if a.Path != nil {
					line += " " + strconv.Quote(*a.Path)
				}
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
				return fmt.Errorf("write init result: %w", err)
			}
		}
		for _, issue := range result.Errors {
			if issue.ActionID != nil {
				continue // Already printed with its action.
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "init: "+issue.Code); err != nil {
				return fmt.Errorf("write init result: %w", err)
			}
		}
		storeSummary := ""
		for _, a := range result.Actions {
			if a.ID == "store" && a.Path != nil {
				label := "Selected store"
				if setupSatisfied(a) {
					label = "Prepared store"
				}
				storeSummary = " " + label + ": " + strconv.Quote(*a.Path)
				break
			}
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "GrayMatter setup %s.%s\n", result.Status, storeSummary); err != nil {
			return fmt.Errorf("write init result: %w", err)
		}
		if result.Complete {
			for _, a := range result.Actions {
				if a.ID == "kg" && setupSatisfied(a) {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s takes effect after your MCP client restart.\n", daemon.KGSentinelFile); err != nil {
						return fmt.Errorf("write init result: %w", err)
					}
					break
				}
			}
		}
	}
	if result.Complete || (result.Phase == "apply" && result.Status == "partial" && result.BestEffort && !result.EffectsUncertain) {
		return nil
	}
	return errors.New("initialization incomplete")
}

func planInitStore(path string) (*initStorePlan, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		ancestor := filepath.Dir(path)
		for {
			stat, statErr := os.Stat(ancestor)
			if statErr == nil {
				if !stat.IsDir() {
					return nil, errors.New("unsafe_store_directory")
				}
				root, rel, err := initAnchor(path)
				if err != nil {
					return nil, err
				}
				return &initStorePlan{path: path, root: root, rel: rel}, nil
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return nil, statErr
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return nil, errors.New("unsafe_store_directory")
			}
			ancestor = parent
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		// Directory aliases are valid selections, including symlinks.
		if info.Mode()&os.ModeSymlink == 0 {
			return nil, errors.New("unsafe_store_directory")
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	inspection := inspectStorePreparation(root, defaultStoreInitOps())
	if inspection.marker != "" || inspection.absent {
		opened, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		return &initStorePlan{path: path, data: root, info: opened}, nil
	}
	_ = root.Close()
	return nil, inspection.err
}

func newInitAction(id, kind, client, path string) setupAction {
	a := setupAction{ID: id, Kind: kind, Selected: true, Required: true, Status: "skipped"}
	if client != "" {
		a.Client = setupString(client)
	}
	if path != "" {
		a.Path = setupString(path)
	}
	return a
}

func appendPlannedFile(result *setupResult, a setupAction, perm fs.FileMode, render func([]byte, bool) ([]byte, string, error)) error {
	p, err := planInitFile(*a.Path, perm, render)
	if err != nil {
		result.addError(&a, initErrorCode(err, "preflight_failed"))
		result.Actions = append(result.Actions, a)
		return err
	}
	a.file = p
	result.Actions = append(result.Actions, a)
	return nil
}

func setupConfigSpec(id string, selection storeSelection) (top string, entry map[string]any, jsonc bool) {
	args := mcpServeArgs(selection)
	switch id {
	case "opencode":
		argv := append([]string{"graymatter"}, args...)
		return "mcp", map[string]any{"type": "local", "command": argv, "enabled": true}, true
	case "vscodecopilot":
		return "servers", map[string]any{"command": "graymatter", "args": args}, false
	default:
		return "mcpServers", map[string]any{"command": "graymatter", "args": args}, false
	}
}

func initSameDestination(a, b *initFilePlan) bool {
	if a == nil || b == nil {
		return false
	}
	if a.info != nil && b.info != nil && os.SameFile(a.info, b.info) {
		return true
	}
	left := filepath.Clean(filepath.Join(a.root.Name(), a.rel))
	right := filepath.Clean(filepath.Join(b.root.Name(), b.rel))
	if left == right {
		return true
	}
	if runtime.GOOS != "windows" || !strings.EqualFold(left, right) {
		return false
	}
	// Windows permits per-directory case sensitivity. Never fold names under
	// a case-sensitive directory merely because the host OS is Windows.
	return !initCaseSensitiveRoot(a.root) && !initCaseSensitiveRoot(b.root)
}

func initSameDestinationPaths(left, right string) (bool, error) {
	first, firstRel, err := initAnchor(left)
	if err != nil {
		return false, err
	}
	defer first.Close()
	second, secondRel, err := initAnchor(right)
	if err != nil {
		return false, err
	}
	defer second.Close()
	firstInfo, firstErr := first.Lstat(firstRel)
	secondInfo, secondErr := second.Lstat(secondRel)
	if firstErr != nil && !errors.Is(firstErr, fs.ErrNotExist) {
		return false, firstErr
	}
	if secondErr != nil && !errors.Is(secondErr, fs.ErrNotExist) {
		return false, secondErr
	}
	return initSameDestination(&initFilePlan{root: first, rel: firstRel, info: firstInfo},
		&initFilePlan{root: second, rel: secondRel, info: secondInfo}), nil
}

func validateInitAliases(result *setupResult) error {
	for i := range result.Actions {
		a := &result.Actions[i]
		if a.file == nil {
			continue
		}
		for j := 0; j < i; j++ {
			b := &result.Actions[j]
			if !initSameDestination(a.file, b.file) {
				continue
			}
			if a.Kind == "instructions" && b.Kind == "instructions" && string(a.file.after) == string(b.file.after) {
				a.aliasOf = b.ID
				continue
			}
			return errors.New("duplicate_destination")
		}
	}
	return nil
}

func planInit(opts initOptions, result *setupResult) error {
	result.Phase = "preflight"
	store := newInitAction("store", "store", "", opts.selection.Path)
	storePlan, err := planInitStore(opts.selection.Path)
	if err != nil {
		result.addError(&store, initErrorCode(err, "store_preflight_failed"))
		result.Actions = append(result.Actions, store)
		return err
	}
	store.store = storePlan
	result.Actions = append(result.Actions, store)
	if opts.kg {
		kgPath := daemon.KGSentinelPath(opts.selection.Path)
		a := newInitAction("kg", "kg", "", kgPath)
		if err := appendPlannedFile(result, a, 0o644, func(data []byte, exists bool) ([]byte, string, error) {
			if exists {
				return data, "preserved", nil
			}
			return []byte{}, "created", nil
		}); err != nil {
			return err
		}
	}
	agents := knownAgents(opts.projectDir)
	for _, agent := range agents {
		if !opts.selected[agent.id] {
			a := newInitAction("mcp:"+agent.id, "mcp", agent.id, "")
			a.Selected, a.Required, a.Status = false, false, "skipped"
			result.Actions = append(result.Actions, a)
			continue
		}
		path, err := agent.configPath(opts.projectDir)
		if err != nil {
			return err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		a := newInitAction("mcp:"+agent.id, "mcp", agent.id, path)
		if agent.id == "codex" {
			if err := appendPlannedFile(result, a, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
				return planTOMLMCP(data, exists, opts.selection, opts.replaceMCP)
			}); err != nil {
				return err
			}
			continue
		}
		top, entry, jsonc := setupConfigSpec(agent.id, opts.selection)
		if err := appendPlannedFile(result, a, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
			return planJSONMCP(data, exists, top, entry, jsonc, opts.replaceMCP)
		}); err != nil {
			return err
		}
	}
	if !opts.skipInstructions {
		for _, name := range instructionFilesFor(agents, opts.selected) {
			path := filepath.Join(opts.projectDir, name)
			a := newInitAction("instructions:"+name, "instructions", "", path)
			for _, agent := range agents {
				if opts.selected[agent.id] && agent.instructionFile == name {
					a.depends = append(a.depends, "mcp:"+agent.id)
				}
			}
			if err := appendPlannedFile(result, a, 0o644, renderInitInstructions); err != nil {
				return err
			}
		}
	}
	if opts.global {
		paths, err := globalInstructionPaths()
		if err != nil {
			return err
		}
		for i, path := range paths {
			path, err = filepath.Abs(path)
			if err != nil {
				return err
			}
			a := newInitAction(fmt.Sprintf("global:%d", i), "instructions", "", path)
			if err := appendPlannedFile(result, a, 0o644, renderInitInstructions); err != nil {
				return err
			}
		}
	}
	if opts.hooks {
		path, err := claudeSettingsPath(scopeProject)
		if err != nil {
			return err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		globalPath, err := claudeSettingsPath(scopeGlobal)
		if err != nil {
			return err
		}
		globalPath, err = filepath.Abs(globalPath)
		if err != nil {
			return err
		}
		if same, err := initSameDestinationPaths(path, globalPath); err != nil {
			return err
		} else if same {
			return errors.New("ambiguous_hook_scope")
		}
		exe, err := resolveOwnBinary()
		if err != nil {
			return err
		}
		a := newInitAction("hooks", "hooks", "", path)
		if opts.selected["claudecode"] {
			a.depends = append(a.depends, "mcp:claudecode")
		}
		if err := appendPlannedFile(result, a, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
			return renderInitHookSettings(data, exists, exe, opts.selection)
		}); err != nil {
			return err
		}
	}
	if opts.path {
		a := newInitAction("path", "path", "", "")
		if err := initPreflightPath(); err != nil {
			result.addError(&a, "path_preflight_failed")
			result.Actions = append(result.Actions, a)
			return err
		}
		result.Actions = append(result.Actions, a)
	}
	return validateInitAliases(result)
}

func applyInit(opts initOptions, result *setupResult) {
	result.Phase = "apply"
	lookup := map[string]*setupAction{}
	for i := range result.Actions {
		lookup[result.Actions[i].ID] = &result.Actions[i]
	}
	for i := range result.Actions {
		a := &result.Actions[i]
		if !a.Required {
			continue
		}
		if a.aliasOf != "" {
			a.Status = lookup[a.aliasOf].Status
			continue
		}
		if a.Kind == "instructions" && !strings.HasPrefix(a.ID, "global:") && len(a.depends) > 0 {
			any := false
			for _, id := range a.depends {
				any = any || setupSatisfied(*lookup[id])
			}
			if !any {
				a.Status = "skipped_dependency"
				continue
			}
		}
		if a.Kind == "hooks" && len(a.depends) > 0 && !setupSatisfied(*lookup[a.depends[0]]) {
			a.Status = "skipped_dependency"
			continue
		}
		if a.Kind == "path" {
			ready := true
			for _, previous := range result.Actions {
				if previous.Required && previous.ID != "path" && !setupSatisfied(previous) {
					ready = false
				}
			}
			if !ready {
				a.Status = "skipped_dependency"
				continue
			}
			changed, err := initAddExeDirToUserPath()
			if err != nil {
				result.EffectsUncertain = true
				result.addError(a, "path_update_failed")
			} else if changed {
				a.Status = "updated"
				a.Changed = true
			} else {
				a.Status = "unchanged"
			}
			continue
		}
		if a.Kind == "store" {
			prepared, err := prepareInitStoreWithReceipt(a.store)
			a.Changed = prepared.changed
			result.EffectsUncertain = result.EffectsUncertain || prepared.uncertain
			if err != nil {
				if prepared.status == "created" {
					a.Status = "applied_cleanup_failed"
				}
				result.addError(a, "store_apply_failed")
				continue
			}
			if prepared.status == "created" {
				a.Status = "created"
			} else {
				a.Status = "unchanged"
			}
			continue
		}
		if !setupSatisfied(*lookup["store"]) {
			a.Status = "skipped_dependency"
			continue
		}
		if a.file != nil {
			out := applyInitFile(a.file)
			a.Status, a.Changed = out.status, out.changed
			result.EffectsUncertain = result.EffectsUncertain || out.uncertain
			if out.err != nil {
				result.addError(a, initErrorCode(out.err, "apply_failed"))
				if out.residue != "" {
					result.Errors[len(result.Errors)-1].Message = "Owned staging file retained for inspection: " + out.residue
				}
			}
		}
	}
}

func closeInitPlans(result *setupResult) {
	for i := range result.Actions {
		if p := result.Actions[i].file; p != nil {
			if err := closeInitFile(p); err != nil {
				result.EffectsUncertain = true
				if result.Actions[i].Status == "created" || result.Actions[i].Status == "updated" {
					result.Actions[i].Status = "applied_cleanup_failed"
				}
				result.addError(&result.Actions[i], "close_failed")
			}
		}
		if p := result.Actions[i].store; p != nil {
			if err := p.close(); err != nil {
				result.EffectsUncertain = true
				if result.Actions[i].Status == "created" || result.Actions[i].Status == "updated" {
					result.Actions[i].Status = "applied_cleanup_failed"
				}
				result.addError(&result.Actions[i], "close_failed")
			}
		}
	}
}

func runSetupInit(cmd *cobra.Command, args []string, flags initCLIFlags) error {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	result := newSetupResult(flags.bestEffort)
	finish := func() error { return renderSetupResult(cmd, &result, quiet, jsonOut) }
	if len(args) != 0 {
		result.addError(nil, "unexpected_argument")
		return finish()
	}
	cwd, err := os.Getwd()
	if err != nil {
		result.addError(nil, "working_directory_unavailable")
		return finish()
	}
	selection, err := resolveStoreSelection(dataDir, rootCmd.PersistentFlags().Changed("dir"), cwd)
	if err != nil {
		result.addError(nil, "invalid_data_dir")
		return finish()
	}
	agents := knownAgents(cwd)
	var selected map[string]bool
	pathRequested := !flags.noPath
	if flags.interactive {
		if jsonOut || cmd.Flags().Changed("only") || flags.skipClaudeCode || flags.skipCursor || flags.skipCodex || flags.skipOpencode || flags.withAntigravity {
			result.addError(nil, "incompatible_flags")
			return finish()
		}
		selected, pathRequested, err = collectInitWizardSelection(agents, flags.noPath, flags.global, flags.installHooks, flags.enableKG)
		if err != nil {
			result.addError(nil, "cancelled")
			return finish()
		}
	} else {
		var onlySet map[string]bool
		onlySet, err = parseOnlyFlag(flags.only, agents)
		if err != nil || (cmd.Flags().Changed("only") && len(onlySet) == 0) {
			result.addError(nil, "invalid_only")
			return finish()
		}
		selected = map[string]bool{}
		skipped := map[string]bool{"claudecode": flags.skipClaudeCode, "cursor": flags.skipCursor, "codex": flags.skipCodex, "opencode": flags.skipOpencode}
		for _, agent := range agents {
			if cmd.Flags().Changed("only") {
				selected[agent.id] = onlySet[agent.id]
			} else if agent.optIn {
				selected[agent.id] = agent.id == "antigravity" && flags.withAntigravity
			} else {
				selected[agent.id] = !skipped[agent.id]
			}
		}
	}
	if flags.replaceMCP {
		if (!flags.interactive && !cmd.Flags().Changed("only")) || !anyInitSelected(selected) {
			result.addError(nil, "replace_requires_selection")
			return finish()
		}
	}
	opts := initOptions{selection: selection, projectDir: cwd, selected: selected, interactive: flags.interactive,
		replaceMCP: flags.replaceMCP, bestEffort: flags.bestEffort, skipInstructions: flags.skipInstructions,
		global: flags.global, hooks: flags.installHooks, kg: flags.enableKG, path: pathRequested, quiet: quiet, json: jsonOut}
	if err := planInit(opts, &result); err != nil {
		if len(result.Errors) == 0 {
			result.addError(nil, initErrorCode(err, "preflight_failed"))
		}
		closeInitPlans(&result)
		result.Phase = "preflight"
		return finish()
	}
	applyInit(opts, &result)
	closeInitPlans(&result)
	return finish()
}

func anyInitSelected(selected map[string]bool) bool {
	for _, yes := range selected {
		if yes {
			return true
		}
	}
	return false
}
