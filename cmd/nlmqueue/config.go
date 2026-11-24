package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// config holds the environment-derived settings. It is built per invocation
// so tests and long-running processes see current environment variables.
type config struct {
	NB            string
	Topics        string
	Data          string
	Queue         string
	Lock          string
	Reports       string
	Profiles      []string
	Profile       string
	MaxActive     int
	Reserve       float64
	MinRolling    float64
	Auto          bool
	AutoDepth     int
	AutoChildren  int
	AutoPerDay    int
	AutoMinWeekly float64
	Lang          string
	NLMXBin       string
}

const (
	autoPrompt = "この調査結果を踏まえ、次に深掘りすべき具体的な調査テーマを{n}件挙げてください。" +
		"このノートブックで既に十分扱われた内容は除き、それぞれ Web で調べられる1文の日本語の" +
		"調査クエリにしてください。各行を「- 」で始め、前置き・説明・引用番号は書かないでください。"

	statePending     = "pending"
	stateResearching = "researching"
	stateImporting   = "importing"
	stateReporting   = "reporting"
	stateAsking      = "asking"
	stateDone        = "done"
	stateFailed      = "failed"
)

var defaultQuestions = []string{
	"このテーマの主要な結論を、根拠となるソースとともに整理してください。" +
		"ソース間で見解が分かれている点があれば明記してください。",
	"未解決の論点・リスク・反論・情報の古さが懸念される点を挙げてください。",
	"実務で今すぐ試せる具体的な次のアクションを、優先度順に挙げてください。",
}

var activeStates = map[string]bool{
	stateResearching: true,
	stateImporting:   true,
	stateReporting:   true,
	stateAsking:      true,
}

func loadConfig() (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, err
	}
	nb := os.Getenv("NLM_QUEUE_HOME")
	if nb == "" {
		nb = filepath.Join(home, "nb")
	} else if strings.HasPrefix(nb, "~") {
		nb = filepath.Join(home, strings.TrimPrefix(nb, "~/"))
	}
	nb = filepath.Clean(nb)
	data := filepath.Join(nb, ".nlm-queue")
	reports := filepath.Join(nb, "reports")
	if out := os.Getenv("NLM_QUEUE_OUT"); out != "" {
		reports = out
	}
	c := config{
		NB:            nb,
		Topics:        filepath.Join(nb, "topics.md"),
		Data:          data,
		Queue:         filepath.Join(data, "queue.json"),
		Lock:          filepath.Join(data, "queue.lock"),
		Reports:       reports,
		Profiles:      splitList(os.Getenv("NLM_QUEUE_PROFILES"), "iida,saitama"),
		Profile:       os.Getenv("NLM_QUEUE_PROFILE"),
		MaxActive:     envInt("NLM_QUEUE_MAX_ACTIVE", 3),
		Reserve:       envFloat("NLM_QUEUE_RESERVE", 10),
		MinRolling:    envFloat("NLM_QUEUE_MIN_ROLLING", 10),
		Auto:          os.Getenv("NLM_QUEUE_AUTO") != "0",
		AutoDepth:     envInt("NLM_QUEUE_AUTO_DEPTH", 3),
		AutoChildren:  envInt("NLM_QUEUE_AUTO_CHILDREN", 3),
		AutoPerDay:    envInt("NLM_QUEUE_AUTO_PER_DAY", 24),
		AutoMinWeekly: envFloat("NLM_QUEUE_AUTO_MIN_WEEKLY", 20),
		Lang:          envString("NLM_QUEUE_LANG", "ja"),
		NLMXBin:       os.Getenv("NLMX_BIN"),
	}
	return c, nil
}

func splitList(raw, fallback string) []string {
	if raw == "" {
		raw = fallback
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return v
}

func envFloat(name string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil {
		return fallback
	}
	return v
}

// activeProfiles returns the profiles usable in this environment. A single
// profile is represented as a one-element slice.
func (c config) activeProfiles() []string {
	if len(c.Profiles) > 0 {
		return append([]string(nil), c.Profiles...)
	}
	return []string{c.Profile}
}

func (c config) profileFor(it *Item) string {
	if it.Profile != nil && *it.Profile != "" {
		return *it.Profile
	}
	if activeStates[it.State] {
		if len(c.Profiles) > 0 {
			return c.Profiles[0]
		}
		return c.Profile
	}
	if len(c.Profiles) == 0 {
		return c.Profile
	}
	idx := it.ID - 1
	if idx < 0 {
		idx = 0
	}
	return c.Profiles[idx%len(c.Profiles)]
}

func (c config) mine(it *Item) bool {
	if c.Profile != "" {
		return c.profileFor(it) == c.Profile
	}
	return true
}

// chooseProfile assigns a new item to the least loaded profile.
func (c config) chooseProfile(q *Queue) string {
	if len(c.Profiles) == 0 {
		return c.Profile
	}
	load := make(map[string]int, len(c.Profiles))
	for _, p := range c.Profiles {
		load[p] = 0
	}
	for _, it := range q.Items {
		p := c.profileFor(it)
		if _, ok := load[p]; ok {
			load[p]++
		}
	}
	best := c.Profiles[0]
	for _, p := range c.Profiles[1:] {
		if load[p] < load[best] {
			best = p
		}
	}
	return best
}

func nowISO() string {
	return time.Now().Format("2006-01-02T15:04:05")
}

func logf(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
