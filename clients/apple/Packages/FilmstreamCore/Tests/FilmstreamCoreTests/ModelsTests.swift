import Foundation
import Testing
@testable import FilmstreamCore

@Test func decodesMovieArtwork() throws {
    let data = Data(#"{"id":"tmdb:335984","title":"Blade Runner 2049","year":2017,"poster_url":"https://image.tmdb.org/poster.jpg","backdrop_url":"https://image.tmdb.org/backdrop.jpg","genres":["Science Fiction","Drama"]}"#.utf8)
    let movie = try JSONDecoder().decode(Movie.self, from: data)
    #expect(movie.id == "tmdb:335984")
    #expect(movie.year == 2017)
    #expect(movie.posterURL?.host == "image.tmdb.org")
    #expect(movie.primaryGenre == "Science Fiction")
    #expect(movie.genreSummary == "Science Fiction • Drama")
}

@Test func decodesDiscoverySections() throws {
    let data = Data(#"{"id":"popular","title":"Popular Now","subtitle":"Popular movies available for home viewing","items":[{"id":"tmdb:335984","title":"Blade Runner 2049","year":2017}]}"#.utf8)
    let section = try JSONDecoder().decode(DiscoverySection.self, from: data)
    #expect(section.id == "popular")
    #expect(section.items.first?.title == "Blade Runner 2049")
}

@Test func decodesMixedShowMetadataAndEpisodes() throws {
    let showData = Data(#"{"show":{"id":"tmdb-tv:66732","media_type":"show","title":"Stranger Things","original_language":"ja","year":2016,"genres":["Drama"],"number_of_seasons":5},"seasons":[{"number":1,"name":"Season 1","episode_count":8}]}"#.utf8)
    let details = try JSONDecoder().decode(SeriesDetails.self, from: showData)
    #expect(details.show.isShow)
    #expect(details.show.originalLanguage == "ja")
    #expect(details.show.catalogMetadata == "Show • Drama • 2016 • 5 Seasons")
    #expect(details.seasons.first?.episodeCount == 8)

    let seasonData = Data(#"{"series_id":"tmdb-tv:66732","series_title":"Stranger Things","number":1,"name":"Season 1","episodes":[{"id":"tmdb-tv:66732:s1:e1","series_id":"tmdb-tv:66732","series_title":"Stranger Things","season_number":1,"episode_number":1,"title":"Chapter One","still_url":"https://image.tmdb.org/episode.jpg","runtime_minutes":49}]}"#.utf8)
    let season = try JSONDecoder().decode(ShowSeason.self, from: seasonData)
    let episode = try #require(season.episodes.first)
    #expect(episode.label == "S1 E1")
    #expect(episode.playbackMovie(in: details.show).seriesID == details.show.id)
    #expect(episode.playbackMovie(in: details.show).originalLanguage == "ja")
}

@Test func decodesExternalMovieRatings() throws {
    let data = Data(#"{"imdb":8.7,"rotten_tomatoes":83,"content_rating":"R"}"#.utf8)
    let ratings = try JSONDecoder().decode(MovieRatings.self, from: data)
    #expect(ratings.imdb == 8.7)
    #expect(ratings.rottenTomatoes == 83)
    #expect(ratings.contentRating == "R")
    #expect(!ratings.isEmpty)
}

@Test func decodesNativeHLSPlayback() throws {
    let data = Data(#"{"playback_id":"abc123","playlist_url":"https://filmstream.example/v1/playbacks/abc123/hls/index.m3u8","start_seconds":120,"duration_seconds":7200,"video_codec":"h264","subtitles":[{"index":6,"language":"en","title":"SDH"}]}"#.utf8)
    let playback = try JSONDecoder().decode(HLSPlayback.self, from: data)
    #expect(playback.id == "abc123")
    #expect(playback.startSeconds == 120)
    #expect(playback.durationSeconds == 7200)
    #expect(playback.playlistURL.pathExtension == "m3u8")
    #expect(playback.subtitles?.first?.language == "en")
}

@Test func parsesGrowingWebVTTWithPlaybackOffset() {
    let data = Data("""
    WEBVTT

    00:01.250 --> 00:03.500
    <i>Wake up, Neo.</i>

    00:04.000 --> 00:06.000 align:middle
    The Matrix has you.

    """.utf8)
    let cues = WebVTTParser.parse(data, offsetSeconds: 120)
    #expect(cues == [
        SubtitleCue(startSeconds: 121.25, endSeconds: 123.5, text: "Wake up, Neo."),
        SubtitleCue(startSeconds: 124, endSeconds: 126, text: "The Matrix has you.")
    ])
}

@Test func decodesEpisodeContinueWatchingAsShow() throws {
    let data = Data(#"{"id":"entry-show","media_id":"tmdb-tv:66732:s1:e2","media_type":"show","title":"Stranger Things","year":2016,"number_of_seasons":5,"series_id":"tmdb-tv:66732","series_title":"Stranger Things","season_number":1,"episode_number":2,"episode_title":"Chapter Two","position_seconds":600,"duration_seconds":1800,"completed":false,"updated_at":"2026-08-10T00:00:00Z"}"#.utf8)
    let entry = try JSONDecoder().decode(WatchHistoryEntry.self, from: data)
    #expect(entry.episodeLabel == "S1 E2")
    #expect(entry.movie.id == "tmdb-tv:66732")
    #expect(entry.movie.isShow)
    #expect(entry.movie.episodeLabel == "S1 E2")
    #expect(entry.playbackMovie.id == "tmdb-tv:66732:s1:e2")
}

@Test func computesContinueWatchingProgress() throws {
    let data = Data(#"{"id":"entry-1","media_id":"tmdb:335984","title":"Blade Runner 2049","year":2017,"genres":["Science Fiction"],"position_seconds":600,"duration_seconds":1800,"completed":false,"updated_at":"2026-08-10T00:00:00Z"}"#.utf8)
    let entry = try JSONDecoder().decode(WatchHistoryEntry.self, from: data)
    #expect(entry.progress == 1.0 / 3.0)
    #expect(entry.movie.id == "tmdb:335984")
    #expect(entry.movie.primaryGenre == "Science Fiction")
}
