package main

import (
	"encoding/json"
	"testing"
)

func TestParseUsageWindows(t *testing.T) {
	raw := json.RawMessage(`[1,[
		[null,null,null,null,2,[1764000000],25.0,75.0],
		[null,null,null,null,1,[1763913600],null,100.0],
		[null,null,null,null,99,[1763913600],0,100]
	],null,[]]`)
	windows, err := parseUsageWindows(raw)
	if err != nil {
		t.Fatalf("parseUsageWindows: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(windows))
	}
	if windows[0].Window != "weekly" || windows[0].PercentRemaining == nil || *windows[0].PercentRemaining != 75 {
		t.Fatalf("weekly window = %+v", windows[0])
	}
	if windows[0].ResetsAt == "" {
		t.Fatal("weekly reset missing")
	}
	if windows[1].Window != "rolling" || windows[1].PercentUsed != nil {
		t.Fatalf("rolling window = %+v", windows[1])
	}
	if windows[2].Window != "unknown" {
		t.Fatalf("unknown window = %+v", windows[2])
	}
}

func TestParseUsageWindowsRejectsEmpty(t *testing.T) {
	if _, err := parseUsageWindows(json.RawMessage(`[]`)); err == nil {
		t.Fatal("empty payload should be an error")
	}
}

func TestQuotaAllows(t *testing.T) {
	mk := func(rolling, weekly float64) accountUsage {
		r, w := rolling, weekly
		return accountUsage{Windows: []usageWindow{
			{Window: "rolling", PercentRemaining: &r},
			{Window: "weekly", PercentRemaining: &w},
		}}
	}
	tests := []struct {
		name       string
		usage      accountUsage
		minRolling float64
		minWeekly  float64
		want       bool
	}{
		{name: "above thresholds", usage: mk(40, 55), minRolling: 10, minWeekly: 10, want: true},
		{name: "rolling low", usage: mk(5, 55), minRolling: 10, minWeekly: 10, want: false},
		{name: "weekly low", usage: mk(40, 2), minRolling: 10, minWeekly: 10, want: false},
		{name: "missing window", usage: accountUsage{}, minRolling: 10, minWeekly: 10, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaAllows(tc.usage, tc.minRolling, tc.minWeekly); got != tc.want {
				t.Fatalf("quotaAllows = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestFindTier(t *testing.T) {
	payload := []interface{}{
		"other",
		[]interface{}{nil, []interface{}{"NOTEBOOKLM_TIER_PRO_CONSUMER_USER"}},
	}
	if got := findTier(payload); got != "NOTEBOOKLM_TIER_PRO_CONSUMER_USER" {
		t.Fatalf("findTier = %q", got)
	}
}

func TestUsageHeaderShape(t *testing.T) {
	header := usageHeader()
	outer, ok := header[0].(int)
	if !ok || outer != 2 {
		t.Fatalf("header[0] = %#v", header[0])
	}
	tail, ok := header[3].([]interface{})
	if !ok || len(tail) != 11 {
		t.Fatalf("header tail = %#v", header[3])
	}
}
