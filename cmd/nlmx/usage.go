package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tmc/nlm/internal/batchexecute"
	"github.com/tmc/nlm/internal/notebooklm/rpc"
	"github.com/tmc/nlm/nlmauth"
)

const (
	rpcGetUsage        = "EylDcb"
	rpcGetEntitlement  = "ozz5Z"
	entitlementProduct = 627
)

type usageWindow struct {
	Window           string   `json:"window"`
	PercentUsed      *float64 `json:"percent_used,omitempty"`
	PercentRemaining *float64 `json:"percent_remaining,omitempty"`
	ResetsAt         string   `json:"resets_at,omitempty"`
}

type accountUsage struct {
	Account string        `json:"account"`
	Windows []usageWindow `json:"windows,omitempty"`
	Tier    string        `json:"tier,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// usageHeader matches the single header parameter expected by EylDcb.
func usageHeader() []interface{} {
	tail := []interface{}{1}
	for range 9 {
		tail = append(tail, nil)
	}
	tail = append(tail, []interface{}{1, 3})
	return []interface{}{2, nil, []interface{}{1}, tail}
}

func entitlementArgs() []interface{} {
	entry := []interface{}{
		[]interface{}{nil, "1", entitlementProduct},
		[]interface{}{nil, nil, nil, nil, nil, nil, nil, nil, nil, []interface{}{nil, nil, 2}},
		1,
	}
	return []interface{}{[]interface{}{entry}}
}

func numberPtr(v interface{}) *float64 {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return &f
}

// parseUsageWindows decodes the batchexecute payload. The RPC returns an outer
// array whose second element holds the window entries.
func parseUsageWindows(raw json.RawMessage) ([]usageWindow, error) {
	var payload []interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	entries := payload
	if len(payload) >= 2 {
		if nested, ok := payload[1].([]interface{}); ok {
			if _, numeric := payload[0].(float64); numeric {
				entries = nested
			}
		}
	}
	windows := make([]usageWindow, 0, len(entries))
	for _, item := range entries {
		entry, ok := item.([]interface{})
		if !ok || len(entry) < 8 {
			continue
		}
		code, ok := entry[4].(float64)
		if !ok {
			continue
		}
		name := "unknown"
		switch int(code) {
		case 1:
			name = "rolling"
		case 2:
			name = "weekly"
		}
		reset := ""
		if holder, ok := entry[5].([]interface{}); ok && len(holder) > 0 {
			if epoch, ok := holder[0].(float64); ok {
				reset = time.Unix(int64(epoch), 0).UTC().Format(time.RFC3339)
			}
		}
		windows = append(windows, usageWindow{
			Window:           name,
			PercentUsed:      numberPtr(entry[6]),
			PercentRemaining: numberPtr(entry[7]),
			ResetsAt:         reset,
		})
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("usage payload contained no windows")
	}
	return windows, nil
}

func findTier(node interface{}) string {
	switch v := node.(type) {
	case string:
		if strings.HasPrefix(v, "NOTEBOOKLM_TIER_") {
			return v
		}
	case []interface{}:
		for _, item := range v {
			if tier := findTier(item); tier != "" {
				return tier
			}
		}
	case map[string]interface{}:
		for _, item := range v {
			if tier := findTier(item); tier != "" {
				return tier
			}
		}
	}
	return ""
}

func fetchUsage(account string, creds nlmauth.Credentials) (accountUsage, error) {
	result := accountUsage{Account: account}
	opts := []batchexecute.Option{}
	if creds.AuthUser != "" {
		opts = append(opts,
			batchexecute.WithURLParams(map[string]string{"authuser": creds.AuthUser}),
			batchexecute.WithHeaders(map[string]string{"x-goog-authuser": creds.AuthUser}),
		)
	}
	client := rpc.New(creds.AuthToken, creds.Cookies, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := client.Do(ctx, rpc.Call{ID: rpcGetUsage, Args: []interface{}{usageHeader()}})
	if err != nil {
		return result, fmt.Errorf("usage rpc: %w", err)
	}
	result.Windows, err = parseUsageWindows(raw)
	if err != nil {
		return result, err
	}

	if tierRaw, tierErr := client.Do(ctx, rpc.Call{ID: rpcGetEntitlement, Args: entitlementArgs()}); tierErr == nil {
		var parsed interface{}
		if json.Unmarshal(tierRaw, &parsed) == nil {
			result.Tier = findTier(parsed)
		}
	}
	return result, nil
}

func remaining(usage accountUsage, window string) (float64, bool) {
	for _, w := range usage.Windows {
		if w.Window != window || w.PercentRemaining == nil {
			continue
		}
		return *w.PercentRemaining, true
	}
	return 0, false
}

func quotaAllows(usage accountUsage, minRolling, minWeekly float64) bool {
	rolling, ok := remaining(usage, "rolling")
	if !ok || rolling < minRolling {
		return false
	}
	weekly, ok := remaining(usage, "weekly")
	if !ok || weekly < minWeekly {
		return false
	}
	return true
}

func envFloat(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return f
}

func cmdUsage(args []string, accounts []string, accountSet map[string]bool) int {
	asJSON := false
	target := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			asJSON = true
		case arg == "--account" || arg == "-a":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "usage: nlmx usage [--json] [--account <name>]\n")
				return 2
			}
			i++
			target = args[i]
		case strings.HasPrefix(arg, "--account="):
			target = strings.TrimPrefix(arg, "--account=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(os.Stderr, "usage: nlmx usage [--json] [--account <name>]\n")
			return 2
		default:
			target = arg
		}
	}
	if target != "" && !accountSet[target] {
		fmt.Fprintf(os.Stderr, "nlmx: unknown account %q\n", target)
		return 2
	}
	targets := accounts
	if target != "" {
		targets = []string{target}
	}

	results := make([]accountUsage, 0, len(targets))
	okCount := 0
	for _, account := range targets {
		creds, err := credentialsFor(account)
		if err != nil {
			results = append(results, accountUsage{Account: account, Error: err.Error()})
			continue
		}
		usage, err := fetchUsage(account, creds)
		if err != nil {
			usage.Error = err.Error()
		} else {
			okCount++
		}
		results = append(results, usage)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: encode usage: %v\n", err)
			return 1
		}
	} else {
		for _, r := range results {
			if r.Error != "" {
				fmt.Printf("%s\terror=%s\n", r.Account, r.Error)
				continue
			}
			parts := []string{r.Account}
			for _, w := range r.Windows {
				value := "n/a"
				if w.PercentRemaining != nil {
					value = fmt.Sprintf("%.1f%%", *w.PercentRemaining)
				}
				parts = append(parts, fmt.Sprintf("%s=%s", w.Window, value))
				if w.ResetsAt != "" {
					parts = append(parts, fmt.Sprintf("reset=%s", w.ResetsAt))
				}
			}
			if r.Tier != "" {
				parts = append(parts, "tier="+r.Tier)
			}
			fmt.Println(strings.Join(parts, "\t"))
		}
	}
	if okCount == 0 {
		return 1
	}
	return 0
}
