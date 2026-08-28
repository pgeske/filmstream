package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/history"
	"github.com/pgeske/filmstream/internal/metadata"
)

type generatorTestModel struct {
	content     string
	system      string
	user        string
	calls       atomic.Int32
	completeErr error
}

func (m *generatorTestModel) Complete(_ context.Context, system, user string) (string, error) {
	m.calls.Add(1)
	m.system = system
	m.user = user
	return m.content, m.completeErr
}

type generatorTestHistory struct {
	entries []history.Entry
	err     error
}

func (h generatorTestHistory) List() ([]history.Entry, error) {
	return append([]history.Entry(nil), h.entries...), h.err
}

type generatorTestMetadata struct {
	search func(string) ([]metadata.Movie, error)
	calls  atomic.Int32
	active atomic.Int32
	peak   atomic.Int32
}

func (m *generatorTestMetadata) Search(_ context.Context, title string) ([]metadata.Movie, error) {
	m.calls.Add(1)
	active := m.active.Add(1)
	for {
		peak := m.peak.Load()
		if active <= peak || m.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	defer m.active.Add(-1)
	return m.search(title)
}

func TestGeneratorUsesDeterministicTasteSignalsAndExcludesWatchedTitles(t *testing.T) {
	now := time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC)
	model := &generatorTestModel{content: `{"candidates":[
		{"title":"Watched Film","year":2001,"media_type":"movie"},
		{"title":"Fresh Show","year":2019,"media_type":"show"},
		{"title":"Fresh Show Alias","year":2019,"media_type":"show"},
		{"title":"Fresh Film","year":2020,"media_type":"movie"}
	]}`}
	provider := &generatorTestMetadata{search: func(title string) ([]metadata.Movie, error) {
		switch title {
		case "Watched Film":
			return []metadata.Movie{{ID: "tmdb:watched", MediaType: metadata.MediaTypeMovie, Title: title, Year: 2001}}, nil
		case "Fresh Show", "Fresh Show Alias":
			return []metadata.Movie{{ID: "tmdb-tv:fresh", MediaType: metadata.MediaTypeShow, Title: "Fresh Show", Year: 2019}}, nil
		case "Fresh Film":
			return []metadata.Movie{{ID: "tmdb:fresh", MediaType: metadata.MediaTypeMovie, Title: title, Year: 2020}}, nil
		default:
			return nil, nil
		}
	}}
	histories := generatorTestHistory{entries: []history.Entry{
		{
			ID: "episode-1", MediaID: "episode:1", MediaType: "show", Title: "Episode One",
			SeriesID: "tmdb-tv:watched", SeriesTitle: "Watched Series", SeasonNumber: 1, EpisodeNumber: 1,
			Completed: true, PositionSeconds: 1800, DurationSeconds: 1800, UpdatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID: "movie", MediaID: "tmdb:watched", MediaType: "movie", Title: "Watched Film", Year: 2001,
			Completed: true, PositionSeconds: 7200, DurationSeconds: 7200, Genres: []string{"Drama"}, UpdatedAt: now,
		},
		{
			ID: "episode-2", MediaID: "episode:2", MediaType: "show", Title: "Episode Two",
			SeriesID: "tmdb-tv:watched", SeriesTitle: "Watched Series", SeasonNumber: 1, EpisodeNumber: 2,
			PositionSeconds: 600, DurationSeconds: 1800, Genres: []string{"Mystery", "Drama"}, UpdatedAt: now.Add(-time.Hour),
		},
	}}

	items, err := NewGenerator(model, provider, histories).Generate(t.Context(), "  thoughtful mysteries  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "tmdb-tv:fresh" || items[1].ID != "tmdb:fresh" {
		t.Fatalf("items = %+v", items)
	}
	if provider.calls.Load() != 4 {
		t.Fatalf("metadata calls = %d", provider.calls.Load())
	}

	var input modelInput
	if err := json.Unmarshal([]byte(model.user), &input); err != nil {
		t.Fatal(err)
	}
	if input.TastePrompt != "thoughtful mysteries" {
		t.Fatalf("taste prompt = %q", input.TastePrompt)
	}
	if len(input.ContinueWatching) != 1 || input.ContinueWatching[0].Title != "Watched Series" ||
		input.ContinueWatching[0].Status != "in_progress" {
		t.Fatalf("continue watching = %+v", input.ContinueWatching)
	}
	if len(input.RecentHistory) != 2 || input.RecentHistory[0].Title != "Watched Film" ||
		input.RecentHistory[1].Title != "Watched Series" {
		t.Fatalf("recent history = %+v", input.RecentHistory)
	}
}

func TestGeneratorBoundsMetadataLookupsAndOutput(t *testing.T) {
	candidates := make([]modelCandidate, 0, maxModelCandidates+5)
	for index := 0; index < maxModelCandidates+5; index++ {
		candidates = append(candidates, modelCandidate{
			Title: fmt.Sprintf("Title %02d", index), Year: 2000, MediaType: metadata.MediaTypeMovie,
		})
	}
	content, err := json.Marshal(struct {
		Candidates []modelCandidate `json:"candidates"`
	}{Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	model := &generatorTestModel{content: string(content)}
	provider := &generatorTestMetadata{search: func(title string) ([]metadata.Movie, error) {
		time.Sleep(2 * time.Millisecond)
		return []metadata.Movie{{ID: "tmdb:" + title, MediaType: metadata.MediaTypeMovie, Title: title, Year: 2000}}, nil
	}}

	items, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != desiredRecommendationCount {
		t.Fatalf("items = %d", len(items))
	}
	if provider.calls.Load() != maxModelCandidates {
		t.Fatalf("metadata calls = %d", provider.calls.Load())
	}
	if provider.peak.Load() > metadataLookupConcurrency {
		t.Fatalf("peak metadata concurrency = %d", provider.peak.Load())
	}
}

func TestGeneratorRejectsPartialResultsWhenMetadataCallFails(t *testing.T) {
	model := &generatorTestModel{content: `{"candidates":[
		{"title":"Fresh Film","year":2020,"media_type":"movie"},
		{"title":"Unavailable Film","year":2021,"media_type":"movie"}
	]}`}
	provider := &generatorTestMetadata{search: func(title string) ([]metadata.Movie, error) {
		if title == "Unavailable Film" {
			return nil, errors.New("metadata unavailable")
		}
		return []metadata.Movie{{ID: "tmdb:fresh", MediaType: metadata.MediaTypeMovie, Title: title, Year: 2020}}, nil
	}}
	items, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "")
	if err == nil || items != nil {
		t.Fatalf("items = %+v, error = %v", items, err)
	}
}

func TestGeneratorRetriesInvalidJSONWithoutNetworkCalls(t *testing.T) {
	model := &generatorTestModel{content: `{"candidates":[]}`}
	provider := &generatorTestMetadata{search: func(string) ([]metadata.Movie, error) {
		t.Fatal("metadata should not be called")
		return nil, nil
	}}
	_, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "private prompt")
	if err == nil {
		t.Fatal("expected invalid candidate error")
	}
	if model.calls.Load() != 2 || provider.calls.Load() != 0 {
		t.Fatalf("model calls = %d, metadata calls = %d", model.calls.Load(), provider.calls.Load())
	}
}
