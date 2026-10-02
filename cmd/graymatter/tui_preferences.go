package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UI preferences live outside the store. Each store gets its own file so two
// workbenches on different stores cannot overwrite one another's selection.
type tuiPreferences struct {
	Version      int    `json:"version"`
	Namespace    string `json:"namespace,omitempty"`
	Theme        string `json:"theme"`
	Lifecycle    string `json:"lifecycle"`
	Filter       string `json:"filter,omitempty"`
	SelectedFact string `json:"selected_fact,omitempty"`
}

func tuiPreferencesPath(dir string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	key := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(base, "graymatter", "workbench", fmt.Sprintf("%x.json", key[:12]))
}

func (m *tuiModel) restorePreferences(path, explicitTheme string) {
	if m.demo || path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	var p tuiPreferences
	if err != nil || json.Unmarshal(data, &p) != nil || p.Version != 1 {
		m.status = "Saved preferences could not be read; using defaults."
		return
	}
	m.namespace, m.factQuery, m.restoreFactID = tuiPlain(p.Namespace), tuiPlain(p.Filter), p.SelectedFact
	switch p.Lifecycle {
	case "active", "retired", "alias", "all":
		m.factMode = p.Lifecycle
	}
	if explicitTheme == "" {
		switch p.Theme {
		case "dark", "light", "terminal":
			m.setTheme(p.Theme)
		}
	}
}

func (m tuiModel) savePreferences(path string) error {
	if m.demo || path == "" {
		return nil
	}
	p := tuiPreferences{1, m.namespace, m.themeName, m.factMode, m.factQuery, m.selectedFactID()}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("save workbench preferences: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".preferences-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return fmt.Errorf("save workbench preferences: %w", err)
	}
	return nil
}
