package metadata

import "context"

type MediaType string

const (
	MediaTypeMovie MediaType = "movie"
	MediaTypeShow  MediaType = "show"
)

type Movie struct {
	ID              string    `json:"id"`
	MediaType       MediaType `json:"media_type"`
	Title           string    `json:"title"`
	OriginalTitle   string    `json:"original_title,omitempty"`
	Year            int       `json:"year,omitempty"`
	ReleaseDate     string    `json:"release_date,omitempty"`
	Overview        string    `json:"overview,omitempty"`
	PosterURL       string    `json:"poster_url,omitempty"`
	BackdropURL     string    `json:"backdrop_url,omitempty"`
	VoteAverage     float64   `json:"vote_average,omitempty"`
	Genres          []string  `json:"genres,omitempty"`
	NumberOfSeasons int       `json:"number_of_seasons,omitempty"`

	Popularity float64 `json:"-"`
	VoteCount  int     `json:"-"`
}

type SeasonSummary struct {
	Number       int    `json:"number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	PosterURL    string `json:"poster_url,omitempty"`
	AirDate      string `json:"air_date,omitempty"`
}

type Episode struct {
	ID            string  `json:"id"`
	SeriesID      string  `json:"series_id"`
	SeriesTitle   string  `json:"series_title"`
	SeasonNumber  int     `json:"season_number"`
	EpisodeNumber int     `json:"episode_number"`
	Title         string  `json:"title"`
	Overview      string  `json:"overview,omitempty"`
	AirDate       string  `json:"air_date,omitempty"`
	StillURL      string  `json:"still_url,omitempty"`
	VoteAverage   float64 `json:"vote_average,omitempty"`
	Runtime       int     `json:"runtime_minutes,omitempty"`
}

type Show struct {
	Movie
	Seasons []SeasonSummary `json:"seasons"`
}

type Season struct {
	SeriesID    string    `json:"series_id"`
	SeriesTitle string    `json:"series_title"`
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	Overview    string    `json:"overview,omitempty"`
	PosterURL   string    `json:"poster_url,omitempty"`
	Episodes    []Episode `json:"episodes"`
}

type MovieRatings struct {
	IMDb           *float64 `json:"imdb,omitempty"`
	RottenTomatoes *int     `json:"rotten_tomatoes,omitempty"`
	ContentRating  *string  `json:"content_rating,omitempty"`
}

type Provider interface {
	Search(context.Context, string) ([]Movie, error)
}

type ShowProvider interface {
	Show(context.Context, string) (Show, error)
	Season(context.Context, string, int) (Season, error)
}

type RatingsProvider interface {
	Ratings(context.Context, string, int) (MovieRatings, error)
}

type IMDbRatingsProvider interface {
	RatingsByIMDbID(context.Context, string) (MovieRatings, error)
}

type IMDbIDProvider interface {
	IMDbID(context.Context, string) (string, error)
}

type Collection string

const (
	CollectionPopular  Collection = "popular"
	CollectionTopRated Collection = "top-rated"
)

type DiscoveryProvider interface {
	Discover(context.Context, Collection) ([]Movie, error)
}
