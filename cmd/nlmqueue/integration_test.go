package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeNLMX = `#!/bin/sh
printf '%s\n' "$*" >> "$NLM_TEST_LOG"
case "$1" in
  usage)
    printf '%s\n' '[{"account":"iida","windows":[{"window":"rolling","percent_remaining":88},{"window":"weekly","percent_remaining":77}]}]'
    exit 0 ;;
  notebook)
    printf '%s\n' 'nb-42'
    exit 0 ;;
  research)
    printf '%s\n' '{"type":"complete","report":"# 調査結果","sources":[{"title":"src"}]}'
    printf '%s\n' 'imported 1 sources'
    exit 0 ;;
  generate-report)
    printf '%s\n' '# レポート' '' '本文です'
    exit 0 ;;
  generate-chat)
    printf '%s\n' '回答です'
    exit 0 ;;
esac
exit 1
`

func setupFakeNLMX(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NLM_QUEUE_HOME", filepath.Join(home, "nb"))
	t.Setenv("NLM_QUEUE_PROFILES", "iida")
	t.Setenv("NLM_QUEUE_PROFILE", "iida")
	t.Setenv("NLM_QUEUE_AUTO", "0")
	script := filepath.Join(home, "fake-nlmx.sh")
	if err := os.WriteFile(script, []byte(fakeNLMX), 0o755); err != nil {
		t.Fatalf("write fake nlmx: %v", err)
	}
	t.Setenv("NLMX_BIN", script)
	logPath := filepath.Join(home, "nlmx.log")
	t.Setenv("NLM_TEST_LOG", logPath)
	return logPath
}

func runCommand(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := execute(args, &out, &errOut); code != 0 {
		t.Fatalf("nlm-queue %s failed (code %d): %s", strings.Join(args, " "), code, errOut.String())
	}
	return out.String()
}

func TestRunAdvancesPendingItemToDone(t *testing.T) {
	logPath := setupFakeNLMX(t)
	if out := runCommand(t, "add", "統合テーマ", "--mode", "fast", "--tags", "ai"); !strings.Contains(out, "#1 統合テーマ") {
		t.Fatalf("add output = %q", out)
	}
	runCommand(t, "run")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	q, err := loadQueue(c)
	if err != nil {
		t.Fatalf("loadQueue: %v", err)
	}
	if len(q.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(q.Items))
	}
	it := q.Items[0]
	if it.State != stateDone || it.Out == "" {
		t.Fatalf("item = %+v, want done with output", it)
	}
	if it.Imported == nil || *it.Imported != 1 {
		t.Fatalf("imported = %v, want 1", it.Imported)
	}
	if len(it.Answers) != len(defaultQuestions) {
		t.Fatalf("answers = %d, want %d", len(it.Answers), len(defaultQuestions))
	}

	itemDirPath, err := itemDir(c, it)
	if err != nil {
		t.Fatalf("itemDir: %v", err)
	}
	report, err := os.ReadFile(filepath.Join(itemDirPath, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	if !strings.Contains(string(report), "# レポート") {
		t.Fatalf("report.md = %s", report)
	}
	research, err := os.ReadFile(filepath.Join(itemDirPath, "research.md"))
	if err != nil || !strings.Contains(string(research), "# 調査結果") {
		t.Fatalf("research.md = %s (err=%v)", research, err)
	}
	output, err := os.ReadFile(it.Out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	for _, needle := range []string{"# 統合テーマ", "## レポート", "取込ソース 1 件", "nlm-queue #1", "## 深掘り Q&A"} {
		if !strings.Contains(string(output), needle) {
			t.Errorf("output missing %q: %s", needle, output)
		}
	}

	topics, err := os.ReadFile(c.Topics)
	if err != nil {
		t.Fatalf("read topics: %v", err)
	}
	if !strings.Contains(string(topics), "- [x] 統合テーマ #ai") ||
		!strings.Contains(string(topics), "→ [レポート](reports/") {
		t.Fatalf("topics marker not rewritten: %s", topics)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake nlmx log: %v", err)
	}
	for _, needle := range []string{
		"usage --json --account iida",
		"notebook create 統合テーマ",
		"research --mode fast --import nb-42 統合テーマ",
		"generate-report nb-42",
		"generate-chat --citations off nb-42",
	} {
		if !strings.Contains(string(calls), needle) {
			t.Errorf("nlmx calls missing %q: %s", needle, calls)
		}
	}

	shown := runCommand(t, "show", "1")
	if !strings.Contains(shown, `"state": "done"`) {
		t.Fatalf("show output = %s", shown)
	}
}

func TestRetryResetsFailedItemAndMarker(t *testing.T) {
	setupFakeNLMX(t)
	c := testConfig(t)
	if err := ensureNB(c); err != nil {
		t.Fatalf("ensureNB: %v", err)
	}
	q := &Queue{NextID: 2, Items: []*Item{{
		ID: 1, Topic: "失敗テーマ", State: stateFailed, Mode: "deep",
		Questions: []string{}, Tags: []string{}, Error: "boom", FailedAt: stateReporting,
	}}}
	if err := saveQueue(c, q); err != nil {
		t.Fatalf("saveQueue: %v", err)
	}
	data, err := os.ReadFile(c.Topics)
	if err != nil {
		t.Fatalf("read topics: %v", err)
	}
	lines := append(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"),
		"- [!] 失敗テーマ <!-- nlmq:1 -->")
	if err := writeTopics(c, lines); err != nil {
		t.Fatalf("writeTopics: %v", err)
	}
	out := runCommand(t, "retry", "1")
	if !strings.Contains(out, "#1 → pending") {
		t.Fatalf("retry output = %q", out)
	}
	got, err := loadQueue(c)
	if err != nil {
		t.Fatalf("loadQueue: %v", err)
	}
	if got.Items[0].State != statePending || got.Items[0].Error != "" {
		t.Fatalf("item = %+v, want pending without error", got.Items[0])
	}
	data, _ = os.ReadFile(c.Topics)
	if !strings.Contains(string(data), "- [ ] 失敗テーマ <!-- nlmq:1 -->") {
		t.Fatalf("topic marker = %s", data)
	}
}
