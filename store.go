package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Record is one line of logs/usage.jsonl.
type Record struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"ts"`
	Client     string    `json:"client"`
	Provider   string    `json:"provider"`
	API        string    `json:"api"`
	Transport  string    `json:"transport,omitempty"` // "websocket" when not plain HTTP
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Model      string    `json:"model"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
	Stream     bool      `json:"stream"`

	UsageFound       bool  `json:"usage_found"`
	InputTokens      int64 `json:"input_tokens"` // not from cache
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	TotalInputTokens int64 `json:"total_input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`

	Messages          int    `json:"messages"`
	PromptKind        string `json:"prompt_kind,omitempty"`
	Prompt            string `json:"prompt,omitempty"`
	SystemPromptID    string `json:"system_prompt_id,omitempty"`
	SystemPromptChars int    `json:"system_prompt_chars,omitempty"`
	Error             string `json:"error,omitempty"`
}

func applyUsage(rec *Record, u Usage) {
	if u.Model != "" {
		rec.Model = u.Model
	}
	rec.UsageFound = u.Found
	rec.InputTokens, rec.CacheReadTokens, rec.CacheWriteTokens = u.Input, u.CacheRead, u.CacheWrite
	rec.TotalInputTokens = u.Input + u.CacheRead + u.CacheWrite
	rec.OutputTokens, rec.ReasoningTokens = u.Output, u.Reasoning
}

type Totals struct {
	Date       string `json:"date"`
	Requests   int64  `json:"requests"`
	Input      int64  `json:"total_input_tokens"`
	CacheRead  int64  `json:"cache_read_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
	Output     int64  `json:"output_tokens"`
}

type Store struct {
	dir string

	mu     sync.Mutex
	f      *os.File
	keep   int
	recent []Record
	today  Totals
	seen   map[string]bool
	subs   map[chan struct{}]struct{}
}

func logPath(dir string) string         { return filepath.Join(dir, "logs", "usage.jsonl") }
func systemPromptDir(dir string) string { return filepath.Join(dir, "system-prompts") }
func localDate(t time.Time) string      { return t.Local().Format("2006-01-02") }

func openStore(dir string, keep int) (*Store, error) {
	for _, d := range []string{filepath.Join(dir, "logs"), systemPromptDir(dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, keep: keep, seen: map[string]bool{}, subs: map[chan struct{}]struct{}{}}
	s.today.Date = localDate(time.Now())
	err := forEachRecord(logPath(dir), func(r Record) {
		s.pushRecent(r)
		s.addTotals(r)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *Store) LogPath() string { return logPath(s.dir) }

func forEachRecord(path string, fn func(Record)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 512<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			fn(r)
		}
	}
	return sc.Err()
}

func (s *Store) pushRecent(r Record) {
	s.recent = append(s.recent, r)
	if len(s.recent) > s.keep {
		s.recent = append(s.recent[:0], s.recent[len(s.recent)-s.keep:]...)
	}
}

func (s *Store) addTotals(r Record) {
	d := localDate(r.Time)
	if d != s.today.Date {
		if d < s.today.Date {
			return
		}
		s.today = Totals{Date: d}
	}
	s.today.Requests++
	s.today.Input += r.TotalInputTokens
	s.today.CacheRead += r.CacheReadTokens
	s.today.CacheWrite += r.CacheWriteTokens
	s.today.Output += r.OutputTokens
}

func (s *Store) Add(r Record) {
	b, err := json.Marshal(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		_, _ = s.f.Write(append(b, '\n'))
	}
	s.pushRecent(r)
	s.addTotals(r)
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Snapshot returns recent records newest first, and today's totals.
func (s *Store) Snapshot() ([]Record, Totals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.recent))
	for i, r := range s.recent {
		out[len(s.recent)-1-i] = r
	}
	t := s.today
	if now := localDate(time.Now()); t.Date != now {
		t = Totals{Date: now}
	}
	return out, t
}

func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// SaveSystemPrompt stores text once under system-prompts/<id>.txt, where id
// is derived from its SHA-256. Identical prompts are stored only once.
func (s *Store) SaveSystemPrompt(text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	id := hex.EncodeToString(sum[:8])
	s.mu.Lock()
	known := s.seen[id]
	s.mu.Unlock()
	if known {
		return id, nil
	}
	path := filepath.Join(systemPromptDir(s.dir), id+".txt")
	if _, err := os.Stat(path); err != nil {
		tmp, err := os.CreateTemp(systemPromptDir(s.dir), ".tmp-*")
		if err != nil {
			return "", err
		}
		_, werr := tmp.WriteString(text)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(tmp.Name())
			return "", errors.Join(werr, cerr)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			_ = os.Remove(tmp.Name())
			return "", err
		}
	}
	s.mu.Lock()
	s.seen[id] = true
	s.mu.Unlock()
	return id, nil
}

var promptIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Store) SystemPrompt(id string) ([]byte, error) {
	if !promptIDRe.MatchString(id) {
		return nil, fs.ErrNotExist
	}
	return os.ReadFile(filepath.Join(systemPromptDir(s.dir), id+".txt"))
}

func newID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b[:])
}
