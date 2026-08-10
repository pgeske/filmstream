package metadata

import (
	"context"
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
	if _, err := provider.Search(context.Background(), "Sintel"); err == nil {
		t.Fatal("expected TMDB error")
	}
}
