package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	bolt "go.etcd.io/bbolt"
)

const MaxImportBytes = 8 << 20
const maxRecords = 100000

type Config struct {
	Version     int          `json:"version"`
	Connections []Connection `json:"connections"`
}
type Connection struct {
	ID        string   `json:"id"`
	Provider  string   `json:"provider"`
	Kind      string   `json:"kind"`
	AccountID string   `json:"account_id"`
	Enabled   bool     `json:"enabled"`
	APIKeyEnv string   `json:"api_key_env,omitempty"`
	Command   []string `json:"command,omitempty"`
}

func DefaultStateDir() (string, error) {
	p, e := os.UserConfigDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(p, "graymatter"), nil
}
func usageDir(root string) string   { return filepath.Join(root, "usage") }
func ConfigPath(root string) string { return filepath.Join(usageDir(root), "config.json") }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported config version")
	}
	if len(c.Connections) > 32 {
		return fmt.Errorf("maximum 32 usage connections")
	}
	seen := map[string]bool{}
	for _, v := range c.Connections {
		if !safeFields(v.ID, v.Provider, v.AccountID, v.Kind) {
			return fmt.Errorf("invalid connection text")
		}
		if v.ID == "" || v.AccountID == "" || seen[v.ID] {
			return fmt.Errorf("connections require unique id and account_id")
		}
		seen[v.ID] = true
		switch v.Kind {
		case "codex-app-server":
			if v.Provider != "openai" || len(v.Command) == 0 || v.Command[0] == "" {
				return fmt.Errorf("Codex requires provider openai and explicit command")
			}
		case "openai-costs":
			if v.Provider != "openai" || !envName.MatchString(v.APIKeyEnv) {
				return fmt.Errorf("OpenAI costs require an admin key environment variable name")
			}
		case "anthropic-costs":
			if v.Provider != "anthropic" || !envName.MatchString(v.APIKeyEnv) {
				return fmt.Errorf("Anthropic costs require an admin key environment variable name")
			}
		default:
			return fmt.Errorf("unsupported connection kind %q (Claude statusline and Gemini reports use import)", v.Kind)
		}
	}
	return nil
}

func ReadConfig(root string) (Config, error) {
	c := Config{Version: Version}
	f, e := os.Open(ConfigPath(root))
	if errors.Is(e, os.ErrNotExist) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	defer f.Close()
	e = decodeJSON(f, &c, 1<<20, true)
	if e == nil {
		e = c.Validate()
	}
	return c, e
}
func SaveConfig(root string, c Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	p := ConfigPath(root)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".config-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, p)
}

func decodeJSON(r io.Reader, out any, limit int64, strict bool) error {
	data, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return e
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("usage input exceeds %d bytes", limit)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if strict {
		d.DisallowUnknownFields()
	}
	if e = d.Decode(out); e != nil {
		return fmt.Errorf("invalid usage JSON: %w", e)
	}
	var tail any
	if e = d.Decode(&tail); e != io.EOF {
		return fmt.Errorf("expected exactly one JSON object")
	}
	return nil
}
func DecodeSnapshot(r io.Reader) (Snapshot, error) {
	var s Snapshot
	e := decodeJSON(r, &s, MaxImportBytes, true)
	if e == nil {
		e = s.Normalize()
	}
	return s, e
}

func openDB(root string, write bool) (*bolt.DB, error) {
	dir := usageDir(root)
	if write {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
	}
	options := &bolt.Options{Timeout: time.Second, ReadOnly: !write}
	if !write {
		// bbolt v1.3 adds O_CREATE even for read-only opens. Keep config-only
		// state directories untouched and let a missing database stay missing.
		options.OpenFile = func(name string, _ int, _ os.FileMode) (*os.File, error) {
			return os.Open(name)
		}
	}
	return bolt.Open(filepath.Join(dir, "usage.db"), 0600, options)
}

// Import atomically merges validated records. Request events are immutable;
// replaying an event is idempotent and conflicting quantities are rejected.
func Import(ctx context.Context, root string, s Snapshot) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := s.Normalize(); e != nil {
		return e
	}
	db, e := openDB(root, true)
	if e != nil {
		return e
	}
	defer db.Close()
	return db.Update(func(tx *bolt.Tx) error {
		put := func(bucket, id string, v any, observed time.Time, immutable bool) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			b, e := tx.CreateBucketIfNotExists([]byte(bucket))
			if e != nil {
				return e
			}
			data, e := json.Marshal(v)
			if e != nil {
				return e
			}
			if len(data) > 1<<20 {
				return fmt.Errorf("usage record too large")
			}
			old := b.Get([]byte(id))
			if old != nil {
				if immutable {
					// A replay can be received at a different time. Preserve the
					// original event time while rejecting changed attribution or
					// quantities for the same request identity.
					var previous, incoming UsageEvent
					if json.Unmarshal(old, &previous) != nil || json.Unmarshal(data, &incoming) != nil {
						return fmt.Errorf("corrupt usage event")
					}
					incoming.Time = previous.Time
					canonical, _ := json.Marshal(incoming)
					if !bytes.Equal(old, canonical) {
						return fmt.Errorf("conflicting duplicate usage event")
					}
					return nil
				}
				var stamp struct {
					ObservedAt  time.Time  `json:"observed_at"`
					LastAttempt *time.Time `json:"last_attempt"`
				}
				if e = json.Unmarshal(old, &stamp); e != nil {
					return fmt.Errorf("corrupt usage record")
				}
				if stamp.LastAttempt != nil {
					stamp.ObservedAt = *stamp.LastAttempt
				}
				if stamp.ObservedAt.After(observed) {
					return nil
				}
			} else if b.Stats().KeyN >= maxRecords {
				return fmt.Errorf("usage record limit reached; choose a new state directory or export/archive history")
			}
			return b.Put([]byte(id), data)
		}
		for _, q := range s.Quotas {
			if e := put("quotas", q.ID, q, q.ObservedAt, false); e != nil {
				return e
			}
		}
		for _, c := range s.Costs {
			if e := put("costs", c.ID, c, c.ObservedAt, false); e != nil {
				return e
			}
		}
		for _, v := range s.Events {
			if e := put("events", v.ID, v, v.Time, true); e != nil {
				return e
			}
		}
		for _, v := range s.Contexts {
			if e := put("contexts", contextID(v), v, v.ObservedAt, false); e != nil {
				return e
			}
		}
		for _, v := range s.Connections {
			if v.ID == "" {
				return fmt.Errorf("connection status requires id")
			}
			at := s.GeneratedAt
			if v.LastAttempt != nil {
				at = *v.LastAttempt
			}
			if e := put("connections", v.ID, v, at, false); e != nil {
				return e
			}
		}
		return nil
	})
}

func readSnapshot(ctx context.Context, root string, now time.Time) (Snapshot, error) {
	s := Snapshot{Version: Version, GeneratedAt: now}
	db, e := openDB(root, false)
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	defer db.Close()
	var count, total int
	e = db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{"quotas", "costs", "events", "contexts", "connections"} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				continue
			}
			e := b.ForEach(func(_, v []byte) error {
				count++
				total += len(v)
				if count > maxRecords || total > 32<<20 {
					return fmt.Errorf("usage snapshot exceeds bounded read; export/archive history")
				}
				if e := ctx.Err(); e != nil {
					return e
				}
				switch name {
				case "quotas":
					var q QuotaSnapshot
					if e := json.Unmarshal(v, &q); e != nil {
						return e
					}
					q.Stale = q.Stale || now.Sub(q.ObservedAt) > 10*time.Minute || (q.ResetAt != nil && !now.Before(*q.ResetAt))
					s.Quotas = append(s.Quotas, q)
				case "costs":
					var c CostObservation
					if e := json.Unmarshal(v, &c); e != nil {
						return e
					}
					c.Stale = c.Stale || (c.EndAt.After(now.Add(-24*time.Hour)) && now.Sub(c.ObservedAt) > time.Hour)
					s.Costs = append(s.Costs, c)
				case "events":
					var event UsageEvent
					if e := json.Unmarshal(v, &event); e != nil {
						return e
					}
					s.Events = append(s.Events, event)
				case "contexts":
					var c ContextSnapshot
					if e := json.Unmarshal(v, &c); e != nil {
						return e
					}
					c.Stale = c.Stale || now.Sub(c.ObservedAt) > 10*time.Minute
					s.Contexts = append(s.Contexts, c)
				case "connections":
					var c ConnectionStatus
					if e := json.Unmarshal(v, &c); e != nil {
						return e
					}
					s.Connections = append(s.Connections, c)
				}
				return nil
			})
			if e != nil {
				return e
			}
		}
		return nil
	})
	return s, e
}

// Load performs no network or credential access unless refresh is explicitly
// requested. Even then it runs only enabled user-configured connections.
func Load(ctx context.Context, root string, refresh bool) (Snapshot, error) {
	var refreshErr error
	if refresh {
		refreshErr = Refresh(ctx, root)
	}
	readCtx := ctx
	if refresh && ctx.Err() != nil {
		var stop context.CancelFunc
		readCtx, stop = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stop()
	}
	s, e := readSnapshot(readCtx, root, time.Now().UTC())
	if e != nil {
		// Version 1 promises a complete local read, even when a remote refresh
		// failed. A canceled/corrupt partial database scan must not replace the
		// caller's previous good snapshot.
		return Snapshot{}, errors.Join(refreshErr, e)
	}
	c, e := ReadConfig(root)
	if e != nil {
		return s, e
	}
	known := map[string]bool{}
	for i := range s.Connections {
		v := &s.Connections[i]
		known[v.ID] = true
		if v.State == "error" {
			for j := range s.Quotas {
				q := &s.Quotas[j]
				if q.Provider == v.Provider && q.AccountID == v.AccountID && q.Source == v.Kind && (v.LastAttempt == nil || !q.ObservedAt.After(*v.LastAttempt)) {
					q.Stale = true
					q.Error = v.Error
				}
			}
			for j := range s.Costs {
				p := &s.Costs[j]
				if p.Provider == v.Provider && p.AccountID == v.AccountID && p.Source == v.Kind && (v.LastAttempt == nil || !p.ObservedAt.After(*v.LastAttempt)) {
					p.Stale = true
				}
			}
		}
	}
	for _, v := range c.Connections {
		if !v.Enabled {
			for i := range s.Connections {
				if s.Connections[i].ID == v.ID {
					s.Connections[i].State = "disabled"
				}
			}
		}
		for i := range s.Connections {
			old := &s.Connections[i]
			if old.ID == v.ID && (old.Provider != v.Provider || old.AccountID != v.AccountID || old.Kind != v.Kind) {
				state := "unavailable"
				if !v.Enabled {
					state = "disabled"
				}
				*old = ConnectionStatus{ID: v.ID, Provider: v.Provider, Kind: v.Kind, AccountID: v.AccountID, State: state}
			}
		}
		if !known[v.ID] {
			state := "unavailable"
			if !v.Enabled {
				state = "disabled"
			}
			s.Connections = append(s.Connections, ConnectionStatus{ID: v.ID, Provider: v.Provider, Kind: v.Kind, AccountID: v.AccountID, State: state})
		}
	}
	if refreshErr != nil {
		s.Warnings = append(s.Warnings, refreshErr.Error())
	}
	return Merge(s), refreshErr
}
