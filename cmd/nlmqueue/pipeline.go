package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var slugRE = regexp.MustCompile(`[\\/:*?"<>|\s]+`)

func selectWork(q *Queue, c config) (active, pending []*Item) {
	for _, it := range q.Items {
		if !c.mine(it) {
			continue
		}
		switch {
		case activeStates[it.State]:
			active = append(active, it)
		case it.State == statePending:
			pending = append(pending, it)
		}
	}
	return active, pending
}

func step(c config, r *runner, it *Item) (bool, error) {
	switch it.State {
	case statePending:
		if it.NotebookID == "" {
			if it.Notebook != nil && *it.Notebook != "" {
				it.NotebookID = *it.Notebook
			} else {
				id, err := r.createNotebook(it)
				if err != nil {
					return false, err
				}
				it.NotebookID = id
			}
		}
		outcome, err := r.research(it)
		if err != nil {
			return false, err
		}
		dir, err := itemDir(c, it)
		if err != nil {
			return false, err
		}
		if err := atomicWrite(filepath.Join(dir, "research.md"), []byte(outcome.Report), 0o644); err != nil {
			return false, err
		}
		if outcome.TaskID != "" {
			it.TaskID = outcome.TaskID
		}
		if outcome.Imported != nil {
			it.Imported = outcome.Imported
		} else if outcome.Sources > 0 {
			count := outcome.Sources
			it.Imported = &count
		}
		it.State = stateResearching
		logf("#%d research 開始 (%s) nb=%s", it.ID, it.Mode, it.NotebookID)
		return true, nil

	case stateResearching:
		it.State = stateImporting
		return true, nil

	case stateImporting:
		it.State = stateReporting
		return true, nil

	case stateReporting:
		if err := r.quotaGate(c, "report"); err != nil {
			return false, err
		}
		report, err := r.generateReport(it.NotebookID)
		if err != nil {
			return false, err
		}
		dir, err := itemDir(c, it)
		if err != nil {
			return false, err
		}
		if err := atomicWrite(filepath.Join(dir, "report.md"), []byte(report+"\n"), 0o644); err != nil {
			return false, err
		}
		it.State = stateAsking
		logf("#%d report 取得", it.ID)
		return true, nil

	case stateAsking:
		answers := append([]Answer(nil), it.Answers...)
		remaining := 0
		if len(answers) < len(it.Questions) {
			remaining = len(it.Questions) - len(answers)
		}
		for _, question := range it.Questions[len(it.Questions)-remaining:] {
			if err := r.quotaGate(c, "query"); err != nil {
				return false, err
			}
			answer, err := r.ask(it.NotebookID, question)
			if err != nil {
				return false, err
			}
			answers = append(answers, Answer{Q: question, A: answer})
			it.Answers = answers
			if err := updateItem(c, it); err != nil {
				return false, err
			}
		}
		wantsAuto := c.Auto && it.Depth < c.AutoDepth && !it.NoAuto && len(it.Suggestions) == 0
		if wantsAuto {
			if err := r.quotaGate(c, "auto-suggest"); err != nil {
				return false, err
			}
			answer, err := r.ask(it.NotebookID, strings.ReplaceAll(autoPrompt, "{n}", strconv.Itoa(c.AutoChildren)))
			if err != nil {
				return false, err
			}
			it.Suggestions = parseSuggestions(answer, c.AutoChildren)
			if err := updateItem(c, it); err != nil {
				return false, err
			}
		}
		path, err := writeOutput(c, it)
		if err != nil {
			return false, err
		}
		it.Out = path
		it.State = stateDone
		if len(it.Suggestions) > 0 && !it.SuggestionsAdded {
			if err := appendSuggestions(c, it); err != nil {
				return false, err
			}
			it.SuggestionsAdded = true
		}
		logf("#%d 完了 → %s", it.ID, path)
		return false, nil
	}
	return false, nil
}

// advance runs a state machine until the item completes, fails, defers, or
// one step has been made (when once is true).
func advance(c config, r *runner, it *Item, once bool) error {
	for it.State != stateDone && it.State != stateFailed {
		before := it.State
		cont, err := step(c, r, it)
		if err != nil {
			if errors.Is(err, errQuotaLow) {
				logf("保留: %v", err)
				return err
			}
			it.Error = truncate(err.Error(), 500)
			it.Attempts++
			it.FailedAt = before
			it.State = stateFailed
			if updateErr := updateItem(c, it); updateErr != nil {
				return updateErr
			}
			if markErr := markTopic(c, it); markErr != nil {
				return markErr
			}
			return err
		}
		it.Error = ""
		if err := updateItem(c, it); err != nil {
			return err
		}
		if it.State == stateDone {
			if err := markTopic(c, it); err != nil {
				return err
			}
		}
		if !cont || once {
			return nil
		}
	}
	return nil
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

func demoteHeadings(md string) string {
	var out []string
	fence := false
	for _, line := range strings.Split(strings.TrimRight(md, "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(line, "#") {
			line = "#" + line
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func slug(text string, max int) string {
	safe := strings.Trim(slugRE.ReplaceAllString(text, "_"), "_.")
	runes := []rune(safe)
	if len(runes) > max {
		runes = runes[:max]
	}
	if len(runes) == 0 {
		return "untitled"
	}
	return string(runes)
}

func writeOutput(c config, it *Item) (string, error) {
	dir, err := itemDir(c, it)
	if err != nil {
		return "", err
	}
	notebook := it.NotebookURL
	if notebook == "" {
		notebook = it.NotebookID
	}
	if notebook == "" && it.Notebook != nil {
		notebook = *it.Notebook
	}
	imported := "?"
	if it.Imported != nil {
		imported = strconv.Itoa(*it.Imported)
	}
	parts := []string{
		"# " + it.Topic,
		"",
		"- ノートブック: " + notebook,
		fmt.Sprintf("- 生成: %s / research=%s / 取込ソース %s 件 / nlm-queue #%d",
			nowISO(), it.Mode, imported, it.ID),
	}
	if len(it.Tags) > 0 {
		parts = append(parts, "- tags: "+strings.Join(it.Tags, ", "))
	}
	parts = append(parts, "")

	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err == nil {
		parts = append(parts, "## レポート", "", demoteHeadings(string(report)), "")
	}
	if len(it.Answers) > 0 {
		parts = append(parts, "## 深掘り Q&A", "")
		for i, qa := range it.Answers {
			parts = append(parts, fmt.Sprintf("### Q%d. %s", i+1, qa.Q), "", strings.TrimSpace(qa.A), "")
		}
	}
	if len(it.Suggestions) > 0 {
		parts = append(parts, "## 次に調べる候補 (topics.md の自動提案へ追加済み)", "")
		for _, suggestion := range it.Suggestions {
			parts = append(parts, "- "+suggestion)
		}
		parts = append(parts, "")
	}
	research, err := os.ReadFile(filepath.Join(dir, "research.md"))
	if err == nil && strings.TrimSpace(string(research)) != "" {
		parts = append(parts, "## Deep Research 原レポート", "", demoteHeadings(string(research)), "")
	}

	name := fmt.Sprintf("%s_%03d_%s.md", time.Now().Format("2006-01-02"), it.ID, slug(it.Topic, 60))
	path := filepath.Join(c.Reports, name)
	if err := atomicWrite(path, []byte(strings.Join(parts, "\n")+"\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
