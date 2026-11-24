package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NLM_QUEUE_HOME", filepath.Join(home, "nb"))
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return c
}

func TestQueueLoadSaveRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		queue Queue
		check func(t *testing.T, raw map[string]any)
	}{
		{
			name:  "empty queue",
			queue: Queue{NextID: 1, Items: []*Item{}},
			check: func(t *testing.T, raw map[string]any) {
				if raw["next_id"] != float64(1) {
					t.Fatalf("next_id = %#v", raw["next_id"])
				}
				if items, ok := raw["items"].([]any); !ok || len(items) != 0 {
					t.Fatalf("items = %#v", raw["items"])
				}
			},
		},
		{
			name: "python schema fields",
			queue: Queue{
				NextID: 7,
				Nudged: "2026-01-01T00:00:00Z",
				Items: []*Item{{
					ID: 6, Topic: "テスト", State: stateDone, Mode: "fast",
					Questions: []string{"q1"}, Tags: []string{"auto", "tag"},
					Notebook: nil, Profile: strPtr("iida"), Parent: intPtr(2),
					Depth: 1, NoAuto: true, Added: "2026-01-01T00:00:00",
					Updated: "2026-01-02T00:00:00", Out: "/tmp/report.md",
					TaskID: "task-1", Imported: intPtr(3), ReportID: "report-1",
					Answers:     []Answer{{Q: "q1", A: "a1"}},
					Suggestions: []string{"次"}, Error: "",
					Extra: map[string]json.RawMessage{"custom": json.RawMessage(`"kept"`)},
				}},
			},
			check: func(t *testing.T, raw map[string]any) {
				items := raw["items"].([]any)
				item := items[0].(map[string]any)
				for key, want := range map[string]any{
					"id": float64(6), "topic": "テスト", "state": "done", "mode": "fast",
					"parent": float64(2), "depth": float64(1), "noauto": true,
					"profile": "iida", "task_id": "task-1", "report_id": "report-1",
					"imported": float64(3), "out": "/tmp/report.md", "custom": "kept",
				} {
					if got := item[key]; got != want {
						t.Errorf("%s = %#v, want %#v", key, got, want)
					}
				}
				if raw["nudged"] != "2026-01-01T00:00:00Z" {
					t.Fatalf("nudged = %#v", raw["nudged"])
				}
				if notebook, ok := item["notebook"]; !ok || notebook != nil {
					t.Fatalf("notebook must round-trip as null: %#v", notebook)
				}
				if questions, ok := item["questions"].([]any); !ok || len(questions) != 1 {
					t.Fatalf("questions = %#v", item["questions"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			want := tc.queue
			if err := saveQueue(c, &want); err != nil {
				t.Fatalf("saveQueue: %v", err)
			}
			info, err := os.Stat(c.Queue)
			if err != nil {
				t.Fatalf("stat queue: %v", err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("queue mode = %o, want 644", info.Mode().Perm())
			}
			if _, err := os.Stat(c.Queue + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("temporary file left behind: %v", err)
			}
			data, err := os.ReadFile(c.Queue)
			if err != nil {
				t.Fatalf("read queue: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("decode queue: %v", err)
			}
			tc.check(t, raw)
			got, err := loadQueue(c)
			if err != nil {
				t.Fatalf("loadQueue: %v", err)
			}
			if got.NextID != tc.queue.NextID || len(got.Items) != len(tc.queue.Items) {
				t.Fatalf("loaded queue = %+v", got)
			}
			if len(tc.queue.Items) > 0 {
				item := got.Items[0]
				if item.Topic != "テスト" || item.Extra["custom"] == nil ||
					item.Parent == nil || *item.Parent != 2 {
					t.Fatalf("loaded item = %+v", item)
				}
			}
		})
	}
}

func TestLoadQueueMissing(t *testing.T) {
	c := testConfig(t)
	q, err := loadQueue(c)
	if err != nil {
		t.Fatalf("loadQueue: %v", err)
	}
	if q.NextID != 1 || len(q.Items) != 0 {
		t.Fatalf("queue = %+v", q)
	}
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }
