package recommendations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/metadata"
)

type serviceGeneratorFunc func(context.Context, string) ([]metadata.Movie, error)

func (f serviceGeneratorFunc) Generate(ctx context.Context, prompt string) ([]metadata.Movie, error) {
	return f(ctx, prompt)
}

func TestServiceAutomaticRefreshUsesDailyTTLAndDeduplicates(t *testing.T) {
	now := time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	var calls atomic.Int32
	generator := serviceGeneratorFunc(func(ctx context.Context, _ string) ([]metadata.Movie, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return []metadata.Movie{{ID: "tmdb:1", MediaType: metadata.MediaTypeMovie, Title: "Fresh"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	service := NewService(NewStore(t.TempDir()), generator, nil, ServiceOptions{Now: func() time.Time { return now }})

	response := service.Refresh(RefreshAutomatic)
	if !response.Refreshing || response.Items == nil {
		t.Fatalf("response = %+v", response)
	}
	<-started
	service.Refresh(RefreshAutomatic)
	service.Refresh(RefreshManual)
	if calls.Load() != 1 {
		t.Fatalf("overlapping calls = %d", calls.Load())
	}
	release <- struct{}{}
	waitForRefresh(t, service)

	now = now.Add(23 * time.Hour)
	service.Refresh(RefreshAutomatic)
	if calls.Load() != 1 {
		t.Fatalf("calls before TTL = %d", calls.Load())
	}
	now = now.Add(2 * time.Hour)
	response = service.Refresh(RefreshAutomatic)
	if !response.Refreshing {
		t.Fatalf("stale response = %+v", response)
	}
	<-started
	if calls.Load() != 2 {
		t.Fatalf("calls after TTL = %d", calls.Load())
	}
	release <- struct{}{}
	waitForRefresh(t, service)
}

func TestServiceFailedRefreshRetainsLastGoodListAndThrottlesRetries(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	generatedAt := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	if err := store.save(persistedState{
		GeneratedAt: generatedAt,
		Prompt:      "quiet science fiction",
		Items: []metadata.Movie{
			{ID: "tmdb-tv:old", MediaType: metadata.MediaTypeShow, Title: "Last Good Show"},
			{ID: "tmdb:old", MediaType: metadata.MediaTypeMovie, Title: "Last Good Movie"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	now := generatedAt.Add(48 * time.Hour)
	var calls atomic.Int32
	generator := serviceGeneratorFunc(func(context.Context, string) ([]metadata.Movie, error) {
		calls.Add(1)
		return []metadata.Movie{{
			ID: "tmdb:new", MediaType: metadata.MediaTypeMovie, Title: "Lopsided Partial Result",
		}}, errors.New("private show metadata outage")
	})
	service := NewService(store, generator, nil, ServiceOptions{Now: func() time.Time { return now }})

	service.Refresh(RefreshAutomatic)
	waitForRefresh(t, service)
	response := service.Snapshot()
	if response.GeneratedAt == nil || !response.GeneratedAt.Equal(generatedAt) ||
		len(response.Items) != 2 || response.Items[0].Title != "Last Good Show" ||
		response.Items[1].Title != "Last Good Movie" {
		t.Fatalf("response after failure = %+v", response)
	}
	service.Refresh(RefreshAutomatic)
	time.Sleep(10 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("failed automatic refresh retried early: %d calls", calls.Load())
	}

	reloaded, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Items) != 2 || reloaded.Items[0].Title != "Last Good Show" ||
		reloaded.Items[1].Title != "Last Good Movie" || reloaded.LastAttemptAt.IsZero() {
		t.Fatalf("persisted state = %+v", reloaded)
	}
}

func TestPromptChangePersistsAndRefreshesLatestRevision(t *testing.T) {
	store := NewStore(t.TempDir())
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	generator := serviceGeneratorFunc(func(ctx context.Context, prompt string) ([]metadata.Movie, error) {
		started <- prompt
		select {
		case <-release:
			return []metadata.Movie{{ID: "tmdb:" + prompt, MediaType: metadata.MediaTypeMovie, Title: prompt}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	service := NewService(store, generator, nil, ServiceOptions{})

	response, err := service.SetPrompt("  first preference  ")
	if err != nil {
		t.Fatal(err)
	}
	if response.Prompt != "first preference" || !response.Refreshing {
		t.Fatalf("first response = %+v", response)
	}
	if prompt := <-started; prompt != "first preference" {
		t.Fatalf("first generated prompt = %q", prompt)
	}
	response, err = service.SetPrompt("second preference")
	if err != nil {
		t.Fatal(err)
	}
	if response.Prompt != "second preference" || !response.Refreshing {
		t.Fatalf("second response = %+v", response)
	}
	persisted, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Prompt != "second preference" || !persisted.PromptRefreshPending {
		t.Fatalf("state while first refresh runs = %+v", persisted)
	}

	release <- struct{}{}
	if prompt := <-started; prompt != "second preference" {
		t.Fatalf("follow-up generated prompt = %q", prompt)
	}
	release <- struct{}{}
	waitForRefresh(t, service)
	response = service.Snapshot()
	if response.Prompt != "second preference" || len(response.Items) != 1 || response.Items[0].Title != "second preference" {
		t.Fatalf("final response = %+v", response)
	}

	restarted := NewService(store, nil, nil, ServiceOptions{})
	restored := restarted.Snapshot()
	if restored.Prompt != "second preference" || len(restored.Items) != 1 {
		t.Fatalf("restored response = %+v", restored)
	}
	cleared, err := restarted.SetPrompt("   ")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Prompt != "" {
		t.Fatalf("cleared prompt = %q", cleared.Prompt)
	}
}

func TestStoreUsesPrivateAtomicFileAndCorruptionDoesNotBreakService(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.save(persistedState{Prompt: "animation", Items: []metadata.Movie{}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "recommendations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("recommendation state mode = %o", info.Mode().Perm())
	}
	if err := os.WriteFile(store.path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("expected corrupt state error")
	}
	service := NewService(store, serviceGeneratorFunc(func(context.Context, string) ([]metadata.Movie, error) {
		return nil, errors.New("generation failed")
	}), nil, ServiceOptions{})
	response := service.Refresh(RefreshAutomatic)
	if response.Items == nil || len(response.Items) != 0 || response.Prompt != "" {
		t.Fatalf("response from corrupt state = %+v", response)
	}
	waitForRefresh(t, service)
	contents, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "{not-json" {
		t.Fatalf("corrupt state was unexpectedly replaced: %q", contents)
	}
}

func TestPromptFileOverridesStoredPromptAndRefreshesOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "taste.md")
	if err := os.WriteFile(notePath, []byte("hopeful science fiction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(t.TempDir())
	if err := store.save(persistedState{Prompt: "stored prompt"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC)
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	generator := serviceGeneratorFunc(func(_ context.Context, prompt string) ([]metadata.Movie, error) {
		started <- prompt
		<-release
		return []metadata.Movie{{ID: "tmdb:" + prompt, MediaType: metadata.MediaTypeMovie, Title: prompt}}, nil
	})
	service := NewService(store, generator, nil, ServiceOptions{
		PromptFile: notePath, Now: func() time.Time { return now },
	})

	service.Start(context.Background())
	if prompt := <-started; prompt != "hopeful science fiction" {
		t.Fatalf("generated prompt = %q", prompt)
	}
	// The stored prompt stays the reported API prompt; the note only drives
	// generation.
	if response := service.Snapshot(); response.Prompt != "stored prompt" {
		t.Fatalf("stored prompt changed: %+v", response)
	}
	release <- struct{}{}
	waitForRefresh(t, service)

	now = now.Add(time.Hour)
	service.Refresh(RefreshAutomatic)
	select {
	case prompt := <-started:
		t.Fatalf("unexpected regeneration for unchanged note: %q", prompt)
	default:
	}

	if err := os.WriteFile(notePath, []byte("cozy mysteries"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(notePath, future, future); err != nil {
		t.Fatal(err)
	}
	response := service.Refresh(RefreshAutomatic)
	if !response.Refreshing {
		t.Fatalf("edited note did not schedule a refresh: %+v", response)
	}
	if prompt := <-started; prompt != "cozy mysteries" {
		t.Fatalf("generated prompt after edit = %q", prompt)
	}
	release <- struct{}{}
	waitForRefresh(t, service)
}

func TestPromptFileMissingOrEmptyFallsBackToStoredPrompt(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyPath, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, promptFile := range []string{filepath.Join(dir, "missing.md"), emptyPath} {
		started := make(chan string, 2)
		generator := serviceGeneratorFunc(func(_ context.Context, prompt string) ([]metadata.Movie, error) {
			started <- prompt
			return []metadata.Movie{{ID: "tmdb:" + prompt, MediaType: metadata.MediaTypeMovie, Title: prompt}}, nil
		})
		service := NewService(NewStore(t.TempDir()), generator, nil, ServiceOptions{PromptFile: promptFile})

		response, err := service.SetPrompt("stored prompt")
		if err != nil {
			t.Fatal(err)
		}
		if !response.Refreshing {
			t.Fatalf("PUT with %q did not refresh: %+v", promptFile, response)
		}
		if prompt := <-started; prompt != "stored prompt" {
			t.Fatalf("generated prompt = %q", prompt)
		}
		waitForRefresh(t, service)
	}
}

func TestPromptFileUpdateStoresPromptWithoutRegeneration(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "taste.md")
	if err := os.WriteFile(notePath, []byte("note prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	generator := serviceGeneratorFunc(func(_ context.Context, prompt string) ([]metadata.Movie, error) {
		started <- prompt
		return []metadata.Movie{{ID: "tmdb:" + prompt, MediaType: metadata.MediaTypeMovie, Title: prompt}}, nil
	})
	service := NewService(NewStore(t.TempDir()), generator, nil, ServiceOptions{PromptFile: notePath})

	service.Start(context.Background())
	if prompt := <-started; prompt != "note prompt" {
		t.Fatalf("generated prompt = %q", prompt)
	}
	waitForRefresh(t, service)

	// With an authoritative note, PUT keeps the stored prompt without spending
	// a model refresh on it.
	response, err := service.SetPrompt("stored fallback prompt")
	if err != nil {
		t.Fatal(err)
	}
	if response.Refreshing || response.Prompt != "stored fallback prompt" {
		t.Fatalf("PUT response = %+v", response)
	}
	select {
	case prompt := <-started:
		t.Fatalf("unexpected regeneration: %q", prompt)
	default:
	}

	// Removing the note falls back to the stored prompt on the next refresh.
	if err := os.Remove(notePath); err != nil {
		t.Fatal(err)
	}
	if response := service.Refresh(RefreshAutomatic); !response.Refreshing {
		t.Fatalf("missing note did not schedule a refresh: %+v", response)
	}
	if prompt := <-started; prompt != "stored fallback prompt" {
		t.Fatalf("fallback generated prompt = %q", prompt)
	}
	waitForRefresh(t, service)
}

func TestPromptFileEditedWhileDownRefreshesOnNextGet(t *testing.T) {
	notePath := filepath.Join(t.TempDir(), "taste.md")
	if err := os.WriteFile(notePath, []byte("new note prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC)
	store := NewStore(t.TempDir())
	if err := store.save(persistedState{
		GeneratedAt:         generatedAt,
		GeneratedPromptHash: promptHash("old note prompt"),
		Prompt:              "old note prompt",
		Items:               []metadata.Movie{{ID: "tmdb:old", MediaType: metadata.MediaTypeMovie, Title: "Old"}},
	}); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	generator := serviceGeneratorFunc(func(_ context.Context, prompt string) ([]metadata.Movie, error) {
		started <- prompt
		return []metadata.Movie{{ID: "tmdb:new", MediaType: metadata.MediaTypeMovie, Title: "New"}}, nil
	})
	service := NewService(store, generator, nil, ServiceOptions{
		PromptFile: notePath,
		Now:        func() time.Time { return generatedAt.Add(time.Hour) },
	})

	if response := service.Refresh(RefreshAutomatic); !response.Refreshing || len(response.Items) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if prompt := <-started; prompt != "new note prompt" {
		t.Fatalf("generated prompt = %q", prompt)
	}
	waitForRefresh(t, service)
}

func waitForRefresh(t *testing.T, service *Service) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for service.Snapshot().Refreshing {
		if time.Now().After(deadline) {
			t.Fatal("recommendation refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
