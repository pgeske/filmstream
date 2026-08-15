package indexer

import (
	"context"
	"errors"
	"net/url"

	"github.com/pgeske/filmstream/internal/catalog"
)

type OpenMedia struct {
	name     string
	endpoint string
}

func NewOpenMedia(name, endpoint string) *OpenMedia {
	return &OpenMedia{name: name, endpoint: endpoint}
}

func (o *OpenMedia) Name() string { return o.name }

func (o *OpenMedia) Search(_ context.Context, _ catalog.SearchRequest) ([]catalog.Candidate, error) {
	items := []struct {
		id       string
		name     string
		year     int
		filename string
	}{
		{id: "sintel", name: "Sintel", year: 2010, filename: "sintel.torrent"},
		{id: "big-buck-bunny", name: "Big Buck Bunny", year: 2008, filename: "big-buck-bunny.torrent"},
		{id: "tears-of-steel", name: "Tears of Steel", year: 2012, filename: "tears-of-steel.torrent"},
		{id: "cosmos-laundromat", name: "Cosmos Laundromat", year: 2015, filename: "cosmos-laundromat.torrent"},
	}
	candidates := make([]catalog.Candidate, 0, len(items))
	for _, item := range items {
		torrentURL, err := url.JoinPath(o.endpoint, item.filename)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, catalog.Candidate{
			Protocol:   catalog.ProtocolTorrent,
			ID:         item.id,
			Name:       item.name,
			Year:       item.year,
			Languages:  []string{"en"},
			Trusted:    true,
			TorrentURL: torrentURL,
		})
	}
	return candidates, nil
}

func (o *OpenMedia) Resolve(_ context.Context, candidate catalog.Candidate) (Source, error) {
	if candidate.TorrentURL == "" {
		return Source{}, errors.New("open-media candidate has no torrent URL")
	}
	return Source{TorrentURL: candidate.TorrentURL}, nil
}
