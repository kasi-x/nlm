package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseTopic(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		text     string
		tags     []string
		mode     string
		notebook string
		noauto   bool
	}{
		{
			name: "plain", body: "新しいテーマ  ", text: "新しいテーマ", mode: "deep",
		},
		{
			name: "decorations", body: "日本のAI #ai #tech !fast @work !noauto",
			text: "日本のAI", tags: []string{"ai", "tech"}, mode: "fast", notebook: "work", noauto: true,
		},
		{
			name: "report link", body: "完了テーマ → [レポート](reports/1_001_x.md)",
			text: "完了テーマ", mode: "deep",
		},
		{
			name: "last mode wins", body: "テーマ !fast !deep",
			text: "テーマ", mode: "deep",
		},
		{
			name: "tag must start token", body: "foo#notatag #real",
			text: "foo#notatag", tags: []string{"real"}, mode: "deep",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTopic(tc.body)
			if got.Text != tc.text {
				t.Errorf("text = %q, want %q", got.Text, tc.text)
			}
			if !reflect.DeepEqual(got.Tags, tc.tags) {
				t.Errorf("tags = %#v, want %#v", got.Tags, tc.tags)
			}
			if got.Mode != tc.mode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.mode)
			}
			if (got.Notebook == nil && tc.notebook != "") || (got.Notebook != nil && *got.Notebook != tc.notebook) {
				t.Errorf("notebook = %v, want %q", got.Notebook, tc.notebook)
			}
			if got.NoAuto != tc.noauto {
				t.Errorf("noauto = %t, want %t", got.NoAuto, tc.noauto)
			}
		})
	}
}

func TestSyncTopicsAndMarkerRewrite(t *testing.T) {
	c := testConfig(t)
	if err := ensureNB(c); err != nil {
		t.Fatalf("ensureNB: %v", err)
	}
	topics := strings.Join([]string{
		"# inbox",
		"- [ ] 新しいテーマ #ai !fast",
		"  - 質問その1",
		"  - [ ] 質問その2",
		"- [~] 処理中 <!-- nlmq:9 -->",
		"- [ ] 登録済み <!-- nlmq:8 -->",
		"",
	}, "\n")
	if err := os.WriteFile(c.Topics, []byte(topics), 0o644); err != nil {
		t.Fatalf("write topics: %v", err)
	}

	added, err := syncTopics(c, nil)
	if err != nil {
		t.Fatalf("syncTopics: %v", err)
	}
	if len(added) != 1 {
		t.Fatalf("added %d items, want 1", len(added))
	}
	it := added[0]
	if it.Topic != "新しいテーマ" || it.Mode != "fast" || len(it.Tags) != 1 || it.Tags[0] != "ai" {
		t.Fatalf("item = %+v", it)
	}
	if len(it.Questions) != len(defaultQuestions)+2 || it.Questions[len(it.Questions)-2] != "質問その1" {
		t.Fatalf("questions = %#v", it.Questions)
	}
	data, err := os.ReadFile(c.Topics)
	if err != nil {
		t.Fatalf("read topics: %v", err)
	}
	if !strings.Contains(string(data), "- [~] 新しいテーマ #ai !fast <!-- nlmq:1 -->") {
		t.Fatalf("marker missing: %s", data)
	}
	again, err := syncTopics(c, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sync added %d items (err=%v)", len(again), err)
	}
	q, err := loadQueue(c)
	if err != nil || q.NextID != 2 || len(q.Items) != 1 {
		t.Fatalf("queue = %+v (err=%v)", q, err)
	}

	out := filepath.Join(c.Reports, "2026-01-01_001_x.md")
	it.State = stateDone
	it.Out = out
	if err := markTopic(c, it); err != nil {
		t.Fatalf("markTopic: %v", err)
	}
	data, err = os.ReadFile(c.Topics)
	if err != nil {
		t.Fatalf("read topics: %v", err)
	}
	if !strings.Contains(string(data), "- [x] 新しいテーマ #ai !fast → [レポート](reports/2026-01-01_001_x.md) <!-- nlmq:1 -->") {
		t.Fatalf("done marker missing: %s", data)
	}
	if err := rewriteTopic(c, it.ID, " ", ""); err != nil {
		t.Fatalf("rewriteTopic: %v", err)
	}
	data, _ = os.ReadFile(c.Topics)
	if !strings.Contains(string(data), "- [ ] 新しいテーマ #ai !fast <!-- nlmq:1 -->") {
		t.Fatalf("retry marker missing: %s", data)
	}
}

func TestAppendTopicAndSuggestions(t *testing.T) {
	c := testConfig(t)
	if err := ensureNB(c); err != nil {
		t.Fatalf("ensureNB: %v", err)
	}
	notebook := "work"
	it := &Item{ID: 4, Topic: "手動テーマ", Tags: []string{"ai"}, Mode: "fast", Notebook: &notebook}
	if err := appendTopic(c, it); err != nil {
		t.Fatalf("appendTopic: %v", err)
	}
	it.Suggestions = []string{"さらに詳しく調べるべきテーマについて"}
	if err := appendSuggestions(c, it); err != nil {
		t.Fatalf("appendSuggestions: %v", err)
	}
	data, err := os.ReadFile(c.Topics)
	if err != nil {
		t.Fatalf("read topics: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "- [~] 手動テーマ #ai !fast @work <!-- nlmq:4 -->") {
		t.Fatalf("appended topic missing: %s", text)
	}
	if !strings.Contains(text, autoSection) ||
		!strings.Contains(text, "<!-- auto:4 -->") {
		t.Fatalf("suggestion missing: %s", text)
	}
}
