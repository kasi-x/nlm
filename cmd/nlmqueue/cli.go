package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type flagDef struct {
	name  string
	short string
	value bool
}

type cli struct {
	cfg    config
	out    io.Writer
	errOut io.Writer
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(args []string, out, errOut io.Writer) int {
	c, err := loadConfig()
	if err != nil {
		fmt.Fprintf(errOut, "nlm-queue: %v\n", err)
		return 1
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	cmd := &cli{cfg: c, out: out, errOut: errOut}
	var fn func(map[string][]string, []string) error
	var defs []flagDef
	switch args[0] {
	case "add":
		fn, defs = cmd.cmdAdd, []flagDef{
			{name: "file", short: "f", value: true},
			{name: "mode", short: "m", value: true},
			{name: "question", short: "q", value: true},
			{name: "no-default-questions"},
			{name: "notebook", short: "n", value: true},
			{name: "tags", short: "t", value: true},
		}
	case "init":
		fn = cmd.cmdInit
	case "sync":
		fn = cmd.cmdSync
	case "list", "ls":
		fn, defs = cmd.cmdList, []flagDef{{name: "all", short: "a"}}
	case "usage":
		fn = cmd.cmdUsage
	case "show":
		fn, defs = cmd.cmdShow, []flagDef{{name: "log", value: true}}
	case "retry":
		fn = cmd.cmdRetry
	case "rm":
		fn = cmd.cmdRm
	case "run":
		fn, defs = cmd.cmdRun, []flagDef{
			{name: "once"},
			{name: "profile", value: true},
		}
	default:
		fmt.Fprintf(errOut, "nlm-queue: 不明なコマンド: %s\n", args[0])
		return 2
	}
	values, positional, err := parseFlags(args[1:], defs)
	if err != nil {
		fmt.Fprintf(errOut, "nlm-queue %s: %v\n", args[0], err)
		return 2
	}
	if err := fn(values, positional); err != nil {
		fmt.Fprintf(errOut, "nlm-queue: %v\n", err)
		return 1
	}
	return 0
}

// parseFlags accepts flags before or after positional arguments, matching
// argparse's behavior for `nlm-queue add "topic" --mode fast`.
func parseFlags(args []string, defs []flagDef) (map[string][]string, []string, error) {
	values := map[string][]string{}
	var positional []string
	lookup := func(name string) (flagDef, bool) {
		for _, def := range defs {
			if def.name == name || (def.short != "" && def.short == name) {
				return def, true
			}
		}
		return flagDef{}, false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		var inline string
		if idx := strings.Index(name, "="); idx >= 0 {
			inline, name = name[idx+1:], name[:idx]
		}
		def, ok := lookup(name)
		if !ok {
			return nil, nil, fmt.Errorf("未知のオプション: %s", arg)
		}
		if !def.value {
			values[def.name] = append(values[def.name], "true")
			continue
		}
		if inline == "" {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s には値が必要です", arg)
			}
			i++
			inline = args[i]
		}
		values[def.name] = append(values[def.name], inline)
	}
	return values, positional, nil
}

func flagValue(values map[string][]string, name, fallback string) string {
	if vals := values[name]; len(vals) > 0 {
		return vals[len(vals)-1]
	}
	return fallback
}

func flagBool(values map[string][]string, name string) bool {
	return len(values[name]) > 0
}

var stateMark = map[string]string{
	statePending: "○", stateResearching: "◔", stateImporting: "◑",
	stateReporting: "◕", stateAsking: "◕", stateDone: "✓", stateFailed: "▲",
}

func (c *cli) cmdAdd(values map[string][]string, positional []string) error {
	if err := ensureNB(c.cfg); err != nil {
		return err
	}
	var topics []string
	if file := flagValue(values, "file", ""); file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				topics = append(topics, line)
			}
		}
	}
	if len(positional) > 0 {
		topics = append(topics, strings.Join(positional, " "))
	}
	if len(topics) == 0 {
		return errors.New("テーマを指定してください")
	}
	mode := flagValue(values, "mode", "deep")
	if mode != "fast" && mode != "deep" {
		return fmt.Errorf("--mode は fast または deep です: %s", mode)
	}
	questions := []string{}
	if !flagBool(values, "no-default-questions") {
		questions = append(questions, defaultQuestions...)
	}
	questions = append(questions, values["question"]...)
	var tags []string
	for _, tag := range strings.Split(flagValue(values, "tags", ""), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	var notebook *string
	if alias := flagValue(values, "notebook", ""); alias != "" {
		notebook = &alias
	}
	var items []*Item
	if err := withQueueLock(c.cfg, func() error {
		q, err := loadQueue(c.cfg)
		if err != nil {
			return err
		}
		for _, topic := range topics {
			it := newItem(q, topic, mode, questions, tags, notebook)
			profile := c.cfg.chooseProfile(q)
			it.Profile = &profile
			items = append(items, it)
		}
		return saveQueue(c.cfg, q)
	}); err != nil {
		return err
	}
	for _, it := range items {
		if err := appendTopic(c.cfg, it); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "+ #%d %s\n", it.ID, it.Topic)
	}
	return nil
}

func (c *cli) cmdInit(values map[string][]string, positional []string) error {
	if err := ensureNB(c.cfg); err != nil {
		return err
	}
	if _, err := exec.LookPath("git"); err == nil {
		if _, err := os.Stat(filepath.Join(c.cfg.NB, ".git")); os.IsNotExist(err) {
			_ = exec.Command("git", "init", "-q", c.cfg.NB).Run()
		}
	}
	fmt.Fprintf(c.out, "%s を用意しました。%s に `- [ ] テーマ` を書いてください\n", c.cfg.NB, c.cfg.Topics)
	return nil
}

func (c *cli) cmdSync(values map[string][]string, positional []string) error {
	if err := ensureNB(c.cfg); err != nil {
		return err
	}
	var usage *usageInfo
	r, err := newRunner(c.cfg, c.cfg.Profile)
	if err != nil {
		logf("usage 取得失敗 (自動提案は保留): %v", err)
	} else {
		usage, err = r.fetchUsage()
		if err != nil {
			logf("usage 取得失敗 (自動提案は保留): %v", err)
			usage = nil
		}
	}
	added, err := syncTopics(c.cfg, usage)
	if err != nil {
		return err
	}
	if len(added) > 0 {
		fmt.Fprintf(c.out, "topics.md から %d 件投入\n", len(added))
	} else {
		fmt.Fprintln(c.out, "新しいテーマはありません")
	}
	return nil
}

func (c *cli) cmdList(values map[string][]string, positional []string) error {
	q, err := loadQueue(c.cfg)
	if err != nil {
		return err
	}
	showAll := flagBool(values, "all")
	var items []*Item
	if showAll {
		items = q.Items
	} else {
		for _, it := range q.Items {
			if it.State != stateDone {
				items = append(items, it)
			}
		}
		if len(items) > 50 {
			items = items[len(items)-50:]
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(c.out, `キューは空です (nlm-queue add "テーマ")`)
	}
	for _, it := range items {
		extra := ""
		if it.Error != "" {
			extra = "  ← " + truncate(it.Error, 60)
		}
		if it.State == stateDone && it.Out != "" {
			extra = "  → " + it.Out
		}
		profile := ""
		if len(c.cfg.Profiles) > 0 {
			profile = fmt.Sprintf("[%s] ", c.cfg.profileFor(it))
		}
		mark := stateMark[it.State]
		if mark == "" {
			mark = "?"
		}
		fmt.Fprintf(c.out, "%s #%-3d %-11s %s%s%s\n", mark, it.ID, it.State, profile, truncate(it.Topic, 60), extra)
	}
	if !showAll {
		done := 0
		for _, it := range q.Items {
			if it.State == stateDone {
				done++
			}
		}
		if done > 0 {
			fmt.Fprintf(c.out, "(完了 %d 件は --all で表示)\n", done)
		}
	}
	return nil
}

func (c *cli) cmdUsage(values map[string][]string, positional []string) error {
	q, err := loadQueue(c.cfg)
	if err != nil {
		return err
	}
	for _, profile := range c.cfg.activeProfiles() {
		r, err := newRunner(c.cfg, profile)
		if err != nil {
			fmt.Fprintf(c.out, "[%s] 取得失敗: %v\n", displayProfile(profile), err)
			continue
		}
		usage, err := r.fetchUsage()
		if err != nil {
			fmt.Fprintf(c.out, "[%s] 取得失敗: %v\n", displayProfile(profile), err)
			continue
		}
		fmt.Fprintf(c.out, "[%s] 5h枠 残り %s%% (リセット %s) / 週 %s%% (%s)\n",
			displayProfile(profile), formatFloat(usage.Rolling), displayTime(usage.RollingAt),
			formatFloat(usage.Weekly), usage.Tier)
	}
	pending, active, failed := 0, 0, 0
	for _, it := range q.Items {
		switch it.State {
		case statePending:
			pending++
		case stateFailed:
			failed++
		default:
			if activeStates[it.State] {
				active++
			}
		}
	}
	fmt.Fprintf(c.out, "キュー: 待ち %d / 処理中 %d / 失敗 %d\n", pending, active, failed)
	return nil
}

func displayProfile(profile string) string {
	if profile == "" {
		return "default"
	}
	return profile
}

func timeParse(value string) (time.Time, error) {
	return time.Parse(time.RFC3339, value)
}

func displayTime(value string) string {
	if value == "" {
		return "-"
	}
	if t, err := timeParse(value); err == nil {
		return t.Format("01/02 15:04")
	}
	return value
}

func (c *cli) cmdShow(values map[string][]string, positional []string) error {
	if len(positional) == 0 {
		return errors.New("id を指定してください")
	}
	id, err := strconv.Atoi(positional[0])
	if err != nil {
		return fmt.Errorf("id が不正です: %s", positional[0])
	}
	q, err := loadQueue(c.cfg)
	if err != nil {
		return err
	}
	it, err := findItem(q, id)
	if err != nil {
		return err
	}
	view := *it
	if limit := flagValue(values, "log", "0"); limit != "0" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			return fmt.Errorf("--log が不正です: %s", limit)
		}
		if n < len(view.Answers) {
			view.Answers = append([]Answer(nil), view.Answers[len(view.Answers)-n:]...)
		}
	} else {
		view.Answers = nil
	}
	data, err := json.MarshalIndent(view, "", " ")
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, string(data))
	return nil
}

func (c *cli) cmdRetry(values map[string][]string, positional []string) error {
	if len(positional) == 0 {
		return errors.New("id を指定してください")
	}
	id, err := strconv.Atoi(positional[0])
	if err != nil {
		return fmt.Errorf("id が不正です: %s", positional[0])
	}
	var item *Item
	if err := withQueueLock(c.cfg, func() error {
		q, err := loadQueue(c.cfg)
		if err != nil {
			return err
		}
		it, err := findItem(q, id)
		if err != nil {
			return err
		}
		if it.State == stateFailed {
			it.State = statePending
		}
		it.Error = ""
		it.Attempts = 0
		it.FailedAt = ""
		item = it
		return saveQueue(c.cfg, q)
	}); err != nil {
		return err
	}
	if err := rewriteTopic(c.cfg, id, " ", ""); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "#%d → %s\n", id, item.State)
	return nil
}

func (c *cli) cmdRm(values map[string][]string, positional []string) error {
	if len(positional) == 0 {
		return errors.New("id を指定してください")
	}
	id, err := strconv.Atoi(positional[0])
	if err != nil {
		return fmt.Errorf("id が不正です: %s", positional[0])
	}
	var item *Item
	if err := withQueueLock(c.cfg, func() error {
		q, err := loadQueue(c.cfg)
		if err != nil {
			return err
		}
		it, err := findItem(q, id)
		if err != nil {
			return err
		}
		item = it
		kept := q.Items[:0]
		for _, other := range q.Items {
			if other.ID != id {
				kept = append(kept, other)
			}
		}
		q.Items = kept
		return saveQueue(c.cfg, q)
	}); err != nil {
		return err
	}
	if err := rewriteTopic(c.cfg, id, "-", ""); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(c.cfg.Data, "items", strconv.Itoa(id)))
	fmt.Fprintf(c.out, "- #%d %s (NotebookLM 側のノートブックは残ります)\n", id, item.Topic)
	return nil
}

func (c *cli) cmdRun(values map[string][]string, positional []string) error {
	if profile := flagValue(values, "profile", ""); profile != "" {
		c.cfg.Profile = profile
		c.cfg.Profiles = []string{profile}
	}
	if err := ensureNB(c.cfg); err != nil {
		return err
	}
	lock, err := tryRunLock(c.cfg, c.cfg.Profile)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		logf("別の run が実行中")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()

	r, err := newRunner(c.cfg, c.cfg.Profile)
	if err != nil {
		return err
	}
	usage, err := r.fetchUsage()
	if err != nil {
		logf("usage 取得失敗: %v", err)
		usage = nil
	} else if problem := quotaProblem(usage, c.cfg); problem != "" {
		logf("保留: %s", problem)
		return nil
	}
	if _, err := syncTopics(c.cfg, usage); err != nil {
		return err
	}
	q, err := loadQueue(c.cfg)
	if err != nil {
		return err
	}
	active, pending := selectWork(q, c.cfg)
	once := flagBool(values, "once")
	slots := c.cfg.MaxActive - len(active)
	if slots < 0 {
		slots = 0
	}
	if once && slots > 1 {
		slots = 1
	}
	for _, it := range pending {
		if slots <= 0 {
			break
		}
		if err := advance(c.cfg, r, it, true); err != nil && !errors.Is(err, errQuotaLow) {
			logf("#%d 開始できませんでした: %v", it.ID, err)
		}
		if activeStates[it.State] {
			active = append(active, it)
			slots--
		}
	}
	limit := c.cfg.MaxActive
	if once {
		limit = 1
	}
	processed := 0
	for _, it := range active {
		if processed >= limit {
			break
		}
		if err := advance(c.cfg, r, it, false); err != nil {
			logf("#%d: %v", it.ID, err)
		}
		if it.State == stateDone || it.State == stateFailed {
			processed++
		}
	}
	logf("run 終了 (完了/失敗 %d 件)", processed)
	return nil
}
