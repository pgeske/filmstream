package catalog

import (
	"maps"
	"slices"
	"testing"
	"time"
)

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
			MaxSizeBytes: 60 << 30,
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

func TestRankStreamingOptimizedPrefersUsenet(t *testing.T) {
	manySeeders := 500
	ranked := Rank(SearchRequest{
		Query: "Sintel",
		Year:  2010,
		Preferences: Preferences{
			Resolution:         "1080p",
			Codecs:             []string{"h264"},
			StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "Sintel.2010.1080p.WEB.H264-USENET", Protocol: ProtocolUsenet},
		{Name: "Sintel.2010.1080p.WEB.H264-TORRENT", Protocol: ProtocolTorrent, Seeders: &manySeeders},
	})
	if len(ranked) != 2 {
		t.Fatalf("ranked = %+v", ranked)
	}
	if got := ranked[0].Candidate.Protocol; got != ProtocolUsenet {
		t.Fatalf("top protocol = %q, want Usenet", got)
	}
}

func TestRankStreamingOptimizedPrefersRecentUsenetPost(t *testing.T) {
	now := time.Now()
	ranked := Rank(SearchRequest{
		Query: "The Movie",
		Year:  2001,
		Preferences: Preferences{
			Resolution: "1080p", Codecs: []string{"h264"}, StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "The.Movie.2001.1080p.H264-OLD", Protocol: ProtocolUsenet, PublishedUnix: now.AddDate(-12, 0, 0).Unix()},
		{Name: "The.Movie.2001.1080p.H264-NEW", Protocol: ProtocolUsenet, PublishedUnix: now.AddDate(0, -2, 0).Unix()},
	})
	if len(ranked) != 2 || ranked[0].Candidate.Name != "The.Movie.2001.1080p.H264-NEW" {
		t.Fatalf("ranked = %+v", ranked)
	}
}

func TestRankStreamingOptimizedPrefersNativeFriendlyEncode(t *testing.T) {
	manySeeders, enoughSeeders := 300, 20
	ranked := Rank(SearchRequest{
		Query: "The Matrix",
		Year:  1999,
		Preferences: Preferences{
			Resolution:         "1080p",
			Codecs:             []string{"h264", "h265"},
			MaxSizeBytes:       50 << 30,
			StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "The.Matrix.1999.1080p.BluRay.AVC.TrueHD.REMUX", SizeBytes: 32 << 30, Seeders: &manySeeders},
		{Name: "The.Matrix.1999.2160p.DV.HEVC.REMUX", SizeBytes: 40 << 30, Seeders: &manySeeders},
		{Name: "The.Matrix.1999.1080p.WEB-DL.x264.AAC", SizeBytes: 8 << 30, Seeders: &enoughSeeders},
		{Name: "The.Matrix.1999.1080p.Xvid", SizeBytes: 2 << 30, Seeders: &manySeeders},
	})
	if len(ranked) != 2 {
		t.Fatalf("got %d candidates, want compatible WEB and remux candidates", len(ranked))
	}
	if got := ranked[0].Candidate.Name; got != "The.Matrix.1999.1080p.WEB-DL.x264.AAC" {
		t.Fatalf("top candidate = %q", got)
	}
}

func TestRankStreamingOptimizedRejectsUpscalesAnd2160pRemuxes(t *testing.T) {
	seeders := 500
	ranked := Rank(SearchRequest{
		Query: "Classic Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 1,
		PreferSeasonPack: true,
		Preferences: Preferences{
			Resolution: "1080p", Codecs: []string{"h264", "h265"}, StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "Classic.Show.S01.2160p.UHD.REMUX.HEVC", Seeders: &seeders},
		{Name: "Classic.Show.S01.1080p.AI.Upscaled.x265", Seeders: &seeders},
		{Name: "Classic.Show.S01.1080p.WEB-DL.x265", Seeders: &seeders},
	})
	if len(ranked) != 1 || ranked[0].Candidate.Name != "Classic.Show.S01.1080p.WEB-DL.x265" {
		t.Fatalf("ranked = %+v", ranked)
	}
}

func TestRankStreamingOptimizedPrioritizesPreferredQualityBeforeSeeders(t *testing.T) {
	manySeeders, enoughSeeders := 5000, 20
	ranked := Rank(SearchRequest{
		Query: "The Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 1,
		PreferSeasonPack: true,
		Preferences: Preferences{
			Resolution: "1080p", Codecs: []string{"h264", "h265"}, StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "The.Show.S01.720p.WEB-DL.x264", Seeders: &manySeeders},
		{Name: "The.Show.S01.1080p.WEB-DL.x265", Seeders: &enoughSeeders},
	})
	if len(ranked) != 2 || ranked[0].Candidate.Name != "The.Show.S01.1080p.WEB-DL.x265" {
		t.Fatalf("ranked = %+v", ranked)
	}
}

func TestRankStreamingOptimizedPrioritizesPopularityOverSize(t *testing.T) {
	popularSeeders, smallerSeeders := 140, 40
	ranked := Rank(SearchRequest{
		Query: "The Matrix",
		Year:  1999,
		Preferences: Preferences{
			Resolution:         "1080p",
			Codecs:             []string{"h264", "h265"},
			MaxSizeBytes:       50 << 30,
			StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "The.Matrix.1999.1080p.BluRay.x264-LARGE", SizeBytes: 30 << 30, Seeders: &popularSeeders},
		{Name: "The.Matrix.1999.1080p.WEBRip.x264-SMALL", SizeBytes: 6 << 30, Seeders: &smallerSeeders},
	})
	if got := ranked[0].Candidate.Name; got != "The.Matrix.1999.1080p.BluRay.x264-LARGE" {
		t.Fatalf("top candidate = %q, want more popular release", got)
	}
}

func TestRankStreamingOptimizedIgnoresFreeleechAsHealthSignal(t *testing.T) {
	moreSeeders, fewerSeeders := 80, 60
	freeleech, normal := 0.0, 1.0
	ranked := Rank(SearchRequest{
		Query: "The Matrix",
		Year:  1999,
		Preferences: Preferences{
			Resolution:         "1080p",
			Codecs:             []string{"h264"},
			StreamingOptimized: true,
		},
	}, []Candidate{
		{Name: "The.Matrix.1999.1080p.BluRay.x264-POPULAR", SizeBytes: 10 << 30, Seeders: &moreSeeders, DownloadVolumeFactor: &normal},
		{Name: "The.Matrix.1999.1080p.WEBRip.x264-FREE", SizeBytes: 10 << 30, Seeders: &fewerSeeders, DownloadVolumeFactor: &freeleech},
	})
	if got := ranked[0].Candidate.Name; got != "The.Matrix.1999.1080p.BluRay.x264-POPULAR" {
		t.Fatalf("top candidate = %q, want more popular release", got)
	}
}

func TestRankStreamingOptimizedAllowsTrustedUnknownCodec(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query: "Sintel",
		Preferences: Preferences{
			Codecs:             []string{"h264", "h265"},
			StreamingOptimized: true,
		},
	}, []Candidate{{Name: "Sintel", Trusted: true}})
	if len(ranked) != 1 {
		t.Fatalf("trusted catalog candidate was rejected: %+v", ranked)
	}
}

func TestRankPopularMovieReleaseNames(t *testing.T) {
	seeders := 100
	preferences := Preferences{
		Resolution: "1080p", Codecs: []string{"h264", "h265"},
		MaxSizeBytes: 60 << 30, StreamingOptimized: true,
	}
	tests := []struct {
		name       string
		mediaID    string
		query      string
		year       int
		candidates []Candidate
		want       string
	}{
		{
			name: "Pirates of the Caribbean Dead Men Tell No Tales", mediaID: "tmdb:166426",
			query: "Pirates of the Caribbean: Dead Men Tell No Tales", year: 2017,
			candidates: []Candidate{
				{Name: "Pirates of the Caribbean Dead Men Tell No Tales 2017 UHD BluRay 2160p DV HEVC REMUX-FraMeSToR", SizeBytes: 52 << 30, Seeders: &seeders},
				{Name: "Pirates of the Caribbean Dead Men Tell No Tales 2017 REPACK 1080p BluRay DD+ 7 1 X265-Ralphy", SizeBytes: 6 << 30, Seeders: &seeders},
			},
			want: "Pirates of the Caribbean Dead Men Tell No Tales 2017 REPACK 1080p BluRay DD+ 7 1 X265-Ralphy",
		},
		{
			name: "Dune Part Two", mediaID: "tmdb:693134",
			query: "Dune: Part Two", year: 2024,
			candidates: []Candidate{
				{Name: "Dune Part Two 2024 UHD BluRay 2160p TrueHD Atmos DV HEVC REMUX-FraMeSToR", SizeBytes: 64 << 30, Seeders: &seeders},
				{Name: "Dune Part Two 2024 1080p WEB-DL H264 AAC-InMemoryOfEVO", SizeBytes: 7 << 30, Seeders: &seeders},
			},
			want: "Dune Part Two 2024 1080p WEB-DL H264 AAC-InMemoryOfEVO",
		},
	}
	for _, test := range tests {
		t.Run(test.mediaID, func(t *testing.T) {
			ranked := Rank(SearchRequest{
				Query: test.query, Year: test.year, MediaType: "movie", Preferences: preferences,
			}, test.candidates)
			if len(ranked) != 1 || ranked[0].Candidate.Name != test.want {
				t.Fatalf("ranked = %+v", ranked)
			}
		})
	}
}

func TestRankDiagnosticsExplainFastFixtureMovieRejections(t *testing.T) {
	candidates := []Candidate{
		{Name: "Sintel", Year: 2010, Trusted: true},
		{Name: "Big Buck Bunny", Year: 2008, Trusted: true},
		{Name: "Tears of Steel", Year: 2012, Trusted: true},
		{Name: "Cosmos Laundromat", Year: 2015, Trusted: true},
	}
	ranked, diagnostics := RankWithDiagnostics(SearchRequest{
		Query: "Dune: Part Two", Year: 2024, MediaType: "movie",
		Preferences: Preferences{Codecs: []string{"h264", "h265"}, StreamingOptimized: true},
	}, candidates)
	if len(ranked) != 0 || diagnostics.Accepted != 0 || diagnostics.Rejected != len(candidates) {
		t.Fatalf("ranked = %+v, diagnostics = %+v", ranked, diagnostics)
	}
	if got := diagnostics.RejectionReasons[rejectionYearMismatch]; got != 4 {
		t.Fatalf("year mismatch rejections = %d, diagnostics = %+v", got, diagnostics)
	}
	for index, rejection := range diagnostics.Rejections {
		if rejection.Candidate.Name != candidates[index].Name || rejection.Reason != rejectionYearMismatch {
			t.Fatalf("rejection %d = %+v", index, rejection)
		}
	}
}

func TestRankDiagnosticsKeepZeroSeederMovieEligible(t *testing.T) {
	seeders := 0
	ranked, diagnostics := RankWithDiagnostics(SearchRequest{
		Query: "Dune: Part Two", Year: 2024, MediaType: "movie",
		Preferences: Preferences{
			Resolution: "1080p", Codecs: []string{"h264"}, StreamingOptimized: true,
		},
	}, []Candidate{{
		Name: "Dune Part Two 2024 1080p WEB-DL H264-GROUP", Seeders: &seeders,
		Categories: []int{2000, 2030},
	}})
	if len(ranked) != 1 || diagnostics.Accepted != 1 || diagnostics.Rejected != 0 {
		t.Fatalf("ranked = %+v, diagnostics = %+v", ranked, diagnostics)
	}
}

func TestRankDiagnosticsCountEveryHardPolicyReason(t *testing.T) {
	request := SearchRequest{
		Query: "The Movie", Year: 2001, MediaType: "movie",
		Preferences: Preferences{
			Codecs: []string{"h264", "h265"}, MaxSizeBytes: 60 << 30, StreamingOptimized: true,
		},
	}
	_, diagnostics := RankWithDiagnostics(request, []Candidate{
		{Name: "The Movie 2001 1080p x264", SizeBytes: 61 << 30},
		{Name: "The Movie 2001 2160p DV x265"},
		{Name: "The Movie 2001 1080p AI Upscaled x265"},
		{Name: "The Movie 2001 2160p REMUX x265"},
		{Name: "The Movie 2001 1080p"},
		{Name: "The Movie 2001 1080p AV1", Codec: "av1"},
		{Name: "The Movie 1999 1080p x264"},
		{Name: "Another Movie 2001 1080p x264"},
	})
	want := map[string]int{
		rejectionMaxSize: 1, rejectionDolbyVision: 1, rejectionAIUpscale: 1,
		rejection2160pRemux: 1, rejectionUnknownCodec: 1,
		rejectionUnsupportedCodec: 1, rejectionYearMismatch: 1, rejectionTitleMismatch: 1,
	}
	if !maps.Equal(diagnostics.RejectionReasons, want) || diagnostics.Rejected != 8 || diagnostics.Accepted != 0 {
		t.Fatalf("diagnostics = %+v, want reasons = %v", diagnostics, want)
	}
}

func TestRankExcludesOversizedCandidate(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query:       "Sintel",
		Preferences: Preferences{MaxSizeBytes: 60 << 30},
	}, []Candidate{{Name: "Sintel", SizeBytes: 61 << 30}})
	if len(ranked) != 0 {
		t.Fatalf("got %d candidates, want none", len(ranked))
	}
}

func TestRankRejectsNewerSequelForOriginalMovie(t *testing.T) {
	manySeeders, fewSeeders := 297, 3
	ranked := Rank(SearchRequest{Query: "Kung Fu Panda", Year: 2008}, []Candidate{
		{Name: "Kung Fu Panda 4 2024 1080p WEB h264-ETHEL", Seeders: &manySeeders},
		{Name: "Kung Fu Panda 2008 1080p BluRay x264", Seeders: &fewSeeders},
	})
	if len(ranked) != 1 {
		t.Fatalf("got %d candidates, want only the matching year", len(ranked))
	}
	if got := ranked[0].Candidate.Name; got != "Kung Fu Panda 2008 1080p BluRay x264" {
		t.Fatalf("top candidate = %q", got)
	}
	if got := ranked[0].Candidate.Year; got != 2008 {
		t.Fatalf("inferred year = %d, want 2008", got)
	}
}

func TestRankRejectsOriginalMovieForRequestedSequel(t *testing.T) {
	manySeeders, fewSeeders := 200, 2
	ranked := Rank(SearchRequest{Query: "Paddington 2", Year: 2017}, []Candidate{
		{Name: "Paddington 2014 1080p BluRay x264", Seeders: &manySeeders},
		{Name: "Paddington 2 2017 1080p BluRay x264", Seeders: &fewSeeders},
	})
	if len(ranked) != 1 {
		t.Fatalf("got %d candidates, want only the matching year", len(ranked))
	}
	if got := ranked[0].Candidate.Name; got != "Paddington 2 2017 1080p BluRay x264" {
		t.Fatalf("top candidate = %q", got)
	}
}

func TestRankSelectsExactEpisodeAndAllowsSeasonPackFallback(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query: "Top Rated Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 2,
	}, []Candidate{
		{Name: "Top.Rated.Show.S01E03.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S01.Complete.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S01E02.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S02E02.1080p.WEB.H264"},
	})
	if len(ranked) != 2 {
		t.Fatalf("ranked = %+v", ranked)
	}
	if ranked[0].Candidate.Name != "Top.Rated.Show.S01E02.1080p.WEB.H264" {
		t.Fatalf("top candidate = %q", ranked[0].Candidate.Name)
	}
	if !slices.Contains(ranked[0].Reasons, "exact episode") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
}

func TestRankUsesOnlySeasonPacksWhenAvailable(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query: "Top Rated Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 2,
		PreferSeasonPack: true,
	}, []Candidate{
		{Name: "Top.Rated.Show.S01E02.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S01.Complete.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S01E03.1080p.WEB.H264"},
	})
	if len(ranked) != 1 || ranked[0].Candidate.Name != "Top.Rated.Show.S01.Complete.1080p.WEB.H264" {
		t.Fatalf("ranked = %+v", ranked)
	}
	if !slices.Contains(ranked[0].Reasons, "season pack") {
		t.Fatalf("reasons = %v", ranked[0].Reasons)
	}
}

func TestRankFallsBackToIndividualEpisodeWithoutSeasonPack(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query: "Top Rated Show", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 2,
		PreferSeasonPack: true,
	}, []Candidate{
		{Name: "Top.Rated.Show.S01E02.1080p.WEB.H264"},
		{Name: "Top.Rated.Show.S01E03.1080p.WEB.H264"},
	})
	if len(ranked) != 1 || ranked[0].Candidate.Name != "Top.Rated.Show.S01E02.1080p.WEB.H264" {
		t.Fatalf("ranked = %+v", ranked)
	}
}

func TestRankRejectsDifferentSeriesSharingOnlyStopWords(t *testing.T) {
	ranked := Rank(SearchRequest{
		Query: "House of the Dragon", MediaType: "show", SeasonNumber: 1, EpisodeNumber: 1,
	}, []Candidate{{Name: "Game.of.Thrones.S01E01.1080p.HEVC.x265"}})
	if len(ranked) != 0 {
		t.Fatalf("ranked = %+v, want unrelated series rejected", ranked)
	}
}

func TestRankRejectsDifferentTitlesThatContainRequestedTitle(t *testing.T) {
	tests := []struct {
		query     string
		candidate string
	}{
		{"Chernobyl", "Chernobyl.Inside.the.Meltdown.S01E01.1080p.WEB.H264"},
		{"Paddington", "The.Adventures.of.Paddington.S01E01.1080p.WEB.H264"},
	}
	for _, test := range tests {
		ranked := Rank(SearchRequest{
			Query: test.query, MediaType: "show", SeasonNumber: 1, EpisodeNumber: 1,
		}, []Candidate{{Name: test.candidate}})
		if len(ranked) != 0 {
			t.Fatalf("query %q ranked unrelated candidate %q", test.query, test.candidate)
		}
	}
}

func TestTitleSimilarityMatchesPossessiveReleaseNames(t *testing.T) {
	if got := titleSimilarity("Harry Potter and the Philosopher's Stone", "Harry.Potter.and.the.Philosophers.Stone.2001.1080p.x265"); got != 1 {
		t.Fatalf("title similarity = %v, want 1", got)
	}
}

func TestTitleSimilarityDistinguishesSequels(t *testing.T) {
	exact := titleSimilarity("Kung Fu Panda", "Kung Fu Panda 2008 1080p BluRay x264")
	sequel := titleSimilarity("Kung Fu Panda", "Kung Fu Panda 4 2024 1080p WEB h264-ETHEL")
	if exact != 1 {
		t.Fatalf("exact title similarity = %v, want 1", exact)
	}
	if sequel >= exact {
		t.Fatalf("sequel similarity = %v, want less than exact match %v", sequel, exact)
	}
}

func TestInferReleaseYearIgnoresNumbersInMovieTitle(t *testing.T) {
	tests := []struct {
		query    string
		release  string
		wantYear int
	}{
		{"2001: A Space Odyssey", "2001 A Space Odyssey 1968 1080p BluRay", 1968},
		{"Blade Runner 2049", "Blade Runner 2049 2017 2160p UHD BluRay", 2017},
		{"1917", "1917 2019 1080p BluRay", 2019},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			if got := inferReleaseYear(test.query, test.release); got != test.wantYear {
				t.Fatalf("inferred year = %d, want %d", got, test.wantYear)
			}
		})
	}
}
