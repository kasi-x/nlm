package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const autoSection = "## 自動提案 (nlm)"

const topicsTemplate = `# 調べたいこと

` + "`- [ ] テーマ`" + ` を書くと nlm-queue が次の run (30分ごと) で拾って調査します。
` + "`#tag` でタグ、`!fast` で軽量調査、`@alias` で既存ノートブックに追記、`!noauto` で自動展開なし。" + `
下にインデントした ` + "`- 質問`" + ` は、レポート後に追加で聞く質問になります。
状態: ` + "`[~]` 処理中 / `[x]` 完了 (レポートへのリンク付き) / `[!]` 失敗 (`nlm-queue retry <id>`)" + `

「` + autoSection + `」には完了したテーマから nlm が次に調べるテーマを積みます。週枠に余裕が
あるときだけ自動で調査されます。要らない提案は行を消すか ` + "`[-]`" + ` にしてください。

## inbox

`

var (
	topicRE      = regexp.MustCompile(`^([-*] \[)([ ~x!-])(\] )(.*?)(?:\s*<!-- ((?:\w+:\d+ ?)+) -->)?\s*$`)
	subRE        = regexp.MustCompile(`^\s+[-*] (?:\[.\] )?(.+?)\s*$`)
	tagRE        = regexp.MustCompile(`(?:^|\s)#([^\s#\d][^\s#]*)`)
	modeRE       = regexp.MustCompile(`(?:^|\s)!(fast|deep)\b`)
	noautoRE     = regexp.MustCompile(`(?:^|\s)!noauto\b`)
	nbRE         = regexp.MustCompile(`(?:^|\s)@(\S+)`)
	linkRE       = regexp.MustCompile(`\s*→ \[レポート\]\([^)]*\)`)
	suggestionRE = regexp.MustCompile(`^\s*(?:[-*・]|\d+[.)])\s+(.+)$`)
	citationRE   = regexp.MustCompile(`\s*\[[\d,\s-]+\]`)
	normRE       = regexp.MustCompile(`[\s、。,.・「」『』()（）]`)
	markerPairRE = regexp.MustCompile(`(\w+):(\d+)`)
)

type topicSpec struct {
	Text     string
	Tags     []string
	Mode     string
	Notebook *string
	NoAuto   bool
}

func ensureNB(c config) error {
	for _, dir := range []string{c.NB, c.Reports, c.Data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(c.Topics); os.IsNotExist(err) {
		if err := os.WriteFile(c.Topics, []byte(topicsTemplate), 0o644); err != nil {
			return err
		}
	}
	gitignore := filepath.Join(c.NB, ".gitignore")
	if _, err := os.Stat(gitignore); os.IsNotExist(err) {
		if err := os.WriteFile(gitignore, []byte(".nlm-queue/\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func readTopics(c config) ([]string, error) {
	data, err := os.ReadFile(c.Topics)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

func writeTopics(c config, lines []string) error {
	return atomicWrite(c.Topics, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func topicsLines(c config) ([]string, error) {
	lines, err := readTopics(c)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return strings.Split(strings.TrimSuffix(topicsTemplate, "\n"), "\n"), nil
	}
	return lines, nil
}

func parseMarkers(match []string) map[string]int {
	marks := map[string]int{}
	if len(match) < 6 {
		return marks
	}
	for _, pair := range markerPairRE.FindAllStringSubmatch(match[5], -1) {
		value, err := strconv.Atoi(pair[2])
		if err == nil {
			marks[pair[1]] = value
		}
	}
	return marks
}

func markerString(marks map[string]int) string {
	if len(marks) == 0 {
		return ""
	}
	keys := make([]string, 0, len(marks))
	for key := range marks {
		keys = append(keys, key)
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", key, marks[key]))
	}
	return " <!-- " + strings.Join(parts, " ") + " -->"
}

// parseTopic decodes #tag, !fast/!deep, @notebook and !noauto decorations.
func parseTopic(body string) topicSpec {
	var tags []string
	for _, m := range tagRE.FindAllStringSubmatch(body, -1) {
		tags = append(tags, m[1])
	}
	mode := "deep"
	if modes := modeRE.FindAllStringSubmatch(body, -1); len(modes) > 0 {
		mode = modes[len(modes)-1][1]
	}
	var notebook *string
	if nbs := nbRE.FindAllStringSubmatch(body, -1); len(nbs) > 0 {
		value := nbs[len(nbs)-1][1]
		notebook = &value
	}
	text := linkRE.ReplaceAllString(body, "")
	for _, re := range []*regexp.Regexp{tagRE, modeRE, noautoRE, nbRE} {
		text = re.ReplaceAllString(text, "")
	}
	return topicSpec{
		Text:     strings.Join(strings.Fields(text), " "),
		Tags:     tags,
		Mode:     mode,
		Notebook: notebook,
		NoAuto:   noautoRE.MatchString(body),
	}
}

func autoBudget(c config, q *Queue, usage *usageInfo) int {
	if !c.Auto || usage == nil || usage.Weekly == nil || *usage.Weekly < c.AutoMinWeekly {
		return 0
	}
	today := time.Now().Format("2006-01-02")
	used := 0
	for _, it := range q.Items {
		if it.Parent != nil && strings.HasPrefix(it.Added, today) {
			used++
		}
	}
	if used >= c.AutoPerDay {
		return 0
	}
	return c.AutoPerDay - used
}

// syncTopics queues unchecked topics.md lines, rewriting their markers to [~].
func syncTopics(c config, usage *usageInfo) ([]*Item, error) {
	lines, err := readTopics(c)
	if err != nil {
		return nil, err
	}
	var added []*Item
	err = withQueueLock(c, func() error {
		q, err := loadQueue(c)
		if err != nil {
			return err
		}
		budget := autoBudget(c, q, usage)
		for i, line := range lines {
			match := topicRE.FindStringSubmatch(line)
			if match == nil || match[2] != " " {
				continue
			}
			marks := parseMarkers(match)
			if _, ok := marks["nlmq"]; ok {
				continue
			}
			spec := parseTopic(match[4])
			if spec.Text == "" {
				continue
			}
			parentID, isAuto := marks["auto"]
			if isAuto {
				if budget <= 0 {
					continue
				}
				budget--
			}
			var parent *Item
			if isAuto {
				parent, _ = findItem(q, parentID)
			}
			var extra []string
			for _, sub := range lines[i+1:] {
				subMatch := subRE.FindStringSubmatch(sub)
				if subMatch == nil {
					break
				}
				extra = append(extra, strings.TrimSpace(subMatch[1]))
			}
			questions := append(append([]string{}, defaultQuestions...), extra...)
			it := newItem(q, spec.Text, spec.Mode, questions, spec.Tags, spec.Notebook)
			profile := c.chooseProfile(q)
			it.Profile = &profile
			it.NoAuto = spec.NoAuto
			if isAuto {
				it.Parent = &parentID
				it.Depth = 0
				if parent != nil {
					it.Depth = parent.Depth + 1
				}
			}
			marks["nlmq"] = it.ID
			lines[i] = match[1] + "~" + match[3] + match[4] + markerString(marks)
			added = append(added, it)
		}
		if len(added) > 0 {
			if err := saveQueue(c, q); err != nil {
				return err
			}
			return writeTopics(c, lines)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, it := range added {
		kind := ""
		if it.Parent != nil {
			kind = ", auto"
		}
		logf("+ #%d %s (topics.md%s)", it.ID, it.Topic, kind)
	}
	return added, nil
}

func appendTopic(c config, it *Item) error {
	lines, err := topicsLines(c)
	if err != nil {
		return err
	}
	parts := []string{it.Topic}
	for _, tag := range it.Tags {
		parts = append(parts, "#"+tag)
	}
	if it.Mode == "fast" {
		parts = append(parts, "!fast")
	}
	if it.Notebook != nil && *it.Notebook != "" {
		parts = append(parts, "@"+*it.Notebook)
	}
	lines = append(lines, fmt.Sprintf("- [~] %s <!-- nlmq:%d -->", strings.Join(parts, " "), it.ID))
	return writeTopics(c, lines)
}

func boxForState(state string) string {
	switch state {
	case stateDone:
		return "x"
	case stateFailed:
		return "!"
	case "removed":
		return "-"
	default:
		return "~"
	}
}

func reportLink(c config, it *Item) string {
	if it.State != stateDone || it.Out == "" {
		return ""
	}
	rel, err := filepath.Rel(c.NB, it.Out)
	if err != nil || strings.HasPrefix(rel, "..") {
		return it.Out
	}
	return rel
}

func markTopic(c config, it *Item) error {
	return rewriteTopic(c, it.ID, boxForState(it.State), reportLink(c, it))
}

// rewriteTopic updates the checkbox of the line carrying the nlmq marker.
func rewriteTopic(c config, id int, box, linkText string) error {
	lines, err := readTopics(c)
	if err != nil {
		return err
	}
	for i, line := range lines {
		match := topicRE.FindStringSubmatch(line)
		if match == nil || parseMarkers(match)["nlmq"] != id {
			continue
		}
		body := linkRE.ReplaceAllString(match[4], "")
		if linkText != "" {
			body += " → [レポート](" + linkText + ")"
		}
		lines[i] = match[1] + box + match[3] + body + markerString(parseMarkers(match))
		return writeTopics(c, lines)
	}
	return nil
}

func norm(text string) string {
	return strings.ToLower(normRE.ReplaceAllString(text, ""))
}

func parseSuggestions(answer string, limit int) []string {
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		match := suggestionRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		text := strings.TrimSpace(strings.ReplaceAll(citationRE.ReplaceAllString(match[1], ""), "**", ""))
		if len([]rune(text)) >= 8 && len([]rune(text)) <= 200 {
			out = append(out, text)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func appendSuggestions(c config, it *Item) error {
	lines, err := topicsLines(c)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, line := range lines {
		if match := topicRE.FindStringSubmatch(line); match != nil {
			seen[norm(parseTopic(match[4]).Text)] = true
		}
	}
	tags := []string{"auto"}
	for _, tag := range it.Tags {
		if tag != "auto" {
			tags = append(tags, tag)
		}
	}
	var fresh []string
	for _, suggestion := range it.Suggestions {
		if seen[norm(suggestion)] {
			continue
		}
		decorations := ""
		for _, tag := range tags {
			decorations += " #" + tag
		}
		fresh = append(fresh, fmt.Sprintf("- [ ] %s%s <!-- auto:%d -->", suggestion, decorations, it.ID))
	}
	if len(fresh) == 0 {
		return nil
	}
	at := -1
	for i, line := range lines {
		if line == autoSection {
			at = i + 1
			break
		}
	}
	if at < 0 {
		lines = append(lines, "", autoSection, "")
		at = len(lines)
	}
	for at < len(lines) && strings.TrimSpace(lines[at]) == "" {
		at++
	}
	end := at
	for end < len(lines) && !strings.HasPrefix(lines[end], "## ") {
		end++
	}
	for end > at && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	updated := make([]string, 0, len(lines)+len(fresh))
	updated = append(updated, lines[:at]...)
	updated = append(updated, fresh...)
	updated = append(updated, lines[at:]...)
	return writeTopics(c, updated)
}
