package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOMDbReturnsIMDbAndRottenTomatoesRatings(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("apikey") != "test-key" || r.URL.Query().Get("t") != "The Matrix" || r.URL.Query().Get("y") != "1999" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"Response":"True","imdbRating":"8.7","Ratings":[{"Source":"Internet Movie Database","Value":"8.7/10"},{"Source":"Rotten Tomatoes","Value":"83%"}]}`))
	}))
	defer server.Close()

	provider, err := NewOMDb(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ratings, err := provider.Ratings(context.Background(), "The Matrix", 1999)
		if err != nil {
			t.Fatal(err)
		}
		if ratings.IMDb == nil || *ratings.IMDb != 8.7 || ratings.RottenTomatoes == nil || *ratings.RottenTomatoes != 83 {
			t.Fatalf("ratings = %+v", ratings)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want cached single request", requests.Load())
	}
}

func TestOMDbLooksUpRatingsByIMDbID(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("i") != "tt0241527" || r.URL.Query().Get("t") != "" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"Response":"True","imdbRating":"7.7","Ratings":[{"Source":"Rotten Tomatoes","Value":"80%"}]}`))
	}))
	defer server.Close()

	provider, err := NewOMDb(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ratings, err := provider.RatingsByIMDbID(t.Context(), "tt0241527")
		if err != nil {
			t.Fatal(err)
		}
		if ratings.IMDb == nil || *ratings.IMDb != 7.7 || ratings.RottenTomatoes == nil || *ratings.RottenTomatoes != 80 {
			t.Fatalf("ratings = %+v", ratings)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want cached single request", requests.Load())
	}
}

func TestOMDbReturnsServiceErrorsWithoutExposingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Invalid API key!"}`))
	}))
	defer server.Close()

	provider, err := NewOMDb(server.URL, "private-test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Ratings(context.Background(), "The Matrix", 1999)
	if err == nil || err.Error() != "OMDb: Invalid API key!" {
		t.Fatalf("error = %v", err)
	}
}
