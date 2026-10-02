package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var providerNumber = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]{1,2})?$`)

func Refresh(ctx context.Context, root string) error {
	return refreshWith(ctx, root, fetchConnection)
}

func fetchConnection(ctx context.Context, c Connection, now time.Time) (Snapshot, error) {
	switch c.Kind {
	case "codex-app-server":
		return readCodex(ctx, c, now)
	case "openai-costs", "anthropic-costs":
		key := os.Getenv(c.APIKeyEnv)
		if key == "" {
			return Snapshot{}, fmt.Errorf("configured admin key environment variable is unset")
		}
		return fetchCosts(ctx, c, key, now, &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, "")
	default:
		return Snapshot{}, fmt.Errorf("unsupported connection")
	}
}

// refreshWith bounds concurrency so one stalled provider cannot starve other
// accounts. The injected fetch function is used only by synthetic tests.
func refreshWith(ctx context.Context, root string, fetch func(context.Context, Connection, time.Time) (Snapshot, error)) error {
	var failures []error
	config, e := ReadConfig(root)
	if e != nil {
		return e
	}
	previous, e := readSnapshot(ctx, root, time.Now().UTC())
	if e != nil {
		return e
	}
	states := map[string]ConnectionStatus{}
	for _, s := range previous.Connections {
		states[s.ID] = s
	}
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	results := make(chan error, len(config.Connections))
	for _, c := range config.Connections {
		if !c.Enabled {
			continue
		}
		now := time.Now().UTC()
		old := states[c.ID]
		if old.Provider != c.Provider || old.AccountID != c.AccountID || old.Kind != c.Kind {
			old = ConnectionStatus{}
		}
		interval := time.Minute
		if c.Kind == "codex-app-server" {
			interval = 10 * time.Second
		}
		if old.LastAttempt != nil && now.Sub(*old.LastAttempt) < interval {
			if old.State == "error" {
				failures = append(failures, fmt.Errorf("%s: %s (refresh backoff)", c.ID, old.Error))
			}
			continue
		}
		wg.Add(1)
		go func(c Connection, old ConnectionStatus) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- ctx.Err()
				return
			}
			now := time.Now().UTC()
			status := ConnectionStatus{ID: c.ID, Provider: c.Provider, AccountID: c.AccountID, Kind: c.Kind, State: "ready", LastAttempt: &now, LastSuccess: old.LastSuccess}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			s, fetchErr := fetch(callCtx, c, now)
			cancel()
			if fetchErr == nil {
				fetchErr = s.Normalize()
			}
			if fetchErr != nil {
				status.State = "error"
				status.Error = fetchErr.Error()
				s = Snapshot{Version: Version, GeneratedAt: now}
			} else {
				status.LastSuccess = &now
				// A successful source snapshot replaces its set of quota
				// windows. A removed bucket is unknown, not an old fresh limit.
				if c.Kind == "codex-app-server" {
					ids := map[string]bool{}
					for _, q := range s.Quotas {
						ids[q.ID] = true
					}
					for _, q := range previous.Quotas {
						if q.Provider == c.Provider && q.AccountID == c.AccountID && q.Source == c.Kind && !ids[q.ID] {
							q.UsedPercent = nil
							q.ResetAt = nil
							q.ObservedAt = now
							q.Stale = false
							q.Error = ""
							s.Quotas = append(s.Quotas, q)
						}
					}
				}
			}
			s.Connections = append(s.Connections, status)
			// Persist completion independently of a remote timeout; this local
			// write cannot extend a canceled network request or read credentials.
			storeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			saveErr := Import(storeCtx, root, s)
			stop()
			if joined := errors.Join(fetchErr, saveErr); joined != nil {
				results <- fmt.Errorf("%s: %w", c.ID, joined)
			}
		}(c, old)
	}
	wg.Wait()
	close(results)
	for err := range results {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// fetchCosts reads only the provider's reporting endpoints. Endpoint override
// exists for package tests; it is never exposed by config or the CLI.
func fetchCosts(ctx context.Context, c Connection, key string, now time.Time, client *http.Client, endpoint string) (Snapshot, error) {
	s := Snapshot{Version: Version, GeneratedAt: now}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	q := url.Values{}
	headers := http.Header{}
	if c.Kind == "openai-costs" {
		if endpoint == "" {
			endpoint = "https://api.openai.com/v1/organization/costs"
		}
		q.Set("start_time", strconv.FormatInt(start.Unix(), 10))
		q.Set("end_time", strconv.FormatInt(now.Unix(), 10))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		headers.Set("Authorization", "Bearer "+key)
	} else {
		if endpoint == "" {
			endpoint = "https://api.anthropic.com/v1/organizations/cost_report"
		}
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", now.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31")
		headers.Set("x-api-key", key)
		headers.Set("anthropic-version", "2023-06-01")
	}
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		var response struct {
			Data    *[]json.RawMessage `json:"data"`
			HasMore *bool              `json:"has_more"`
			Next    *string            `json:"next_page"`
		}
		if e := getJSON(ctx, client, endpoint+"?"+q.Encode(), headers, &response); e != nil {
			return s, e
		}
		if response.Data == nil || response.HasMore == nil {
			return s, fmt.Errorf("cost report data or pagination is unavailable")
		}
		for _, raw := range *response.Data {
			costs, e := parseCostBucket(raw, c, now)
			if e != nil {
				return s, e
			}
			s.Costs = append(s.Costs, costs...)
		}
		if !*response.HasMore {
			return s, s.Normalize()
		}
		if response.Next == nil || *response.Next == "" || seen[*response.Next] {
			return s, fmt.Errorf("invalid or repeated cost pagination cursor")
		}
		seen[*response.Next] = true
		q.Set("page", *response.Next)
	}
	return s, fmt.Errorf("cost report pagination limit exceeded")
}

func getJSON(ctx context.Context, client *http.Client, target string, headers http.Header, out any) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if e != nil {
			return fmt.Errorf("invalid reporting endpoint")
		}
		req.Header = headers.Clone()
		res, e := client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("cost report network request failed")
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			e = decodeJSON(res.Body, out, MaxImportBytes, false)
			res.Body.Close()
			return e
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		res.Body.Close()
		if res.StatusCode != 429 && res.StatusCode < 500 {
			return fmt.Errorf("cost report HTTP %d; verify configured admin scope", res.StatusCode)
		}
		if attempt == 2 {
			return fmt.Errorf("cost report HTTP %d after retries", res.StatusCode)
		}
		delay := time.Duration(attempt+1) * 200 * time.Millisecond
		if seconds, e := strconv.Atoi(res.Header.Get("Retry-After")); e == nil && seconds > 0 {
			if seconds > 5 {
				seconds = 5
			}
			delay = time.Duration(seconds) * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("cost report unavailable")
}

func parseCostBucket(raw []byte, c Connection, now time.Time) ([]CostObservation, error) {
	var start, end time.Time
	totals := map[string]*big.Rat{}
	add := func(currency, amount string, cents bool) error {
		currency = strings.ToUpper(currency)
		if !currencyPattern.MatchString(currency) {
			return fmt.Errorf("missing or invalid cost currency")
		}
		n, e := decimal(amount)
		if e != nil {
			return e
		}
		if cents {
			n.Quo(n, big.NewRat(100, 1))
		}
		if totals[currency] == nil {
			totals[currency] = new(big.Rat)
		}
		totals[currency].Add(totals[currency], n)
		return nil
	}
	if c.Kind == "openai-costs" {
		var b struct {
			Start   int64 `json:"start_time"`
			End     int64 `json:"end_time"`
			Results []struct {
				Amount *struct {
					Value    json.Number `json:"value"`
					Currency string      `json:"currency"`
				} `json:"amount"`
			} `json:"results"`
		}
		if e := json.Unmarshal(raw, &b); e != nil {
			return nil, fmt.Errorf("invalid OpenAI cost bucket")
		}
		start = time.Unix(b.Start, 0).UTC()
		end = time.Unix(b.End, 0).UTC()
		for _, r := range b.Results {
			if r.Amount == nil {
				return nil, fmt.Errorf("cost report amount is unavailable")
			}
			// JSON numbers may legitimately use exponent notation.
			if len(r.Amount.Value) > 100 || !providerNumber.MatchString(string(r.Amount.Value)) {
				return nil, fmt.Errorf("cost report amount is unavailable")
			}
			n, ok := new(big.Rat).SetString(string(r.Amount.Value))
			if !ok {
				return nil, fmt.Errorf("cost report amount is unavailable")
			}
			if e := add(r.Amount.Currency, decimalString(n), false); e != nil {
				return nil, e
			}
		}
	} else {
		var b struct {
			Start   time.Time `json:"starting_at"`
			End     time.Time `json:"ending_at"`
			Results []struct {
				Amount   *string `json:"amount"`
				Currency string  `json:"currency"`
			} `json:"results"`
		}
		if e := json.Unmarshal(raw, &b); e != nil {
			return nil, fmt.Errorf("invalid Anthropic cost bucket")
		}
		start, end = b.Start, b.End
		for _, r := range b.Results {
			if r.Amount == nil {
				return nil, fmt.Errorf("cost report amount is unavailable")
			}
			if e := add(r.Currency, *r.Amount, true); e != nil {
				return nil, e
			}
		}
	}
	if !end.After(start) || start.IsZero() {
		return nil, fmt.Errorf("invalid cost bucket interval")
	}
	out := []CostObservation{}
	for currency, amount := range totals {
		out = append(out, CostObservation{Provider: c.Provider, AccountID: c.AccountID, Amount: decimalString(amount), Currency: currency, Kind: "reported", Scope: "organization", Source: c.Kind, StartAt: start, EndAt: end, ObservedAt: now, Partial: c.Kind == "anthropic-costs" || end.After(now)})
	}
	return out, nil
}
