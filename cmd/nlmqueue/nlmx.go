package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var errQuotaLow = errors.New("quota low")

type usageInfo struct {
	Account   string
	Tier      string
	Rolling   *float64
	Weekly    *float64
	RollingAt string
	WeeklyAt  string
	Error     string
}

type runner struct {
	cfg     config
	profile string
	bin     string
}

func newRunner(c config, profile string) (*runner, error) {
	bin, err := resolveNLMX(c)
	if err != nil {
		return nil, err
	}
	return &runner{cfg: c, profile: profile, bin: bin}, nil
}

func resolveNLMX(c config) (string, error) {
	if c.NLMXBin != "" {
		return c.NLMXBin, nil
	}
	if path, err := exec.LookPath("nlmx"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(home, "go", "bin", "nlmx-go")
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", errors.New("nlmx が見つかりません (NLMX_BIN を設定してください)")
}

func (r *runner) exec(args []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin, args...)
	if _, err := os.Stat(r.cfg.NB); err == nil {
		cmd.Dir = r.cfg.NB
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "NLMX_PREFERRED=") {
			env = append(env, entry)
		}
	}
	if r.profile != "" {
		env = append(env, "NLMX_PREFERRED="+r.profile)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return out, fmt.Errorf("nlmx %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return out, fmt.Errorf("nlmx %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

type usageWindow struct {
	Window           string   `json:"window"`
	PercentRemaining *float64 `json:"percent_remaining"`
	ResetsAt         string   `json:"resets_at"`
}

type usageEntry struct {
	Account string        `json:"account"`
	Tier    string        `json:"tier"`
	Error   string        `json:"error"`
	Windows []usageWindow `json:"windows"`
}

// fetchUsage calls `nlmx usage --json`. With profiles configured, the entry
// whose account matches the item's profile is the authoritative one.
func (r *runner) fetchUsage() (*usageInfo, error) {
	args := []string{"usage", "--json"}
	if r.profile != "" {
		args = append(args, "--account", r.profile)
	}
	out, err := r.exec(args, 90*time.Second)
	if err != nil {
		return nil, err
	}
	var entries []usageEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("nlmx usage --json: %w", err)
	}
	var chosen *usageEntry
	for i := range entries {
		if entries[i].Error != "" {
			continue
		}
		if r.profile != "" {
			if entries[i].Account == r.profile {
				chosen = &entries[i]
				break
			}
			continue
		}
		chosen = &entries[i]
		break
	}
	if chosen == nil && r.profile != "" && len(entries) == 1 && entries[0].Account == "" && entries[0].Error == "" {
		chosen = &entries[0]
	}
	if chosen == nil {
		return nil, fmt.Errorf("usage: account %q の情報がありません", r.profile)
	}
	info := &usageInfo{Account: chosen.Account, Tier: chosen.Tier}
	for _, w := range chosen.Windows {
		switch w.Window {
		case "rolling":
			info.Rolling = w.PercentRemaining
			info.RollingAt = w.ResetsAt
		case "weekly":
			info.Weekly = w.PercentRemaining
			info.WeeklyAt = w.ResetsAt
		}
	}
	return info, nil
}

func quotaProblem(info *usageInfo, c config) string {
	if info == nil {
		return ""
	}
	if info.Weekly != nil && *info.Weekly < c.Reserve {
		return fmt.Sprintf("週枠残り %.1f%% < reserve %.1f%%", *info.Weekly, c.Reserve)
	}
	if info.Rolling != nil && *info.Rolling < c.MinRolling {
		return fmt.Sprintf("5h枠残り %.1f%% < min %.1f%%", *info.Rolling, c.MinRolling)
	}
	return ""
}

// quotaGate defers paid steps when quota is low. If usage cannot be read the
// step proceeds: the subprocess may still have quota available.
func (r *runner) quotaGate(c config, what string) error {
	info, err := r.fetchUsage()
	if err != nil {
		logf("usage 取得失敗 (%s): %v", what, err)
		return nil
	}
	if problem := quotaProblem(info, c); problem != "" {
		return fmt.Errorf("%w: %s (%s を保留)", errQuotaLow, problem, what)
	}
	return nil
}

type researchEvent struct {
	Type          string            `json:"type"`
	Status        string            `json:"status"`
	Report        string            `json:"report"`
	Message       string            `json:"message"`
	Error         string            `json:"error"`
	TaskID        string            `json:"task_id"`
	Sources       []json.RawMessage `json:"sources"`
	SourcesFound  *int              `json:"sources_found"`
	ImportedCount *int              `json:"imported_count"`
	Imported      *int              `json:"imported"`
}

type researchOutcome struct {
	Report   string
	Sources  int
	TaskID   string
	Imported *int
}

// parseResearchLines reads only JSON-lines events; the import log that follows
// them is plain text and is intentionally ignored.
func parseResearchLines(output string) (researchOutcome, error) {
	var result researchOutcome
	complete := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev researchEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "complete" || ev.Status == "completed" || ev.Status == "complete" {
			complete = true
		}
		if ev.Status == "failed" || ev.Status == "error" {
			return result, fmt.Errorf("research failed: %s", ev.Message)
		}
		if ev.Error != "" {
			return result, errors.New(ev.Error)
		}
		if ev.Report != "" {
			result.Report = ev.Report
		}
		if ev.TaskID != "" {
			result.TaskID = ev.TaskID
		}
		if ev.SourcesFound != nil {
			result.Sources = *ev.SourcesFound
		} else if len(ev.Sources) > 0 {
			result.Sources = len(ev.Sources)
		}
		if ev.ImportedCount != nil {
			count := *ev.ImportedCount
			result.Imported = &count
		} else if ev.Imported != nil {
			count := *ev.Imported
			result.Imported = &count
		}
	}
	if !complete && result.Report == "" {
		return result, errors.New("research: complete イベントがありません")
	}
	return result, nil
}

func (r *runner) research(it *Item) (researchOutcome, error) {
	notebook := it.NotebookID
	if notebook == "" && it.Notebook != nil {
		notebook = *it.Notebook
	}
	if notebook == "" {
		return researchOutcome{}, errors.New("ノートブックが未設定です")
	}
	args := []string{"research", "--mode", it.Mode, "--import", notebook, it.Topic}
	timeout := 5 * time.Minute
	if it.Mode == "deep" {
		timeout = 25 * time.Minute
	}
	out, err := r.exec(args, timeout)
	if err != nil {
		return researchOutcome{}, err
	}
	return parseResearchLines(string(out))
}

func (r *runner) createNotebook(it *Item) (string, error) {
	title := []rune(it.Topic)
	if len(title) > 100 {
		title = title[:100]
	}
	out, err := r.exec([]string{"notebook", "create", string(title)}, 3*time.Minute)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var payload struct {
			NotebookID string `json:"notebook_id"`
			ID         string `json:"id"`
		}
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &payload) == nil {
			if payload.NotebookID != "" {
				return payload.NotebookID, nil
			}
			if payload.ID != "" {
				return payload.ID, nil
			}
			continue
		}
		return line, nil
	}
	return "", errors.New("notebook create: ID が返されませんでした")
}

func (r *runner) generateReport(notebook string) (string, error) {
	out, err := r.exec([]string{"generate-report", notebook}, 10*time.Minute)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (r *runner) ask(notebook, question string) (string, error) {
	out, err := r.exec([]string{"generate-chat", "--citations", "off", notebook, question}, 10*time.Minute)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func formatFloat(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', 1, 64)
}
