import Foundation
import Testing
@testable import FilmstreamCore

@Test func playbackRequestsIncludeResumePointAndPrewarmHint() async throws {
    let recorder = APIRequestRecorder()
    TestURLProtocol.handler = { request in recorder.response(for: request) }
    defer { TestURLProtocol.handler = nil }

    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [TestURLProtocol.self]
    let api = FilmstreamAPI(
        baseURL: URL(string: "https://filmstream.test")!,
        session: URLSession(configuration: configuration)
    )
    let movie = Movie(
        id: "tmdb:1",
        mediaType: .movie,
        title: "The Movie",
        originalLanguage: "ja",
        year: 2020
    )

    try await api.prewarmPlayback(for: movie, startSeconds: 612.5)
    let playback = try await api.createPlayback(for: movie, startSeconds: 612.5)
    let prepared = try await api.prepareNativePlaybackWithRetry(
        playback,
        for: movie,
        startSeconds: 612.5
    )

    #expect(playback.id == "playback-1")
    #expect(prepared.id == "playback-2")
    #expect(recorder.paths == [
        "/v1/playbacks/prewarm",
        "/v1/playbacks",
        "/v1/playbacks/playback-1/hls/subtitles",
        "/v1/playbacks/playback-1/hls",
        "/v1/playbacks",
        "/v1/playbacks/playback-2/hls/subtitles",
        "/v1/playbacks/playback-2/hls",
    ])
    #expect(recorder.startSeconds == [612.5, 612.5, -1, 612.5, 612.5, -1, 612.5])
    #expect(recorder.languages == [
        ["ja", "en", "english"],
        ["ja", "en", "english"],
        [],
        [],
        ["ja", "en", "english"],
        [],
        [],
    ])
}

private final class APIRequestRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var recordedPaths: [String] = []
    private var recordedStartSeconds: [Double] = []
    private var recordedLanguages: [[String]] = []
    private var playbackCount = 0

    var paths: [String] {
        lock.withLock { recordedPaths }
    }

    var startSeconds: [Double] {
        lock.withLock { recordedStartSeconds }
    }

    var languages: [[String]] {
        lock.withLock { recordedLanguages }
    }

    func response(for request: URLRequest) -> (Int, Data) {
        let path = request.url?.path ?? ""
        let body = requestBody(request).flatMap { try? JSONSerialization.jsonObject(with: $0) }
            as? [String: Any]
        let currentPlaybackCount = lock.withLock {
            recordedPaths.append(path)
            recordedStartSeconds.append(body?["start_seconds"] as? Double ?? -1)
            let preferences = body?["preferences"] as? [String: Any]
            recordedLanguages.append(preferences?["languages"] as? [String] ?? [])
            if path == "/v1/playbacks" {
                playbackCount += 1
            }
            return playbackCount
        }
        switch path {
        case "/v1/playbacks/prewarm":
            return (202, Data(#"{"status":"prewarming"}"#.utf8))
        case "/v1/playbacks":
            let response = #"{"id":"playback-\#(currentPlaybackCount)","name":"The Movie","file_name":"movie.mkv","file_size":1000,"stream_url":"https://filmstream.test/v1/playbacks/playback-\#(currentPlaybackCount)/stream"}"#
            return (201, Data(response.utf8))
        case let path where path.hasSuffix("/hls/subtitles"):
            return (200, Data("[]".utf8))
        case "/v1/playbacks/playback-1/hls":
            return (502, Data(#"{"error":"probe playback media: exit status 1"}"#.utf8))
        default:
            let response = #"{"playback_id":"playback-2","playlist_url":"https://filmstream.test/v1/playbacks/playback-2/hls/index.m3u8","start_seconds":612.5,"video_codec":"h264"}"#
            return (201, Data(response.utf8))
        }
    }

    private func requestBody(_ request: URLRequest) -> Data? {
        if let body = request.httpBody {
            return body
        }
        guard let stream = request.httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4_096)
        while stream.hasBytesAvailable {
            let count = stream.read(&buffer, maxLength: buffer.count)
            if count <= 0 { break }
            data.append(buffer, count: count)
        }
        return data
    }
}

private final class TestURLProtocol: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> (Int, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let handler = Self.handler, let url = request.url else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        let (status, data) = handler(request)
        let response = HTTPURLResponse(
            url: url,
            statusCode: status,
            httpVersion: "HTTP/1.1",
            headerFields: ["Content-Type": "application/json"]
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}
