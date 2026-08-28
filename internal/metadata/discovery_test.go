package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPopularDiscoveryUsesIMDbQualityAndKeepsMediaBalanced(t *testing.T) {
	items := make([]ratedDiscoveryItem, 0, 21)
	for index := range 10 {
		rating := 7.2 + float64(index)/20
		votes := 100000 + index*10000
		items = append(items, ratedDiscoveryItem{
			item: Movie{
				ID: fmt.Sprintf("tmdb:%d", index+1), MediaType: MediaTypeMovie,
				Title: fmt.Sprintf("Movie %d", index+1), Year: 2025,
				Popularity: float64(500 - index*10), VoteAverage: 7, VoteCount: 5000,
			},
			ratings: MovieRatings{IMDb: &rating, IMDbVotes: &votes},
		})
	}
	for index := range 10 {
		rating := 7.5
		votes := 200000
		title := fmt.Sprintf("Show %d", index+1)
		popularity := float64(400 - index*10)
		if index == 0 {
			title = "Recognizable Show"
			rating = 8.7
			votes = 1500000
			popularity = 600
		}
		items = append(items, ratedDiscoveryItem{
			item: Movie{
				ID: fmt.Sprintf("tmdb-tv:%d", index+1), MediaType: MediaTypeShow,
				Title: title, Year: 2025, Popularity: popularity, VoteAverage: 7, VoteCount: 2000,
			},
			ratings: MovieRatings{IMDb: &rating, IMDbVotes: &votes},
		})
	}
	lowRating, lowVotes := 5.0, 50000
	items = append(items, ratedDiscoveryItem{
		item: Movie{
			ID: "tmdb-tv:99", MediaType: MediaTypeShow, Title: "Paradise Hotel",
			Year: 2026, Popularity: 1000, VoteAverage: 9.5, VoteCount: 5000,
		},
		ratings: MovieRatings{IMDb: &lowRating, IMDbVotes: &lowVotes},
	})

	ranked := rankDiscoveryCandidates(items, CollectionPopular, 2026)
	if len(ranked) != 20 {
		t.Fatalf("ranked items = %d, want 20", len(ranked))
	}
	movies, shows := 0, 0
	var rankedShows []string
	for _, item := range ranked {
		switch item.MediaType {
		case MediaTypeMovie:
			movies++
		case MediaTypeShow:
			shows++
			rankedShows = append(rankedShows, item.Title)
		}
		if item.Title == "Paradise Hotel" {
			t.Fatalf("low-rated popular item was retained: %+v", item)
		}
	}
	if movies != 10 || shows != 10 {
		t.Fatalf("media balance = %d movies, %d shows", movies, shows)
	}
	if len(rankedShows) == 0 || rankedShows[0] != "Recognizable Show" {
		t.Fatalf("ranked shows = %v", rankedShows)
	}
}

func TestTopRatedDiscoveryWeightsIMDbRatingByVoteCount(t *testing.T) {
	items := []ratedDiscoveryItem{
		ratedDiscoveryTestItem("Tiny Audience", 9.8, 200),
		ratedDiscoveryTestItem("Established Favorite", 9.1, 1500000),
		ratedDiscoveryTestItem("Broadly Rated", 8.8, 3000000),
	}

	ranked := rankDiscoveryCandidates(items, CollectionTopRated, 2026)
	got := []string{ranked[0].Title, ranked[1].Title, ranked[2].Title}
	want := []string{"Established Favorite", "Broadly Rated", "Tiny Audience"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ranking = %v, want %v", got, want)
		}
	}
}

func TestTMDBDiscoveryFallsBackAndStopsAfterRatingsBecomeUnavailable(t *testing.T) {
	var mu sync.Mutex
	pages := map[string]map[int]bool{
		"/discover/movie": {},
		"/discover/tv":    {},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/discover/movie":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			mu.Lock()
			pages[r.URL.Path][page] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Fallback Movie","release_date":"2025-01-01","popularity":90,"vote_average":8.1,"vote_count":10000}]}`))
		case "/discover/tv":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			mu.Lock()
			pages[r.URL.Path][page] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"results":[{"id":2,"name":"Fallback Show","first_air_date":"2024-01-01","popularity":80,"vote_average":8.0,"vote_count":4000}]}`))
		case "/tv/2":
			_, _ = w.Write([]byte(`{"id":2,"name":"Fallback Show","first_air_date":"2024-01-01","number_of_seasons":2,"seasons":[{"season_number":1,"name":"Season 1","episode_count":8}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ratings := &unavailableMediaRatings{}
	items, err := provider.DiscoverWithRatings(t.Context(), CollectionPopular, ratings)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "Fallback Movie" || items[1].Title != "Fallback Show" {
		t.Fatalf("fallback items = %+v", items)
	}
	if calls := ratings.calls.Load(); calls < 1 || calls > discoveryExternalWorkers {
		t.Fatalf("ratings calls = %d, want 1..%d after provider failure", calls, discoveryExternalWorkers)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, fetchedPages := range pages {
		for page := 1; page <= discoveryPageCount; page++ {
			if !fetchedPages[page] {
				t.Errorf("%s did not fetch candidate page %d", path, page)
			}
		}
	}
}

func TestDiscoveryRatingsCallsAreBoundedAndCached(t *testing.T) {
	var requests atomic.Int32
	var active atomic.Int32
	var maximumActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		for {
			maximum := maximumActive.Load()
			if current <= maximum || maximumActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		defer active.Add(-1)
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"Response":"True","imdbRating":"8.2","imdbVotes":"123,456"}`))
	}))
	defer server.Close()

	provider, err := NewOMDb(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]Movie, 0, 200)
	for index := range 100 {
		candidates = append(candidates,
			Movie{ID: fmt.Sprintf("tmdb:%d", index+1), MediaType: MediaTypeMovie, Title: fmt.Sprintf("Movie %d", index), Year: 2025, Popularity: float64(1000 - index)},
			Movie{ID: fmt.Sprintf("tmdb-tv:%d", index+1), MediaType: MediaTypeShow, Title: fmt.Sprintf("Show %d", index), Year: 2025, Popularity: float64(1000 - index)},
		)
	}
	shortlist := shortlistDiscoveryCandidates(candidates, CollectionPopular)
	if len(shortlist) != 2*discoveryCandidateLimitPerType {
		t.Fatalf("shortlist = %d", len(shortlist))
	}

	enrich := func() []ratedDiscoveryItem {
		items := make([]ratedDiscoveryItem, len(shortlist))
		for index, item := range shortlist {
			items[index].item = item
		}
		enrichDiscoveryRatings(t.Context(), items, provider)
		return items
	}
	first := enrich()
	second := enrich()
	if requests.Load() != int32(len(shortlist)) {
		t.Fatalf("OMDb requests = %d, want cached %d", requests.Load(), len(shortlist))
	}
	if maximumActive.Load() > discoveryExternalWorkers {
		t.Fatalf("maximum concurrent OMDb requests = %d, want at most %d", maximumActive.Load(), discoveryExternalWorkers)
	}
	for _, batch := range [][]ratedDiscoveryItem{first, second} {
		for _, item := range batch {
			if item.ratings.IMDb == nil || item.ratings.IMDbVotes == nil || *item.ratings.IMDbVotes != 123456 {
				t.Fatalf("ratings = %+v", item.ratings)
			}
		}
	}
}

func ratedDiscoveryTestItem(title string, rating float64, votes int) ratedDiscoveryItem {
	return ratedDiscoveryItem{
		item: Movie{
			ID: "tmdb:" + title, MediaType: MediaTypeMovie, Title: title,
			VoteAverage: 8, VoteCount: 5000,
		},
		ratings: MovieRatings{IMDb: &rating, IMDbVotes: &votes},
	}
}

type unavailableMediaRatings struct {
	calls atomic.Int32
}

func (p *unavailableMediaRatings) RatingsForMedia(context.Context, Movie) (MovieRatings, error) {
	p.calls.Add(1)
	return MovieRatings{}, ratingsUnavailableError{message: "OMDb: Request limit reached!"}
}
