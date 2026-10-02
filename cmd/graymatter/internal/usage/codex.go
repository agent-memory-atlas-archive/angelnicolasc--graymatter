package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"time"
)

// readCodex opens only the explicitly configured command. The protocol sends
// initialize, initialized, and account/rateLimits/read; it never starts a turn,
// requests login, consumes reset credits, or reads an auth file itself.
func readCodex(ctx context.Context, c Connection, now time.Time) (Snapshot, error) {
	s := Snapshot{Version: Version, GeneratedAt: now}
	if len(c.Command) == 0 {
		return s, fmt.Errorf("Codex app-server command not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	hideCommandWindow(cmd)
	cmd.WaitDelay = time.Second
	in, e := cmd.StdinPipe()
	if e != nil {
		return s, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return s, e
	}
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		return s, fmt.Errorf("cannot start configured Codex app-server")
	}
	defer func() {
		in.Close()
		out.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	// The configured executable can be a wrapper whose descendant inherits
	// stdout after the wrapper exits. CommandContext only kills the direct
	// process, and WaitDelay cannot unblock our synchronous Scanner in every
	// inherited-pipe case. Close only this connection's pipes on cancellation.
	stopClosing := context.AfterFunc(ctx, func() {
		_ = in.Close()
		_ = out.Close()
	})
	defer stopClosing()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(in)
	read := func(id int) (json.RawMessage, error) {
		for n := 0; n < 1000 && scanner.Scan(); n++ {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error"`
				Method string `json:"method"`
			}
			if e := json.Unmarshal(scanner.Bytes(), &m); e != nil {
				return nil, fmt.Errorf("invalid app-server protocol JSON")
			}
			if m.Method != "" && len(m.ID) > 0 {
				return nil, fmt.Errorf("app-server requested unsupported client action")
			}
			if string(m.ID) == strconv.Itoa(id) {
				if m.Error != nil {
					return nil, fmt.Errorf("app-server RPC error %d", m.Error.Code)
				}
				if len(m.Result) == 0 || string(m.Result) == "null" {
					return nil, fmt.Errorf("app-server returned no result")
				}
				return m.Result, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("app-server ended without expected response")
	}
	if e = encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "graymatter_usage", "title": "Graymatter Usage", "version": "1"}}}); e != nil {
		return s, e
	}
	if _, e = read(1); e != nil {
		return s, e
	}
	if e = encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); e != nil {
		return s, e
	}
	if e = encoder.Encode(map[string]any{"id": 2, "method": "account/rateLimits/read"}); e != nil {
		return s, e
	}
	raw, e := read(2)
	if e != nil {
		return s, e
	}
	return ParseCodexRateLimits(raw, c.AccountID, now)
}

func ParseCodexRateLimits(raw []byte, account string, now time.Time) (Snapshot, error) {
	type window struct {
		Used    *float64 `json:"usedPercent"`
		Minutes *int64   `json:"windowDurationMins"`
		Reset   *int64   `json:"resetsAt"`
	}
	type limit struct {
		ID        string  `json:"limitId"`
		Name      *string `json:"limitName"`
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var v struct {
		Legacy *limit           `json:"rateLimits"`
		Limits map[string]limit `json:"rateLimitsByLimitId"`
	}
	s := Snapshot{Version: Version, GeneratedAt: now}
	if e := json.Unmarshal(raw, &v); e != nil {
		return s, fmt.Errorf("invalid Codex rate limits response")
	}
	if len(v.Limits) == 0 && v.Legacy != nil {
		id := v.Legacy.ID
		if id == "" {
			id = "codex"
		}
		v.Limits = map[string]limit{id: *v.Legacy}
	}
	if len(v.Limits) == 0 {
		return s, fmt.Errorf("Codex account returned no subscription quota")
	}
	keys := []string{}
	for k := range v.Limits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := v.Limits[k]
		for _, n := range []string{"primary", "secondary"} {
			w := b.Primary
			if n == "secondary" {
				w = b.Secondary
			}
			q := QuotaSnapshot{Provider: "openai", AccountID: account, Source: "codex-app-server", Label: k, Window: n, ObservedAt: now}
			if w != nil {
				q.UsedPercent = w.Used
				q.WindowMinutes = w.Minutes
				if w.Reset != nil {
					t := time.Unix(*w.Reset, 0).UTC()
					q.ResetAt = &t
				}
			}
			s.Quotas = append(s.Quotas, q)
		}
	}
	return s, s.Normalize()
}
