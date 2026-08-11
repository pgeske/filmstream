import Foundation
import Testing
@testable import FilmstreamCore

@Test func decodesMovieArtwork() throws {
    let data = Data(#"{"id":"tmdb:335984","title":"Blade Runner 2049","year":2017,"poster_url":"https://image.tmdb.org/poster.jpg","backdrop_url":"https://image.tmdb.org/backdrop.jpg"}"#.utf8)
    let movie = try JSONDecoder().decode(Movie.self, from: data)
    #expect(movie.id == "tmdb:335984")
    #expect(movie.year == 2017)
    #expect(movie.posterURL?.host == "image.tmdb.org")
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

@Test func computesContinueWatchingProgress() throws {
    let data = Data(#"{"id":"entry-1","media_id":"tmdb:335984","title":"Blade Runner 2049","year":2017,"position_seconds":600,"duration_seconds":1800,"completed":false,"updated_at":"2026-08-10T00:00:00Z"}"#.utf8)
    let entry = try JSONDecoder().decode(WatchHistoryEntry.self, from: data)
    #expect(entry.progress == 1.0 / 3.0)
    #expect(entry.movie.id == "tmdb:335984")
}
