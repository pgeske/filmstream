package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		{"title":"Fresh Film","year":2020,"media_type":"movie"},
		{"title":" Fresh Film ","year":2020,"media_type":"movie"}
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
	if model.calls.Load() != 1 {
		t.Fatalf("model calls = %d, want one completion for both media types", model.calls.Load())
	}
	if !strings.Contains(model.system, "about 60 strong television show candidates and about 60 strong movie candidates") {
		t.Fatalf("system prompt does not request a per-type candidate surplus: %q", model.system)
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

func TestGeneratorCapsEachMediaTypeWithoutStarvation(t *testing.T) {
	for _, test := range []struct {
		name        string
		primaryType metadata.MediaType
	}{
		{name: "movie-heavy output", primaryType: metadata.MediaTypeMovie},
		{name: "show-heavy output", primaryType: metadata.MediaTypeShow},
	} {
		t.Run(test.name, func(t *testing.T) {
			primaryLabel := "Movie"
			secondaryType := metadata.MediaTypeShow
			secondaryLabel := "Show"
			if test.primaryType == metadata.MediaTypeShow {
				primaryLabel = "Show"
				secondaryType = metadata.MediaTypeMovie
				secondaryLabel = "Movie"
			}
			candidates := make([]modelCandidate, 0, maxModelCandidatesPerType*2+10)
			for index := 0; index < maxModelCandidatesPerType+10; index++ {
				candidates = append(candidates, modelCandidate{
					Title: fmt.Sprintf("%s %03d", primaryLabel, index),
					Year:  2000, MediaType: test.primaryType,
				})
			}
			for index := 0; index < maxModelCandidatesPerType; index++ {
				candidates = append(candidates, modelCandidate{
					Title: fmt.Sprintf("%s %03d", secondaryLabel, index),
					Year:  2000, MediaType: secondaryType,
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
				time.Sleep(time.Millisecond)
				mediaType := metadata.MediaTypeMovie
				if strings.HasPrefix(title, "Show ") {
					mediaType = metadata.MediaTypeShow
				}
				return []metadata.Movie{{
					ID: string(mediaType) + ":" + title, MediaType: mediaType, Title: title, Year: 2000,
				}}, nil
			}}

			items, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != desiredRecommendationsPerType*2 {
				t.Fatalf("items = %d", len(items))
			}
			for index, item := range items {
				wantType := metadata.MediaTypeShow
				wantTitle := fmt.Sprintf("Show %03d", index)
				if index >= desiredRecommendationsPerType {
					wantType = metadata.MediaTypeMovie
					wantTitle = fmt.Sprintf("Movie %03d", index-desiredRecommendationsPerType)
				}
				if item.MediaType != wantType || item.Title != wantTitle {
					t.Fatalf("item %d = %+v, want %s %q", index, item, wantType, wantTitle)
				}
			}
			if model.calls.Load() != 1 {
				t.Fatalf("model calls = %d", model.calls.Load())
			}
			if provider.calls.Load() != maxModelCandidatesPerType*2 {
				t.Fatalf("metadata calls = %d", provider.calls.Load())
			}
			if provider.peak.Load() > metadataLookupConcurrency {
				t.Fatalf("peak metadata concurrency = %d", provider.peak.Load())
			}
		})
	}
}

func TestGeneratorAllowsPartialMetadataFailuresAfterTypeCapIsFilled(t *testing.T) {
	candidates := make([]modelCandidate, 0, desiredRecommendationsPerType+2)
	for index := 0; index <= desiredRecommendationsPerType; index++ {
		candidates = append(candidates, modelCandidate{
			Title: fmt.Sprintf("Show %03d", index), Year: 2000, MediaType: metadata.MediaTypeShow,
		})
	}
	candidates = append(candidates, modelCandidate{
		Title: "Only Film", Year: 2001, MediaType: metadata.MediaTypeMovie,
	})
	content, err := json.Marshal(struct {
		Candidates []modelCandidate `json:"candidates"`
	}{Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	model := &generatorTestModel{content: string(content)}
	provider := &generatorTestMetadata{search: func(title string) ([]metadata.Movie, error) {
		if title == "Show 000" {
			return nil, errors.New("metadata unavailable")
		}
		mediaType := metadata.MediaTypeShow
		year := 2000
		if title == "Only Film" {
			mediaType = metadata.MediaTypeMovie
			year = 2001
		}
		return []metadata.Movie{{
			ID: string(mediaType) + ":" + title, MediaType: mediaType, Title: title, Year: year,
		}}, nil
	}}

	items, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != desiredRecommendationsPerType+1 || items[0].Title != "Show 001" ||
		items[desiredRecommendationsPerType].Title != "Only Film" {
		t.Fatalf("items = %+v", items)
	}
}

func TestGeneratorRejectsIncompleteMediaTypeWhenMetadataCallFails(t *testing.T) {
	model := &generatorTestModel{content: `{"candidates":[
		{"title":"Unavailable Show","year":2021,"media_type":"show"},
		{"title":"Fresh Film","year":2020,"media_type":"movie"}
	]}`}
	provider := &generatorTestMetadata{search: func(title string) ([]metadata.Movie, error) {
		if title == "Unavailable Show" {
			return nil, errors.New("metadata unavailable")
		}
		return []metadata.Movie{{ID: "tmdb:fresh", MediaType: metadata.MediaTypeMovie, Title: title, Year: 2020}}, nil
	}}
	items, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "")
	if err == nil || items != nil {
		t.Fatalf("items = %+v, error = %v", items, err)
	}
}

func TestGeneratorDoesNotRetryInvalidJSONOrCallMetadata(t *testing.T) {
	model := &generatorTestModel{content: `{"candidates":[]}`}
	provider := &generatorTestMetadata{search: func(string) ([]metadata.Movie, error) {
		t.Fatal("metadata should not be called")
		return nil, nil
	}}
	_, err := NewGenerator(model, provider, generatorTestHistory{}).Generate(t.Context(), "private prompt")
	if err == nil {
		t.Fatal("expected invalid candidate error")
	}
	if model.calls.Load() != 1 || provider.calls.Load() != 0 {
		t.Fatalf("model calls = %d, metadata calls = %d", model.calls.Load(), provider.calls.Load())
	}
}
