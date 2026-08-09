package catalog

import "testing"

func TestRankPrefersMatchingHealthySwarm(t *testing.T) {
	seeders1, leechers1 := 12, 4
	seeders2, leechers2 := 1, 0
	request := SearchRequest{
		Query: "Sintel",
		Year:  2010,
		Preferences: Preferences{
			Resolution:   "1080p",
			Codecs:       []string{"h264", "h265"},
			Languages:    []string{"en"},
			MaxSizeBytes: 20 << 30,
		},
	}
	candidates := []Candidate{
		{Name: "Sintel.2010.1080p.x265", Year: 2010, Seeders: &seeders2, Leechers: &leechers2, Languages: []string{"en"}},
		{Name: "Sintel.2010.1080p.x264", Year: 2010, Seeders: &seeders1, Leechers: &leechers1, Languages: []string{"en"}},
		{Name: "Unrelated.2010.1080p.x264", Year: 2010, Seeders: &seeders1},
	}

	ranked := Rank(request, candidates)
	if len(ranked) != 2 {
		t.Fatalf("got %d candidates, want 2", len(ranked))
	}
	if got := ranked[0].Candidate.Name; got != "Sintel.2010.1080p.x264" {
		t.Fatalf("top candidate = %q", got)
	}
	if ranked[0].Candidate.Codec != "h264" || ranked[0].Candidate.Resolution != "1080p" {
		t.Fatalf("release attributes were not inferred: %+v", ranked[0].Candidate)
	}
}

func TestRankExcludesOversizedCandidate(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query:       "Sintel",
		Preferences: Preferences{MaxSizeBytes: 20 << 30},
	}, []Candidate{{Name: "Sintel", SizeBytes: 21 << 30}})
	if len(ranked) != 0 {
		t.Fatalf("got %d candidates, want none", len(ranked))
	}
}
