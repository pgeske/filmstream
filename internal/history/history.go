package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const resumeThresholdSeconds = 30

type Entry struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Year            int       `json:"year,omitempty"`
	PositionSeconds float64   `json:"position_seconds"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	Completed       bool      `json:"completed"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (e Entry) ResumePosition() float64 {
	if e.Completed || e.PositionSeconds < resumeThresholdSeconds {
		return 0
	}
	return e.PositionSeconds
}

func (e Entry) Progress() float64 {
	if e.DurationSeconds <= 0 {
		return 0
	}
	return max(0, min(1, e.PositionSeconds/e.DurationSeconds))
}

func (e Entry) CanContinue() bool {
	return !e.Completed && e.ResumePosition() > 0
}

type Store struct {
	path string
	mu   sync.Mutex
}

func New(stateDir string) *Store {
	return &Store{path: filepath.Join(stateDir, "history.json")}
}

func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
	return entries, nil
}

func (s *Store) Upsert(title string, year int) (Entry, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Entry{}, errors.New("history title cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return Entry{}, err
	}
	id := entryID(title, year)
	now := time.Now().UTC()
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Title = title
			entries[i].Year = year
			entries[i].UpdatedAt = now
			if err := s.write(entries); err != nil {
				return Entry{}, err
			}
			return entries[i], nil
		}
	}
	entry := Entry{ID: id, Title: title, Year: year, UpdatedAt: now}
	entries = append(entries, entry)
	if err := s.write(entries); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (s *Store) UpdateProgress(id string, position, duration float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID != id {
			continue
		}
		position = max(0, position)
		duration = max(0, duration)
		if duration > 0 {
			position = min(position, duration)
		}
		entries[i].PositionSeconds = position
		if duration > 0 {
			entries[i].DurationSeconds = duration
		}
		entries[i].Completed = entries[i].DurationSeconds > 0 && entries[i].Progress() >= 0.9
		entries[i].UpdatedAt = time.Now().UTC()
		return s.write(entries)
	}
	return fmt.Errorf("history entry %q not found", id)
}

func (s *Store) MarkUnwatched(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID != id {
			continue
		}
		entries[i].PositionSeconds = 0
		entries[i].Completed = false
		entries[i].UpdatedAt = time.Now().UTC()
		return s.write(entries)
	}
	return fmt.Errorf("history entry %q not found", id)
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read()
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i].ID == id {
			entries = append(entries[:i], entries[i+1:]...)
			return s.write(entries)
		}
	}
	return nil
}

func (s *Store) read() ([]Entry, error) {
	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read watch history: %w", err)
	}
	var payload struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil {
		return nil, fmt.Errorf("parse watch history: %w", err)
	}
	return payload.Entries, nil
}

func (s *Store) write(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	payload := struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}{Version: 1, Entries: entries}
	contents, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".history-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace watch history: %w", err)
	}
	return nil
}

func entryID(title string, year int) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(title), " "))
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", normalized, year)))
	return hex.EncodeToString(sum[:8])
}
