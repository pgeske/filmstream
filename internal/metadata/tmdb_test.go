package metadata

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTMDBSearchReturnsMixedMoviesAndShows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/multi":
			if got := r.URL.Query().Get("query"); got != "Blade Runner" {
				t.Errorf("query = %q", got)
			}
			_, _ = w.Write([]byte(`{"results":[
				{"id":335984,"media_type":"movie","title":"Blade Runner 2049","original_title":"Blade Runner 2049","original_language":"en","release_date":"2017-10-04","overview":"A young blade runner uncovers a secret.","poster_path":"/poster.jpg","backdrop_path":"/backdrop.jpg","vote_average":7.6,"genre_ids":[878,18]},
				{"id":1399,"media_type":"tv","name":"Game of Thrones","original_name":"Game of Thrones","original_language":"en","first_air_date":"2011-04-17","genre_ids":[10765,18]},
				{"id":1,"media_type":"person","name":"Someone"}]}`))
		case "/tv/1399":
			_, _ = w.Write([]byte(`{"id":1399,"name":"Game of Thrones","first_air_date":"2011-04-17","number_of_seasons":8,"genres":[{"name":"Sci-Fi & Fantasy"}],"seasons":[{"season_number":1,"name":"Season 1","episode_count":10}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	items, err := provider.Search(t.Context(), "Blade Runner")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	movie := items[0]
	if movie.ID != "tmdb:335984" || movie.MediaType != MediaTypeMovie || movie.Year != 2017 || movie.OriginalLanguage != "en" {
		t.Fatalf("movie = %+v", movie)
	}
	if movie.PosterURL != posterBaseURL+"/poster.jpg" || movie.BackdropURL != backdropBaseURL+"/backdrop.jpg" {
		t.Fatalf("artwork = %q, %q", movie.PosterURL, movie.BackdropURL)
	}
	if len(movie.Genres) != 2 || movie.Genres[0] != "Science Fiction" || movie.Genres[1] != "Drama" {
		t.Fatalf("genres = %v", movie.Genres)
	}
	show := items[1]
	if show.ID != "tmdb-tv:1399" || show.MediaType != MediaTypeShow || show.NumberOfSeasons != 8 {
		t.Fatalf("show = %+v", show)
	}
}

func TestTMDBReturnsCachedIMDbIDForMovieAndShow(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/movie/671/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt0241527"}`))
		case "/tv/1399/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt0944947"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewTMDB(server.URL, "test-token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		movieID, err := provider.IMDbID(t.Context(), "tmdb:671")
		if err != nil || movieID != "tt0241527" {
			t.Fatalf("movie IMDb ID = %q, %v", movieID, err)
		}
		showID, err := provider.IMDbID(t.Context(), "tmdb-tv:1399")
		if err != nil || showID != "tt0944947" {
			t.Fatalf("show IMDb ID = %q, %v", showID, err)
		}
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want one per media item", requests)
	}
}

func TestTMDBDiscoveryReturnsMixedMoviesAndShows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/discover/movie":
			query := r.URL.Query()
			if query.Get("language") != "en-US" || query.Get("with_release_type") != "4|5|6" || query.Get("release_date.lte") == "" {
				t.Errorf("movie query = %v", query)
			}
			if query.Get("sort_by") == "popularity.desc" {
				_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Popular Movie","release_date":"2026-07-01","popularity":80},{"id":2,"title":"Adult Movie","adult":true}]}`))
			} else {
				if query.Get("vote_count.gte") != "5000" {
					t.Errorf("top-rated movie query = %v", query)
				}
				_, _ = w.Write([]byte(`{"results":[{"id":3,"title":"Top Rated Movie","release_date":"1972-03-14","vote_average":8.7,"vote_count":10000}]}`))
			}
		case "/discover/tv":
			if r.URL.Query().Get("first_air_date.lte") == "" {
				t.Errorf("TV query = %v", r.URL.Query())
			}
			if r.URL.Query().Get("sort_by") == "popularity.desc" {
				_, _ = w.Write([]byte(`{"results":[{"id":10,"name":"Popular Show","first_air_date":"2020-01-01","popularity":100}]}`))
			} else {
				if r.URL.Query().Get("vote_count.gte") != "2000" {
					t.Errorf("top-rated TV query = %v", r.URL.Query())
				}
				_, _ = w.Write([]byte(`{"results":[{"id":11,"name":"Top Rated Show","first_air_date":"2019-01-01","vote_average":9.1,"vote_count":2000}]}`))
			}
		case "/tv/10":
			_, _ = w.Write([]byte(`{"id":10,"name":"Popular Show","first_air_date":"2020-01-01","number_of_seasons":3,"seasons":[{"season_number":1,"name":"Season 1","episode_count":8}]}`))
		case "/tv/11":
			_, _ = w.Write([]byte(`{"id":11,"name":"Top Rated Show","first_air_date":"2019-01-01","number_of_seasons":2,"seasons":[{"season_number":1,"name":"Season 1","episode_count":10}]}`))
		default:
			http.NotFound(w, r)
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
	if len(popular) != 2 || popular[0].Title != "Popular Movie" || popular[1].Title != "Popular Show" || popular[1].NumberOfSeasons != 3 {
		t.Fatalf("popular = %+v", popular)
	}
	if len(topRated) != 2 || topRated[0].Title != "Top Rated Movie" || topRated[1].Title != "Top Rated Show" {
		t.Fatalf("top rated = %+v", topRated)
	}
}

func TestTMDBReturnsShowSeasonsAndEpisodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tv/66732":
			_, _ = w.Write([]byte(`{"id":66732,"name":"Stranger Things","original_name":"Stranger Things","original_language":"en","first_air_date":"2016-07-15","overview":"A mystery unfolds.","poster_path":"/show.jpg","backdrop_path":"/show-bg.jpg","number_of_seasons":5,"genres":[{"name":"Drama"}],"seasons":[{"season_number":0,"name":"Specials","episode_count":2},{"season_number":1,"name":"Season 1","episode_count":8,"poster_path":"/s1.jpg"},{"season_number":2,"name":"Season 2","episode_count":9}]}`))
		case "/tv/66732/season/1":
			_, _ = w.Write([]byte(`{"name":"Season 1","season_number":1,"episodes":[{"id":1,"name":"Chapter One: The Vanishing of Will Byers","overview":"Will disappears.","air_date":"2016-07-15","still_path":"/episode.jpg","runtime":49,"season_number":1,"episode_number":1},{"id":2,"name":"A Future Chapter","air_date":"2099-01-01","season_number":1,"episode_number":2}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewTMDB(server.URL, "token", "en-US", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	show, err := provider.Show(t.Context(), "tmdb-tv:66732")
	if err != nil {
		t.Fatal(err)
	}
	if show.MediaType != MediaTypeShow || show.OriginalLanguage != "en" || len(show.Seasons) != 2 || show.Seasons[0].EpisodeCount != 8 {
		t.Fatalf("show = %+v", show)
	}
	season, err := provider.Season(t.Context(), show.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(season.Episodes) != 1 || season.Episodes[0].ID != "tmdb-tv:66732:s1:e1" || season.Episodes[0].StillURL != stillBaseURL+"/episode.jpg" {
		t.Fatalf("season = %+v", season)
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
