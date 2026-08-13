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

type Provider interface {
	Search(context.Context, string) ([]Movie, error)
}

type Collection string

const (
	CollectionPopular  Collection = "popular"
	CollectionTopRated Collection = "top-rated"
)

type DiscoveryProvider interface {
	Discover(context.Context, Collection) ([]Movie, error)
}
