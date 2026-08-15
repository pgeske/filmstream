package metadata

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTMDBSearchReturnsMovieArtwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.URL.Query().Get("query"); got != "Blade Runner 2049" {
			t.Fatalf("query = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":335984,"title":"Blade Runner 2049","original_title":"Blade Runner 2049","release_date":"2017-10-04","overview":"A young blade runner uncovers a secret.","poster_path":"/poster.jpg","backdrop_path":"/backdrop.jpg","vote_average":7.6}]}`))
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	movies, err := provider.Search(t.Context(), "Blade Runner 2049")
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 1 {
		t.Fatalf("movies = %+v", movies)
	}
	movie := movies[0]
	if movie.ID != "tmdb:335984" || movie.Year != 2017 {
		t.Fatalf("movie = %+v", movie)
	}
	if movie.PosterURL != posterBaseURL+"/poster.jpg" || movie.BackdropURL != backdropBaseURL+"/backdrop.jpg" {
		t.Fatalf("artwork = %q, %q", movie.PosterURL, movie.BackdropURL)
	}
}

func TestTMDBReturnsCachedIMDbID(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/movie/671/external_ids" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"imdb_id":"tt0241527"}`))
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		imdbID, err := provider.IMDbID(t.Context(), "tmdb:671")
		if err != nil {
			t.Fatal(err)
		}
		if imdbID != "tt0241527" {
			t.Fatalf("IMDb ID = %q", imdbID)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want cached single request", requests)
	}
}

func TestTMDBDiscoveryReturnsHomeReleasedPopularAndTopRatedMovies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		if r.URL.Path != "/discover/movie" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("language") != "en-US" || query.Get("with_release_type") != "4|5|6" || query.Get("release_date.lte") == "" {
			t.Fatalf("query = %v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		switch query.Get("sort_by") {
		case "popularity.desc":
			_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Popular Movie","release_date":"2026-07-01","poster_path":"/popular.jpg"},{"id":2,"title":"Adult Movie","adult":true}]}`))
		case "vote_average.desc":
			if query.Get("vote_count.gte") != "1000" {
				t.Fatalf("vote count = %q", query.Get("vote_count.gte"))
			}
			_, _ = w.Write([]byte(`{"results":[{"id":3,"title":"Top Rated Movie","release_date":"1972-03-14","vote_average":8.7}]}`))
		default:
			http.Error(w, "unexpected sort", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	popular, err := provider.Discover(t.Context(), CollectionPopular)
	if err != nil {
		t.Fatal(err)
	}
	topRated, err := provider.Discover(t.Context(), CollectionTopRated)
	if err != nil {
		t.Fatal(err)
	}
	if len(popular) != 1 || popular[0].Title != "Popular Movie" || popular[0].Year != 2026 {
		t.Fatalf("popular = %+v", popular)
	}
	if len(topRated) != 1 || topRated[0].Title != "Top Rated Movie" || topRated[0].VoteAverage != 8.7 {
		t.Fatalf("top rated = %+v", topRated)
	}
}

func TestTMDBSearchRejectsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status_message":"Invalid API key"}`))
	}))
	defer server.Close()
	provider, err := NewTMDB(server.URL, "bad-token", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Search(t.Context(), "Sintel"); err == nil {
		t.Fatal("expected TMDB error")
	}
}
