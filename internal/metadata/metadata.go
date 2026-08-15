package metadata

import "context"

type Movie struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title,omitempty"`
	Year          int     `json:"year,omitempty"`
	ReleaseDate   string  `json:"release_date,omitempty"`
	Overview      string  `json:"overview,omitempty"`
	PosterURL     string  `json:"poster_url,omitempty"`
	BackdropURL   string  `json:"backdrop_url,omitempty"`
	VoteAverage   float64 `json:"vote_average,omitempty"`
}

type MovieRatings struct {
	IMDb           *float64 `json:"imdb,omitempty"`
	RottenTomatoes *int     `json:"rotten_tomatoes,omitempty"`
}

type Provider interface {
	Search(context.Context, string) ([]Movie, error)
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
