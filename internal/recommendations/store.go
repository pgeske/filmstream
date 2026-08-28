package recommendations

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pgeske/filmstream/internal/metadata"
)

const recommendationStateVersion = 1

type persistedState struct {
	Version              int              `json:"version"`
	GeneratedAt          time.Time        `json:"generated_at,omitempty"`
	GeneratedPromptHash  string           `json:"generated_prompt_hash,omitempty"`
	LastAttemptAt        time.Time        `json:"last_attempt_at,omitempty"`
	Prompt               string           `json:"prompt"`
	PromptRefreshPending bool             `json:"prompt_refresh_pending,omitempty"`
	Items                []metadata.Movie `json:"items"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(stateDir string) *Store {
	return &Store{path: filepath.Join(stateDir, "recommendations.json")}
}

func (s *Store) load() (persistedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	contents, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return persistedState{Version: recommendationStateVersion, Items: []metadata.Movie{}}, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read recommendations: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(contents, &state); err != nil {
		return persistedState{}, fmt.Errorf("parse recommendations: %w", err)
	}
	if state.Version != recommendationStateVersion {
		return persistedState{}, fmt.Errorf("unsupported recommendations version %d", state.Version)
	}
	if state.Items == nil {
		state.Items = []metadata.Movie{}
	}
	return state, nil
}

func (s *Store) save(state persistedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state.Version = recommendationStateVersion
	if state.Items == nil {
		state.Items = []metadata.Movie{}
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recommendations: %w", err)
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create recommendation state directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".recommendations-*.json")
	if err != nil {
		return fmt.Errorf("create temporary recommendations: %w", err)
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
		return fmt.Errorf("replace recommendations: %w", err)
	}
	return nil
}
