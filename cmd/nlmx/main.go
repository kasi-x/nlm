// Command nlmx wraps the Go nlm CLI with per-topic affinity, per-repository
// accounts and automatic failover across accounts.
package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tmc/nlm/nlmauth"
)

var preferredAccounts = []string{"iida", "ano", "saitama"}

var (
	authPattern  = regexp.MustCompile(`(?i)unauth|sign.?in|login required|expired|batchexecute[^\n]*400 bad request|400 bad request[^\n]*batchexecute`)
	quotaPattern = regexp.MustCompile(`(?i)quota|rate.?limit|429|resource.?exhausted|limit exceeded`)
)

type state struct {
	Affinity map[string]string `json:"affinity"`
	Usage    map[string]int    `json:"usage"`
	Rotation int               `json:"rotation"`
}

type repoConfig struct {
	Default string
	Repos   map[string]string
	Extra   map[string]json.RawMessage
}

func (c repoConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.Extra)+2)
	for k, v := range c.Extra {
		out[k] = v
	}
	value, err := json.Marshal(c.Default)
	if err != nil {
		return nil, err
	}
	out["default"] = value
	value, err = json.Marshal(c.Repos)
	if err != nil {
		return nil, err
	}
	out["repos"] = value
	return json.Marshal(out)
}

func (c *repoConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = repoConfig{Repos: map[string]string{}, Extra: map[string]json.RawMessage{}}
	for key, value := range raw {
		switch key {
		case "default":
			_ = json.Unmarshal(value, &c.Default)
		case "repos":
			var repos map[string]string
			if json.Unmarshal(value, &repos) == nil {
				c.Repos = repos
			}
		default:
			c.Extra[key] = value
		}
	}
	if c.Repos == nil {
		c.Repos = map[string]string{}
	}
	return nil
}

type attempt struct {
	Account string
	RC      int
	Class   string
}

type choice struct {
	Account string
	Source  string
	Key     string
}

type paths struct {
	State    string
	RepoJSON string
	LogDir   string
	Accounts string
}

func pathsFor(home string) paths {
	nlmDir := filepath.Join(home, ".nlm")
	return paths{
		State:    filepath.Join(nlmDir, "affinity.json"),
		RepoJSON: filepath.Join(home, ".config", "gx", "repo-accounts.json"),
		LogDir:   filepath.Join(nlmDir, "logs"),
		Accounts: filepath.Join(home, ".config", "gx", "accounts.json"),
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nlmx: home directory: %v\n", err)
		return 1
	}
	p := pathsFor(home)
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	accounts := loadAccounts(p.Accounts)
	accountSet := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		accountSet[a] = true
	}

	if len(args) > 0 {
		switch args[0] {
		case "account":
			return cmdAccount(args[1:], p, cwd, accounts, accountSet)
		case "log":
			return cmdLog(args[1:], p.LogDir)
		case "usage":
			return cmdUsage(args[1:], accounts, accountSet)
		}
	}

	root := repoRoot(cwd)
	if len(args) == 0 {
		logMemo(p.LogDir, root, "-", "list", nil, "-", true, 0)
		return runGX("list")
	}
	if args[0] == "login" || args[0] == "auth" {
		targets := args[1:]
		if len(targets) == 0 {
			targets = accounts
		}
		attempts := make([]attempt, 0, len(targets))
		for _, acct := range targets {
			attempts = append(attempts, attempt{Account: acct, RC: runGX("nlm", "login", acct)})
		}
		logMemo(p.LogDir, root, "-", "auth", attempts, "-", true, len(args))
		return 0
	}

	def := defaultAccount(accounts)
	cfg, changed, err := loadRepoConfig(p.RepoJSON, accounts, def, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
		return 1
	}
	if changed {
		if err := saveRepoConfig(p.RepoJSON, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
			return 1
		}
	}
	st := loadState(p.State, accounts)
	preferred := preferredOverride(accountSet)
	c := choose(&st, cwd, args, &cfg, root, accounts, accountSet, preferred)
	if c.Source == "default" {
		if err := saveRepoConfig(p.RepoJSON, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
			return 1
		}
	}

	bin := childBinary()
	candidates := []string{c.Account}
	for _, a := range accounts {
		if a != c.Account {
			candidates = append(candidates, a)
		}
	}
	attempts := make([]attempt, 0, len(candidates)+1)
	refresh := "-"
	for _, cand := range candidates {
		creds, err := credentialsFor(cand)
		if err != nil || creds.AuthToken == "" || creds.Cookies == "" {
			// An account without a stored session is an auth failure: do not
			// invoke the child with another account's credentials.
			attempts = append(attempts, attempt{Account: cand, RC: 3, Class: "auth"})
			continue
		}
		if os.Getenv("NLMX_QUOTA_GATE") == "1" {
			usage, err := fetchUsage(cand, creds)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nlmx: %s: quota read failed: %v\n", cand, err)
				attempts = append(attempts, attempt{Account: cand, RC: 3, Class: "auth"})
				continue
			}
			minRolling := envFloat("NLMX_QUOTA_MIN_ROLLING", 10)
			minWeekly := envFloat("NLMX_QUOTA_MIN_WEEKLY", 10)
			if !quotaAllows(usage, minRolling, minWeekly) {
				fmt.Fprintf(os.Stderr, "nlmx: %s: quota below threshold (rolling>=%.0f%%, weekly>=%.0f%%)\n",
					cand, minRolling, minWeekly)
				attempts = append(attempts, attempt{Account: cand, RC: 5, Class: "quota"})
				continue
			}
		}
		rc, output := runChild(bin, args, creds, cand)
		cl := ""
		if rc != 0 {
			cl = classify(rc, output)
		}
		attempts = append(attempts, attempt{Account: cand, RC: rc, Class: cl})
		switch {
		case rc == 0:
			return finishSuccess(p, st, c, root, cand, attempts, "-", len(args))
		case cl == "":
			fmt.Fprintf(os.Stderr, "nlmx: %s: unclassified error rc=%d\n", cand, rc)
			saveStateQuiet(p.State, st)
			logMemo(p.LogDir, root, c.Account, c.Source, attempts, "-", false, len(args))
			return rc
		}
	}

	if len(attempts) > 0 {
		allAuth := true
		for _, a := range attempts {
			if a.Class != "auth" {
				allAuth = false
				break
			}
		}
		if allAuth {
			refresh = refreshAuth(home)
			creds, err := credentialsFor(c.Account)
			if err != nil || creds.AuthToken == "" || creds.Cookies == "" {
				attempts = append(attempts, attempt{Account: c.Account, RC: 3, Class: "auth"})
			} else {
				rc, output := runChild(bin, args, creds, c.Account)
				cl := ""
				if rc != 0 {
					cl = classify(rc, output)
				}
				attempts = append(attempts, attempt{Account: c.Account, RC: rc, Class: cl})
				if rc == 0 {
					return finishSuccess(p, st, c, root, c.Account, attempts, refresh, len(args))
				}
			}
		}
	}
	saveStateQuiet(p.State, st)
	logMemo(p.LogDir, root, c.Account, c.Source, attempts, refresh, false, len(args))
	return 1
}

func finishSuccess(p paths, st state, c choice, root, acct string, attempts []attempt, refresh string, argc int) int {
	st.Usage[acct] = st.Usage[acct] + 1 // normalized by choose
	st.Affinity[c.Key] = acct
	if err := saveState(p.State, st); err != nil {
		fmt.Fprintf(os.Stderr, "nlmx: save state: %v\n", err)
		return 1
	}
	logMemo(p.LogDir, root, c.Account, c.Source, attempts, refresh, true, argc)
	return 0
}

// loadAccounts returns canonical gx account names that have an nlm identity,
// in stable order: the preferred names first, then the rest sorted.
func loadAccounts(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var registry map[string]json.RawMessage
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil
	}
	available := map[string]bool{}
	for name, raw := range registry {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		var nlm string
		if json.Unmarshal(fields["nlm"], &nlm) != nil || strings.TrimSpace(nlm) == "" {
			continue
		}
		available[name] = true
	}
	names := make([]string, 0, len(available))
	for _, preferred := range preferredAccounts {
		if available[preferred] {
			names = append(names, preferred)
			delete(available, preferred)
		}
	}
	rest := make([]string, 0, len(available))
	for name := range available {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	return append(names, rest...)
}

func defaultAccount(accounts []string) string {
	for _, a := range accounts {
		if a == "saitama" {
			return a
		}
	}
	if len(accounts) > 0 {
		return accounts[0]
	}
	return ""
}

func preferredOverride(accountSet map[string]bool) string {
	name := os.Getenv("NLMX_PREFERRED")
	if accountSet[name] {
		return name
	}
	return ""
}

func newState(accounts []string) state {
	st := state{Affinity: map[string]string{}, Usage: map[string]int{}, Rotation: 0}
	for _, a := range accounts {
		st.Usage[a] = 0
	}
	return st
}

func loadState(path string, accounts []string) state {
	st := newState(accounts)
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return newState(accounts)
	}
	if st.Affinity == nil {
		st.Affinity = map[string]string{}
	}
	if st.Usage == nil {
		st.Usage = map[string]int{}
	}
	for _, a := range accounts {
		if _, ok := st.Usage[a]; !ok {
			st.Usage[a] = 0
		}
	}
	return st
}

func saveState(path string, st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func saveStateQuiet(path string, st state) {
	if err := saveState(path, st); err != nil {
		fmt.Fprintf(os.Stderr, "nlmx: save state: %v\n", err)
	}
}

func loadRepoConfig(path string, accounts []string, def string, create bool) (repoConfig, bool, error) {
	cfg := repoConfig{Default: def, Repos: map[string]string{}, Extra: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	changed := err != nil
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			cfg = repoConfig{Repos: map[string]string{}, Extra: map[string]json.RawMessage{}}
			changed = true
		}
	}
	set := map[string]bool{}
	for _, a := range accounts {
		set[a] = true
	}
	if !set[cfg.Default] {
		cfg.Default = def
		changed = true
	}
	if cfg.Repos == nil {
		cfg.Repos = map[string]string{}
		changed = true
	}
	if create && changed {
		return cfg, changed, saveRepoConfig(path, cfg)
	}
	return cfg, changed, nil
}

func saveRepoConfig(path string, cfg repoConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func cloneState(st state) state {
	out := state{Rotation: st.Rotation, Affinity: map[string]string{}, Usage: map[string]int{}}
	for k, v := range st.Affinity {
		out.Affinity[k] = v
	}
	for k, v := range st.Usage {
		out.Usage[k] = v
	}
	return out
}

func cloneRepoConfig(cfg repoConfig) repoConfig {
	out := repoConfig{Default: cfg.Default, Repos: map[string]string{}, Extra: map[string]json.RawMessage{}}
	for k, v := range cfg.Repos {
		out.Repos[k] = v
	}
	for k, v := range cfg.Extra {
		out.Extra[k] = v
	}
	return out
}

func repoRoot(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			if abs, err := filepath.Abs(root); err == nil {
				return abs
			}
			return root
		}
	}
	return cwd
}

func keyFor(cwd string, args []string) string {
	sum := sha1.Sum([]byte(cwd + " " + strings.Join(args, " ")))
	return hex.EncodeToString(sum[:])[:16]
}

func mod(i, n int) int {
	if n <= 0 {
		return 0
	}
	return ((i % n) + n) % n
}

func choose(st *state, cwd string, args []string, cfg *repoConfig, root string, accounts []string, accountSet map[string]bool, preferred string) choice {
	if accountSet == nil {
		accountSet = map[string]bool{}
		for _, a := range accounts {
			accountSet[a] = true
		}
	}
	k := keyFor(cwd, args)
	if st.Affinity == nil {
		st.Affinity = map[string]string{}
	}
	if st.Usage == nil {
		st.Usage = map[string]int{}
	}
	if cfg.Repos == nil {
		cfg.Repos = map[string]string{}
	}
	for _, a := range accounts {
		if _, ok := st.Usage[a]; !ok {
			st.Usage[a] = 0
		}
	}
	if acct := st.Affinity[k]; accountSet[acct] {
		return choice{Account: acct, Source: "affinity", Key: k}
	}
	if acct := cfg.Repos[root]; accountSet[acct] {
		st.Affinity[k] = acct
		return choice{Account: acct, Source: "repo", Key: k}
	}
	if len(accounts) == 0 {
		return choice{}
	}
	rotation := mod(st.Rotation, len(accounts))
	def := cfg.Default
	if accountSet[preferred] {
		def = preferred
	}
	if !accountSet[def] {
		def = accounts[0]
	}
	others := make([]string, 0, len(accounts)-1)
	for _, a := range accounts {
		if a != def {
			others = append(others, a)
		}
	}
	acct := ""
	if len(others) > 0 {
		minUsage := st.Usage[others[0]]
		for _, a := range others[1:] {
			if st.Usage[a] < minUsage {
				minUsage = st.Usage[a]
			}
		}
		if st.Usage[def] <= minUsage+1 {
			acct = def
		}
	}
	if acct == "" {
		best := -1
		bestUsage, bestDefault, bestTie := 0, 0, 0
		for i, a := range accounts {
			usage := st.Usage[a]
			isDefault := 0
			if a == def {
				isDefault = 1
			}
			tie := mod(i-rotation, len(accounts))
			if best < 0 || usage < bestUsage ||
				(usage == bestUsage && (isDefault < bestDefault ||
					(isDefault == bestDefault && tie < bestTie))) {
				best, bestUsage, bestDefault, bestTie = i, usage, isDefault, tie
			}
		}
		acct = accounts[best]
	}
	st.Rotation = (rotation + 1) % len(accounts)
	st.Affinity[k] = acct
	cfg.Repos[root] = acct
	return choice{Account: acct, Source: "default", Key: k}
}

func classify(rc int, output string) string {
	switch rc {
	case 3:
		return "auth"
	case 5:
		return "quota"
	case 6, 7:
		return "transient"
	case 2:
		return "" // CLI usage errors must never rotate accounts.
	}
	if authPattern.MatchString(output) {
		return "auth"
	}
	if quotaPattern.MatchString(output) {
		return "quota"
	}
	return ""
}

func credentialsFor(account string) (nlmauth.Credentials, error) {
	store, err := nlmauth.OpenStore()
	if err != nil {
		return nlmauth.Credentials{}, err
	}
	session, err := store.Get(account)
	if err != nil {
		return nlmauth.Credentials{}, err
	}
	return session.Credentials, nil
}

func childBinary() string {
	if bin := os.Getenv("NLM_BIN"); bin != "" {
		return bin
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "nlm")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling
		}
	}
	if bin, err := exec.LookPath("nlm-head"); err == nil {
		return bin
	}
	return "nlm"
}

func childArgs(args []string) []string {
	out := append([]string{}, args...)
	if len(out) == 0 || out[0] != "generate-chat" {
		return out
	}
	for _, arg := range out {
		if arg == "--web" || strings.HasPrefix(arg, "--web=") {
			return out
		}
	}
	return append(out, "--web")
}

func childEnv(creds nlmauth.Credentials) []string {
	replace := map[string]bool{
		"NLM_AUTH_TOKEN": true,
		"NLM_COOKIES":    true,
		"NLM_AUTHUSER":   true,
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !replace[name] {
			env = append(env, kv)
		}
	}
	return append(env,
		"NLM_AUTH_TOKEN="+creds.AuthToken,
		"NLM_COOKIES="+creds.Cookies,
		"NLM_AUTHUSER="+creds.AuthUser,
	)
}

func runChild(bin string, args []string, creds nlmauth.Credentials, account string) (int, string) {
	cmd := exec.Command(bin, childArgs(args)...)
	cmd.Env = childEnv(creds)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdout)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String() + stderr.String()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.String() + stderr.String()
	}
	fmt.Fprintf(os.Stderr, "nlmx: %s: %v\n", account, err)
	return 1, stdout.String() + stderr.String()
}

func runGX(args ...string) int {
	cmd := exec.Command(gxBinary(), args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "nlmx: gx: %v\n", err)
	return 127
}

func gxBinary() string {
	if gx := os.Getenv("GX"); gx != "" {
		return gx
	}
	return "gx"
}

func refreshAuth(home string) string {
	exe, err := exec.LookPath("nlmx-auth-refresh")
	if err != nil {
		exe = filepath.Join(home, ".local", "bin", "nlmx-auth-refresh")
	}
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() || info.Mode().Perm()&0111 == 0 {
		return "unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return "fail"
	}
	defer devNull.Close()
	cmd.Stdin = devNull
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "fail"
	}
	syncIdentities(home)
	return "ok"
}

// syncIdentities copies refreshed env.<account> files into the named identity
// store so subsequent credentialFor() calls see the new session.
func syncIdentities(home string) {
	accounts := loadAccounts(filepath.Join(home, ".config", "gx", "accounts.json"))
	identityDir := filepath.Join(home, ".nlm", "identities")
	for _, account := range accounts {
		src := filepath.Join(home, ".nlm", "env."+account)
		dst := filepath.Join(identityDir, account+".env")
		info, err := os.Stat(dst)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		_ = writeFileAtomic(dst, data)
	}
}

func attemptLabel(a attempt) string {
	return fmt.Sprintf("%s:rc=%d:%s", a.Account, a.RC, dash(a.Class))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func logMemo(logDir, root, account, source string, attempts []attempt, refresh string, ok bool, argc int) {
	now := time.Now()
	trial := []string{}
	for _, a := range attempts {
		trial = append(trial, attemptLabel(a))
	}
	trials := strings.Join(trial, ",")
	if trials == "" {
		trials = "-"
	}
	result := "fail"
	if ok {
		result = "ok"
	}
	line := fmt.Sprintf("- %s repo=%s root=%s account=%s source=%s argc=%d attempts=[%s] refresh=%s result=%s\n",
		now.Format("2006-01-02T15:04:05-07:00"), filepath.Base(root), root, account,
		source, argc, trials, refresh, result)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(logDir, now.Format("2006-01-02")+".md"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(line)
	_ = file.Close()
}

func cmdAccount(rest []string, p paths, cwd string, accounts []string, accountSet map[string]bool) int {
	root := repoRoot(cwd)
	cfg, _, err := loadRepoConfig(p.RepoJSON, accounts, defaultAccount(accounts), false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nlmx: repo config: %v\n", err)
		return 1
	}
	action := "show"
	if len(rest) > 0 {
		action = rest[0]
	}
	usageErr := func(command string) int {
		fmt.Fprintf(os.Stderr, "usage: nlmx account %s\n", command)
		return 2
	}
	switch action {
	case "show":
		st := loadState(p.State, accounts)
		preview := cloneRepoConfig(cfg)
		previewState := cloneState(st)
		preferred := preferredOverride(accountSet)
		c := choose(&previewState, cwd, nil, &preview, root, accounts, accountSet, preferred)
		fmt.Printf("repo: %s (%s)\n", root, filepath.Base(root))
		fmt.Printf("account: %s\n", c.Account)
		fmt.Printf("source: %s\n", c.Source)
		suffix := ""
		if accountSet[preferred] {
			suffix = " (NLMX_PREFERRED=" + preferred + ")"
		}
		fmt.Printf("default: %s%s\n", cfg.Default, suffix)
		usage := make([]string, 0, len(accounts))
		for _, a := range accounts {
			usage = append(usage, fmt.Sprintf("%s=%d", a, st.Usage[a]))
		}
		fmt.Printf("usage: %s\n", strings.Join(usage, " "))
		return 0
	case "set":
		if len(rest) < 2 || !accountSet[rest[1]] {
			return usageErr("set <" + strings.Join(accounts, "|") + ">")
		}
		cfg.Repos[root] = rest[1]
		if err := saveRepoConfig(p.RepoJSON, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
			return 1
		}
		fmt.Printf("%s: %s\n", root, rest[1])
		return 0
	case "reset":
		delete(cfg.Repos, root)
		if err := saveRepoConfig(p.RepoJSON, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
			return 1
		}
		fmt.Printf("%s: reset (default=%s)\n", root, cfg.Default)
		return 0
	case "list":
		fmt.Printf("default: %s\n", cfg.Default)
		repos := make([]string, 0, len(cfg.Repos))
		for repo := range cfg.Repos {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			fmt.Printf("%s\t%s (%s)\n", cfg.Repos[repo], repo, filepath.Base(repo))
		}
		return 0
	case "default":
		if len(rest) < 2 || !accountSet[rest[1]] {
			return usageErr("default <" + strings.Join(accounts, "|") + ">")
		}
		cfg.Default = rest[1]
		if err := saveRepoConfig(p.RepoJSON, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "nlmx: save repo config: %v\n", err)
			return 1
		}
		fmt.Printf("default: %s\n", rest[1])
		return 0
	default:
		return usageErr("[show|set <name>|reset|list|default <name>]")
	}
}

func cmdLog(rest []string, logDir string) int {
	n := 20
	if len(rest) > 0 {
		parsed, err := strconv.Atoi(rest[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "usage: nlmx log [N]")
			return 2
		}
		n = parsed
	}
	files, err := filepath.Glob(filepath.Join(logDir, "*.md"))
	if err != nil || n <= 0 {
		return 0
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	found := []string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		for i := len(lines) - 1; i >= 0 && len(found) < n; i-- {
			if strings.HasPrefix(lines[i], "- ") {
				found = append(found, lines[i])
			}
		}
		if len(found) >= n {
			break
		}
	}
	for i := len(found) - 1; i >= 0; i-- {
		fmt.Println(found[i])
	}
	return 0
}
