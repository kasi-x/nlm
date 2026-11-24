package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Answer is one question/answer pair collected in the asking state.
type Answer struct {
	Q string `json:"q"`
	A string `json:"a"`
}

// Item is the Python queue schema. Extra carries fields written by other
// versions of nlm-queue so loading and saving stays lossless.
type Item struct {
	ID               int                        `json:"id"`
	Topic            string                     `json:"topic"`
	State            string                     `json:"state"`
	Mode             string                     `json:"mode"`
	Questions        []string                   `json:"questions"`
	Tags             []string                   `json:"tags"`
	Notebook         *string                    `json:"notebook"`
	Parent           *int                       `json:"parent,omitempty"`
	Depth            int                        `json:"depth,omitempty"`
	NoAuto           bool                       `json:"noauto,omitempty"`
	Profile          *string                    `json:"profile"`
	Added            string                     `json:"added,omitempty"`
	Updated          string                     `json:"updated,omitempty"`
	Error            string                     `json:"error,omitempty"`
	Out              string                     `json:"out,omitempty"`
	TaskID           string                     `json:"task_id,omitempty"`
	Imported         *int                       `json:"imported,omitempty"`
	ReportID         string                     `json:"report_id,omitempty"`
	Answers          []Answer                   `json:"answers,omitempty"`
	Suggestions      []string                   `json:"suggestions,omitempty"`
	NotebookID       string                     `json:"notebook_id,omitempty"`
	NotebookURL      string                     `json:"notebook_url,omitempty"`
	Attempts         int                        `json:"attempts,omitempty"`
	FailedAt         string                     `json:"failed_at,omitempty"`
	SuggestionsAdded bool                       `json:"suggestions_added,omitempty"`
	Extra            map[string]json.RawMessage `json:"-"`
}

var itemFields = map[string]bool{
	"id": true, "topic": true, "state": true, "mode": true,
	"questions": true, "tags": true, "notebook": true, "parent": true,
	"depth": true, "noauto": true, "profile": true, "added": true,
	"updated": true, "error": true, "out": true, "task_id": true,
	"imported": true, "report_id": true, "answers": true,
	"suggestions": true, "notebook_id": true, "notebook_url": true,
	"attempts": true, "failed_at": true, "suggestions_added": true,
}

func (it *Item) UnmarshalJSON(data []byte) error {
	type plain Item
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*it = Item(p)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key, value := range raw {
		if !itemFields[key] {
			if it.Extra == nil {
				it.Extra = make(map[string]json.RawMessage)
			}
			it.Extra[key] = value
		}
	}
	return nil
}

func (it Item) MarshalJSON() ([]byte, error) {
	type plain Item
	p := plain(it)
	if p.Questions == nil {
		p.Questions = []string{}
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	data, err := json.Marshal(p)
	if err != nil || len(it.Extra) == 0 {
		return data, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for key, value := range it.Extra {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// Queue is the on-disk queue.json document.
type Queue struct {
	NextID int                        `json:"next_id"`
	Items  []*Item                    `json:"items"`
	Nudged string                     `json:"nudged"`
	Extra  map[string]json.RawMessage `json:"-"`
}

func (q *Queue) UnmarshalJSON(data []byte) error {
	type plain Queue
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*q = Queue(p)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key, value := range raw {
		switch key {
		case "next_id", "items", "nudged":
		default:
			if q.Extra == nil {
				q.Extra = make(map[string]json.RawMessage)
			}
			q.Extra[key] = value
		}
	}
	return nil
}

func (q Queue) MarshalJSON() ([]byte, error) {
	type plain Queue
	p := plain(q)
	if p.Items == nil {
		p.Items = []*Item{}
	}
	data, err := json.Marshal(p)
	if err != nil || len(q.Extra) == 0 {
		return data, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for key, value := range q.Extra {
		merged[key] = value
	}
	return json.Marshal(merged)
}

func loadQueue(c config) (*Queue, error) {
	data, err := os.ReadFile(c.Queue)
	if errors.Is(err, os.ErrNotExist) {
		return &Queue{NextID: 1, Items: []*Item{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var q Queue
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("queue.json: %w", err)
	}
	if q.Items == nil {
		q.Items = []*Item{}
	}
	return &q, nil
}

func saveQueue(c config, q *Queue) error {
	data, err := json.MarshalIndent(q, "", " ")
	if err != nil {
		return err
	}
	return atomicWrite(c.Queue, append(data, '\n'), 0o644)
}

// atomicWrite replaces path via a temporary file in the same directory.
// New files that may contain credentials use 0600; queue and Markdown files
// are world-readable like the Python implementation.
func atomicWrite(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := stringsTrimExt(path) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func stringsTrimExt(path string) string {
	return path[:len(path)-len(filepath.Ext(path))]
}

func withQueueLock(c config, fn func() error) error {
	if err := os.MkdirAll(c.Data, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(c.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func tryRunLock(c config, profile string) (*os.File, error) {
	if err := os.MkdirAll(c.Data, 0o755); err != nil {
		return nil, err
	}
	name := "run.lock"
	if profile != "" {
		name = "run-" + profile + ".lock"
	}
	f, err := os.OpenFile(filepath.Join(c.Data, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func findItem(q *Queue, id int) (*Item, error) {
	for _, it := range q.Items {
		if it.ID == id {
			return it, nil
		}
	}
	return nil, fmt.Errorf("#%d はありません", id)
}

func updateItem(c config, it *Item) error {
	it.Updated = nowISO()
	return withQueueLock(c, func() error {
		q, err := loadQueue(c)
		if err != nil {
			return err
		}
		found := false
		for i, other := range q.Items {
			if other.ID == it.ID {
				q.Items[i] = it
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("#%d はありません", it.ID)
		}
		return saveQueue(c, q)
	})
}

func itemDir(c config, it *Item) (string, error) {
	dir := filepath.Join(c.Data, "items", fmt.Sprintf("%d", it.ID))
	return dir, os.MkdirAll(dir, 0o755)
}

func newItem(q *Queue, topic, mode string, questions, tags []string, notebook *string) *Item {
	it := &Item{
		ID:        q.NextID,
		Topic:     topic,
		State:     statePending,
		Mode:      mode,
		Questions: append([]string(nil), questions...),
		Tags:      append([]string(nil), tags...),
		Notebook:  notebook,
		Added:     nowISO(),
		Updated:   nowISO(),
	}
	q.Items = append(q.Items, it)
	q.NextID++
	return it
}
