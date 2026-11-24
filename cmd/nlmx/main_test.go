package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func accountSetOf(accounts []string) map[string]bool {
	set := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		set[a] = true
	}
	return set
}

func TestLoadAccounts(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "preferred first then sorted remainder",
			data: `{"zeta":{"nlm":"z"},"ano":{"nlm":"ano"},"iida":{"nlm":"iida"},"saitama":{"nlm":"saitama"},"blank":{"nlm":""},"missing":{}}`,
			want: []string{"iida", "ano", "saitama", "zeta"},
		},
		{
			name: "no preferred names",
			data: `{"beta":{"nlm":"b"},"alpha":{"nlm":"a"}}`,
			want: []string{"alpha", "beta"},
		},
		{
			name: "non-string nlm is skipped",
			data: `{"iida":{"nlm":"iida"},"bad":{"nlm":7}}`,
			want: []string{"iida"},
		},
		{name: "invalid json", data: `{`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			writeFile(t, path, tc.data, 0600)
			if got := loadAccounts(path); !slices.Equal(got, tc.want) {
				t.Fatalf("loadAccounts() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadAccountsMissingFile(t *testing.T) {
	if got := loadAccounts(filepath.Join(t.TempDir(), "missing.json")); got != nil {
		t.Fatalf("loadAccounts() = %v, want nil", got)
	}
}

func TestDefaultAccount(t *testing.T) {
	tests := []struct {
		name     string
		accounts []string
		want     string
	}{
		{name: "saitama wins", accounts: []string{"iida", "ano", "saitama"}, want: "saitama"},
		{name: "first when absent", accounts: []string{"ano", "iida"}, want: "ano"},
		{name: "empty", accounts: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultAccount(tc.accounts); got != tc.want {
				t.Fatalf("defaultAccount() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyFor(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		args []string
		want string
	}{
		{name: "two args", cwd: "/repo", args: []string{"chat", "list"}, want: "d3400554b1107972"},
		{name: "spaces in arg", cwd: "/home/u", args: []string{"generate-chat", "hello world"}, want: "30f697dc0902e054"},
		{name: "no args", cwd: "/", args: nil, want: "fcbe5033ba9aba46"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyFor(tc.cwd, tc.args); got != tc.want {
				t.Fatalf("keyFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		rc     int
		output string
		want   string
	}{
		{name: "auth code", rc: 3, output: "anything", want: "auth"},
		{name: "quota code", rc: 5, output: "", want: "quota"},
		{name: "transient codes", rc: 6, output: "", want: "transient"},
		{name: "busy code", rc: 7, output: "", want: "transient"},
		{name: "usage error never rotates", rc: 2, output: "flag quota: login required", want: ""},
		{name: "unauthorized", rc: 1, output: "UNAUTHORIZED", want: "auth"},
		{name: "sign-in", rc: 1, output: "Please Sign-In again", want: "auth"},
		{name: "login required", rc: 1, output: "LOGIN REQUIRED", want: "auth"},
		{name: "expired", rc: 1, output: "session expired", want: "auth"},
		{name: "quota", rc: 1, output: "Quota exceeded", want: "quota"},
		{name: "rate limit", rc: 1, output: "rate-limited", want: "quota"},
		{name: "http 429", rc: 1, output: "HTTP 429", want: "quota"},
		{name: "resource exhausted", rc: 1, output: "resource_exhausted", want: "quota"},
		{name: "limit exceeded", rc: 1, output: "limit exceeded", want: "quota"},
		{name: "batchexecute 400", rc: 1, output: "batchexecute error: request failed: 400 Bad Request", want: "auth"},
		{name: "unclassified", rc: 1, output: "boom", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.rc, tc.output); got != tc.want {
				t.Fatalf("classify(%d, %q) = %q, want %q", tc.rc, tc.output, got, tc.want)
			}
		})
	}
}

func TestChildArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "inject web", args: []string{"generate-chat", "hello"}, want: []string{"generate-chat", "hello", "--web"}},
		{name: "existing web", args: []string{"generate-chat", "--web", "hello"}, want: []string{"generate-chat", "--web", "hello"}},
		{name: "existing web value", args: []string{"generate-chat", "--web=false"}, want: []string{"generate-chat", "--web=false"}},
		{name: "other command", args: []string{"chat", "list"}, want: []string{"chat", "list"}},
		{name: "no args", args: nil, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := childArgs(tc.args); !slices.Equal(got, tc.want) {
				t.Fatalf("childArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func testState(usage map[string]int, affinity map[string]string, rotation int) state {
	st := state{Affinity: map[string]string{}, Usage: map[string]int{}, Rotation: rotation}
	for k, v := range usage {
		st.Usage[k] = v
	}
	for k, v := range affinity {
		st.Affinity[k] = v
	}
	return st
}

func TestChoose(t *testing.T) {
	accounts := []string{"iida", "ano", "saitama"}
	set := accountSetOf(accounts)
	const cwd, root = "/work", "/repo"
	args := []string{"chat", "list"}
	key := keyFor(cwd, args)

	tests := []struct {
		name         string
		st           state
		repos        map[string]string
		def          string
		preferred    string
		want         choice
		wantRotation int
		wantAffinity string
		wantRepo     string
	}{
		{
			name:         "affinity wins",
			st:           testState(nil, map[string]string{key: "ano"}, 0),
			repos:        map[string]string{root: "iida"},
			def:          "saitama",
			want:         choice{Account: "ano", Source: "affinity", Key: key},
			wantRotation: 0,
			wantAffinity: "ano",
			wantRepo:     "iida",
		},
		{
			name:         "invalid affinity falls through to repo",
			st:           testState(nil, map[string]string{key: "ghost"}, 0),
			repos:        map[string]string{root: "iida"},
			def:          "saitama",
			want:         choice{Account: "iida", Source: "repo", Key: key},
			wantRotation: 0,
			wantAffinity: "iida",
			wantRepo:     "iida",
		},
		{
			name:         "repo mapping",
			st:           testState(nil, nil, 0),
			repos:        map[string]string{root: "iida"},
			def:          "saitama",
			want:         choice{Account: "iida", Source: "repo", Key: key},
			wantRotation: 0,
			wantAffinity: "iida",
			wantRepo:     "iida",
		},
		{
			name:         "default when usage is close",
			st:           testState(map[string]int{"iida": 0, "ano": 0, "saitama": 1}, nil, 0),
			repos:        map[string]string{},
			def:          "saitama",
			want:         choice{Account: "saitama", Source: "default", Key: key},
			wantRotation: 1,
			wantAffinity: "saitama",
			wantRepo:     "saitama",
		},
		{
			name:         "preferred override",
			st:           testState(nil, nil, 0),
			repos:        map[string]string{},
			def:          "saitama",
			preferred:    "ano",
			want:         choice{Account: "ano", Source: "default", Key: key},
			wantRotation: 1,
			wantAffinity: "ano",
			wantRepo:     "ano",
		},
		{
			name:         "least used when default is overused",
			st:           testState(map[string]int{"iida": 0, "ano": 0, "saitama": 3}, nil, 0),
			repos:        map[string]string{},
			def:          "saitama",
			want:         choice{Account: "iida", Source: "default", Key: key},
			wantRotation: 1,
			wantAffinity: "iida",
			wantRepo:     "iida",
		},
		{
			name:         "rotation breaks usage tie",
			st:           testState(map[string]int{"iida": 0, "ano": 0, "saitama": 3}, nil, 1),
			repos:        map[string]string{},
			def:          "saitama",
			want:         choice{Account: "ano", Source: "default", Key: key},
			wantRotation: 2,
			wantAffinity: "ano",
			wantRepo:     "ano",
		},
		{
			name:         "invalid default falls back to first account",
			st:           testState(nil, nil, 0),
			repos:        map[string]string{},
			def:          "",
			want:         choice{Account: "iida", Source: "default", Key: key},
			wantRotation: 1,
			wantAffinity: "iida",
			wantRepo:     "iida",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			cfg := repoConfig{Default: tc.def, Repos: tc.repos, Extra: map[string]json.RawMessage{}}
			got := choose(&st, cwd, args, &cfg, root, accounts, set, tc.preferred)
			if got != tc.want {
				t.Fatalf("choose() = %+v, want %+v", got, tc.want)
			}
			if st.Rotation != tc.wantRotation {
				t.Errorf("rotation = %d, want %d", st.Rotation, tc.wantRotation)
			}
			if st.Affinity[key] != tc.wantAffinity {
				t.Errorf("affinity[key] = %q, want %q", st.Affinity[key], tc.wantAffinity)
			}
			if cfg.Repos[root] != tc.wantRepo {
				t.Errorf("repos[root] = %q, want %q", cfg.Repos[root], tc.wantRepo)
			}
		})
	}
}

func TestLoadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "affinity.json")
	writeFile(t, path, `{"affinity":{"k":"ano"},"usage":{"ano":2},"rotation":1,"extra":true}`, 0600)
	st := loadState(path, []string{"iida", "ano"})
	if st.Affinity["k"] != "ano" || st.Usage["ano"] != 2 || st.Rotation != 1 {
		t.Fatalf("loadState() = %+v", st)
	}
	if st.Usage["iida"] != 0 {
		t.Fatalf("missing account usage = %d, want 0", st.Usage["iida"])
	}
	writeFile(t, path, `{broken`, 0600)
	if got := loadState(path, []string{"iida"}); len(got.Affinity) != 0 || len(got.Usage) == 0 || got.Rotation != 0 {
		t.Fatalf("invalid state = %+v", got)
	}
}

func TestSaveStateAndRepoConfig(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "affinity.json")
	if err := saveState(statePath, testState(map[string]int{"iida": 1}, nil, 2)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}

	cfgPath := filepath.Join(dir, "repo-accounts.json")
	writeFile(t, cfgPath, `{"default":"iida","repos":{},"custom":{"x":1}}`, 0600)
	cfg, changed, err := loadRepoConfig(cfgPath, []string{"iida", "ano"}, "iida", false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("valid config reported as changed")
	}
	if err := saveRepoConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, cfgPath); !strings.Contains(got, `"custom"`) || !strings.Contains(got, `"x": 1`) {
		t.Fatalf("unknown key lost: %s", got)
	}
	cfgInfo, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfgInfo.Mode().Perm() != 0600 {
		t.Fatalf("repo config mode = %o, want 600", cfgInfo.Mode().Perm())
	}
}

// setupHarness creates an isolated HOME with a gx registry, named identities,
// and a fake child binary selected through NLM_BIN. PATH only contains the
// fake binary directory, so neither git nor a real refresh helper is found.
func setupHarness(t *testing.T, childScript string) (home, childLog string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	binDir := filepath.Join(home, "bin")
	writeFile(t, filepath.Join(binDir, "fake-nlm"), childScript, 0700)
	childLog = filepath.Join(home, "child.log")
	t.Setenv("NLM_BIN", filepath.Join(binDir, "fake-nlm"))
	t.Setenv("NLMX_TEST_OUT", childLog)
	t.Setenv("PATH", binDir)
	t.Setenv("NLMX_PREFERRED", "")
	t.Setenv("GX", "")
	for _, env := range []string{"NLM_AUTH_TOKEN", "NLM_COOKIES", "NLM_AUTHUSER"} {
		t.Setenv(env, "")
	}
	writeFile(t, filepath.Join(home, ".config", "gx", "accounts.json"),
		`{"iida":{"nlm":"iida"},"ano":{"nlm":"ano"},"saitama":{"nlm":"saitama"}}`, 0600)
	writeFile(t, filepath.Join(home, ".nlm", "identities", "saitama.env"),
		`NLM_AUTH_TOKEN="tok-saitama"`+"\n"+`NLM_COOKIES="cookie-saitama"`+"\n"+`NLM_AUTHUSER="4"`+"\n", 0600)
	writeFile(t, filepath.Join(home, ".nlm", "identities", "iida.env"),
		`NLM_AUTH_TOKEN="tok-iida"`+"\n"+`NLM_COOKIES="cookie-iida"`+"\n"+`NLM_AUTHUSER="0"`+"\n", 0600)
	writeFile(t, filepath.Join(home, ".nlm", "identities", "ano.env"),
		`NLM_AUTH_TOKEN="tok-ano"`+"\n"+`NLM_COOKIES="cookie-ano"`+"\n"+`NLM_AUTHUSER="2"`+"\n", 0600)
	return home, childLog
}

func childLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func readState(t *testing.T, home string) state {
	t.Helper()
	var st state
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(home, ".nlm", "affinity.json"))), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func readRepoJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(home, ".config", "gx", "repo-accounts.json"))), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func memoText(t *testing.T, home string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(home, ".nlm", "logs", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no memo log: %v", err)
	}
	return readFile(t, files[0])
}

const recordChild = `#!/bin/sh
printf '%s|%s|%s|%s\n' "$NLM_AUTHUSER" "$NLM_AUTH_TOKEN" "$NLM_COOKIES" "$*" >> "$NLMX_TEST_OUT"
echo fake-output
exit 0
`

func TestIntegrationSuccessUpdatesStateAndEnv(t *testing.T) {
	home, childLog := setupHarness(t, recordChild)
	args := []string{"chat", "list"}
	if rc := run(args); rc != 0 {
		t.Fatalf("run() = %d, want 0", rc)
	}
	lines := childLines(t, childLog)
	if !slices.Equal(lines, []string{"4|tok-saitama|cookie-saitama|chat list"}) {
		t.Fatalf("child invocations = %v", lines)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	st := readState(t, home)
	key := keyFor(cwd, args)
	if st.Usage["saitama"] != 1 || st.Affinity[key] != "saitama" || st.Rotation != 1 {
		t.Fatalf("state = %+v (key %q)", st, key)
	}
	repos, ok := readRepoJSON(t, home)["repos"].(map[string]any)
	if !ok || repos[cwd] != "saitama" {
		t.Fatalf("repo config = %v", readRepoJSON(t, home))
	}
	memo := memoText(t, home)
	for _, want := range []string{"account=saitama", "source=default", "argc=2", "attempts=[saitama:rc=0:-]", "result=ok"} {
		if !strings.Contains(memo, want) {
			t.Errorf("memo missing %q: %s", want, memo)
		}
	}
	if !strings.Contains(memo, time.Now().Format("2006-01-02")) {
		t.Errorf("memo date missing: %s", memo)
	}
}

const failoverChild = `#!/bin/sh
printf '%s|%s|%s|%s\n' "$NLM_AUTHUSER" "$NLM_AUTH_TOKEN" "$NLM_COOKIES" "$*" >> "$NLMX_TEST_OUT"
if [ "$NLM_AUTHUSER" = "4" ]; then
  echo "session expired" >&2
  exit 3
fi
exit 0
`

func TestIntegrationFailoverOnAuthFailure(t *testing.T) {
	home, childLog := setupHarness(t, failoverChild)
	refreshMarker := filepath.Join(home, "refreshed")
	writeFile(t, filepath.Join(home, ".local", "bin", "nlmx-auth-refresh"),
		"#!/bin/sh\n: > \""+refreshMarker+"\"\n", 0700)
	t.Setenv("NLMX_TEST_REFRESH", refreshMarker)

	args := []string{"chat", "list"}
	if rc := run(args); rc != 0 {
		t.Fatalf("run() = %d, want 0", rc)
	}
	lines := childLines(t, childLog)
	want := []string{"4|tok-saitama|cookie-saitama|chat list", "0|tok-iida|cookie-iida|chat list"}
	if !slices.Equal(lines, want) {
		t.Fatalf("child invocations = %v, want %v", lines, want)
	}
	cwd, _ := os.Getwd()
	st := readState(t, home)
	key := keyFor(cwd, args)
	if st.Usage["iida"] != 1 || st.Usage["saitama"] != 0 || st.Affinity[key] != "iida" || st.Rotation != 1 {
		t.Fatalf("state = %+v (key %q)", st, key)
	}
	repos := readRepoJSON(t, home)["repos"].(map[string]any)
	if repos[cwd] != "saitama" {
		t.Fatalf("repo mapping = %v, want saitama", repos[cwd])
	}
	memo := memoText(t, home)
	if !strings.Contains(memo, "attempts=[saitama:rc=3:auth,iida:rc=0:-]") || !strings.Contains(memo, "result=ok") {
		t.Fatalf("memo = %s", memo)
	}
	if _, err := os.Stat(refreshMarker); !os.IsNotExist(err) {
		t.Fatalf("refresh helper ran on partial failure: %v", err)
	}
}

const refreshChild = `#!/bin/sh
printf '%s\n' "$NLM_AUTHUSER" >> "$NLMX_TEST_OUT"
if [ -f "$NLMX_TEST_REFRESH" ]; then
  exit 0
fi
exit 3
`

func TestIntegrationRefreshAfterAllAuthFailures(t *testing.T) {
	home, childLog := setupHarness(t, refreshChild)
	refreshMarker := filepath.Join(home, "refreshed")
	writeFile(t, filepath.Join(home, ".local", "bin", "nlmx-auth-refresh"),
		"#!/bin/sh\n: > \""+refreshMarker+"\"\n", 0700)
	t.Setenv("NLMX_TEST_REFRESH", refreshMarker)

	if rc := run([]string{"chat", "list"}); rc != 0 {
		t.Fatalf("run() = %d, want 0", rc)
	}
	lines := childLines(t, childLog)
	want := []string{"4", "0", "2", "4"}
	if !slices.Equal(lines, want) {
		t.Fatalf("child invocations = %v, want %v", lines, want)
	}
	if _, err := os.Stat(refreshMarker); err != nil {
		t.Fatalf("refresh helper did not run: %v", err)
	}
	memo := memoText(t, home)
	if !strings.Contains(memo, "refresh=ok") || !strings.Contains(memo, "result=ok") {
		t.Fatalf("memo = %s", memo)
	}
	if strings.Count(memo, "saitama:rc=3:auth") == 0 || !strings.Contains(memo, "saitama:rc=0:-") {
		t.Fatalf("memo lacks retry attempt: %s", memo)
	}
}

func TestIntegrationAccountShowIsReadOnly(t *testing.T) {
	home, _ := setupHarness(t, recordChild)
	writeFile(t, filepath.Join(home, ".config", "gx", "repo-accounts.json"),
		`{"default":"iida","repos":{},"extra":1}`, 0600)
	if rc := run([]string{"account", "show"}); rc != 0 {
		t.Fatalf("account show = %d, want 0", rc)
	}
	cfg := readFile(t, filepath.Join(home, ".config", "gx", "repo-accounts.json"))
	if cfg != `{"default":"iida","repos":{},"extra":1}` {
		t.Fatalf("config rewritten: %s", cfg)
	}
	if _, err := os.Stat(filepath.Join(home, ".nlm", "affinity.json")); !os.IsNotExist(err) {
		t.Fatalf("state created by read-only command: %v", err)
	}
}

func TestIntegrationGenerateChatInjectsWeb(t *testing.T) {
	_, childLog := setupHarness(t, recordChild)
	if rc := run([]string{"generate-chat", "topic"}); rc != 0 {
		t.Fatalf("run() = %d, want 0", rc)
	}
	lines := childLines(t, childLog)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "generate-chat topic --web") {
		t.Fatalf("child invocations = %v", lines)
	}
}

func TestIntegrationLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logDir := filepath.Join(home, ".nlm", "logs")
	writeFile(t, filepath.Join(logDir, "2026-01-01.md"), "- first\nnot a memo\n- second\n", 0600)
	writeFile(t, filepath.Join(logDir, "2026-01-02.md"), "- third\n", 0600)
	if rc := run([]string{"log", "2"}); rc != 0 {
		t.Fatalf("log = %d, want 0", rc)
	}
	if rc := run([]string{"log", "bad"}); rc != 2 {
		t.Fatalf("log bad = %d, want 2", rc)
	}
}

func TestChildBinarySelection(t *testing.T) {
	t.Setenv("NLM_BIN", "/custom/nlm")
	if got := childBinary(); got != "/custom/nlm" {
		t.Fatalf("childBinary() = %q", got)
	}
	t.Setenv("NLM_BIN", "")
	t.Setenv("PATH", t.TempDir())
	if got := childBinary(); got != "nlm" {
		t.Fatalf("childBinary() = %q, want nlm", got)
	}
}

func TestPreferredOverride(t *testing.T) {
	set := accountSetOf([]string{"iida", "ano"})
	t.Setenv("NLMX_PREFERRED", "ano")
	if got := preferredOverride(set); got != "ano" {
		t.Fatalf("preferredOverride() = %q", got)
	}
	t.Setenv("NLMX_PREFERRED", "ghost")
	if got := preferredOverride(set); got != "" {
		t.Fatalf("invalid preferred = %q, want empty", got)
	}
}

func ExamplekeyFor() {
	fmt.Println(keyFor("/repo", []string{"chat", "list"}))
	// Output: d3400554b1107972
}

const rc2Child = `#!/bin/sh
printf '%s\n' "$*" >> "$NLMX_TEST_OUT"
echo "usage: quota login required" >&2
exit 2
`

func TestIntegrationUsageErrorNeverFailsOver(t *testing.T) {
	home, childLog := setupHarness(t, rc2Child)
	if rc := run([]string{"chat", "list"}); rc != 2 {
		t.Fatalf("run() = %d, want 2", rc)
	}
	if lines := childLines(t, childLog); len(lines) != 1 {
		t.Fatalf("child invocations = %v, want one", lines)
	}
	memo := memoText(t, home)
	if !strings.Contains(memo, "attempts=[saitama:rc=2:-]") || !strings.Contains(memo, "result=fail") {
		t.Fatalf("memo = %s", memo)
	}
	if _, err := os.Stat(filepath.Join(home, ".nlm", "affinity.json")); err != nil {
		t.Fatalf("state not saved: %v", err)
	}
}

const recordGX = `#!/bin/sh
printf '%s\n' "$*" >> "$NLMX_TEST_OUT"
exit 0
`

func TestIntegrationListAndLogin(t *testing.T) {
	home, log := setupHarness(t, recordChild)
	gx := filepath.Join(home, "bin", "fake-gx")
	writeFile(t, gx, recordGX, 0700)
	t.Setenv("GX", gx)
	if rc := run(nil); rc != 0 {
		t.Fatalf("list = %d, want 0", rc)
	}
	if rc := run([]string{"login"}); rc != 0 {
		t.Fatalf("login = %d, want 0", rc)
	}
	want := []string{"list", "nlm login iida", "nlm login ano", "nlm login saitama"}
	if got := childLines(t, log); !slices.Equal(got, want) {
		t.Fatalf("gx invocations = %v, want %v", got, want)
	}
	memo := memoText(t, home)
	if !strings.Contains(memo, "source=list argc=0") || !strings.Contains(memo, "source=auth argc=1") {
		t.Fatalf("memo = %s", memo)
	}
	if _, err := os.Stat(filepath.Join(home, ".nlm", "affinity.json")); !os.IsNotExist(err) {
		t.Fatalf("state created by list/login: %v", err)
	}
}

func TestIntegrationAccountMutations(t *testing.T) {
	home, _ := setupHarness(t, recordChild)
	cfgPath := filepath.Join(home, ".config", "gx", "repo-accounts.json")
	writeFile(t, cfgPath, `{"default":"saitama","repos":{},"extra":true}`, 0600)
	cwd, _ := os.Getwd()
	if rc := run([]string{"account", "set", "iida"}); rc != 0 {
		t.Fatalf("set = %d, want 0", rc)
	}
	repos := readRepoJSON(t, home)["repos"].(map[string]any)
	if repos[cwd] != "iida" {
		t.Fatalf("set mapping = %v", repos[cwd])
	}
	if rc := run([]string{"account", "default", "ano"}); rc != 0 {
		t.Fatalf("default = %d, want 0", rc)
	}
	cfg := readRepoJSON(t, home)
	if cfg["default"] != "ano" {
		t.Fatalf("default = %v", cfg["default"])
	}
	if rc := run([]string{"account", "reset"}); rc != 0 {
		t.Fatalf("reset = %d, want 0", rc)
	}
	repos = readRepoJSON(t, home)["repos"].(map[string]any)
	if _, ok := repos[cwd]; ok {
		t.Fatalf("reset left mapping: %v", repos)
	}
	if rc := run([]string{"account", "set", "nope"}); rc != 2 {
		t.Fatalf("invalid set = %d, want 2", rc)
	}
	if rc := run([]string{"account", "bogus"}); rc != 2 {
		t.Fatalf("unknown action = %d, want 2", rc)
	}
	if got := readFile(t, cfgPath); !strings.Contains(got, `"extra": true`) {
		t.Fatalf("unknown key lost: %s", got)
	}
}

func TestIntegrationMissingCredentialsFailOver(t *testing.T) {
	home, childLog := setupHarness(t, recordChild)
	if err := os.Remove(filepath.Join(home, ".nlm", "identities", "saitama.env")); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"chat", "list"}); rc != 0 {
		t.Fatalf("run() = %d, want 0", rc)
	}
	want := []string{"0|tok-iida|cookie-iida|chat list"}
	if got := childLines(t, childLog); !slices.Equal(got, want) {
		t.Fatalf("child invocations = %v, want %v", got, want)
	}
	memo := memoText(t, home)
	if !strings.Contains(memo, "attempts=[saitama:rc=3:auth,iida:rc=0:-]") {
		t.Fatalf("memo = %s", memo)
	}
}
