import Foundation
import Testing
@testable import FilmstreamCore

@Test func prewarmerReusesAndRebuildsParkedPlayback() async throws {
    let recorder = RequestRecorder()
    MockURLProtocol.handler = { request in
        recorder.response(for: request)
    }
    defer { MockURLProtocol.handler = nil }

    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [MockURLProtocol.self]
    let api = FilmstreamAPI(
        baseURL: URL(string: "https://filmstream.test")!,
        session: URLSession(configuration: configuration)
    )
    let entry = try JSONDecoder().decode(
        WatchHistoryEntry.self,
        from: Data(#"{"id":"history-1","media_id":"tmdb:1","title":"The Movie","year":2020,"position_seconds":600,"duration_seconds":7200,"completed":false,"updated_at":"2026-08-16T00:00:00Z"}"#.utf8)
    )
    let prewarmer = PlaybackPrewarmer()

    await prewarmer.synchronize(with: [entry], using: api)
    let prepared = await prewarmer.preparedPlayback(
        for: entry.playbackMovie,
        startSeconds: entry.positionSeconds,
        using: api
    )

    #expect(prepared?.playback.id == "warm-playback")
    #expect(recorder.createCount == 1)
    #expect(recorder.hlsStartCount == 2)
    #expect(recorder.parkCount == 1)
    #expect(recorder.stopCount == 0)

    // A fresh Continue Watching load after a quick exit must warm the same
    // resume point again, even when progress changed by less than a second.
    await prewarmer.synchronize(with: [entry], using: api)
    for _ in 0..<100 where recorder.parkCount < 2 {
        try await Task.sleep(for: .milliseconds(10))
    }
    #expect(recorder.createCount == 2)
    #expect(recorder.hlsStartCount == 3)
    #expect(recorder.parkCount == 2)
}

private final class RequestRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var counts: [String: Int] = [:]

    var createCount: Int { count("create") }
    var hlsStartCount: Int { count("hls") }
    var parkCount: Int { count("park") }
    var stopCount: Int { count("stop") }

    func response(for request: URLRequest) -> (Int, Data) {
        let path = request.url?.path ?? ""
        switch (request.httpMethod, path) {
        case ("POST", "/v1/playbacks"):
            increment("create")
            return (201, Data(#"{"id":"warm-playback","name":"The Movie","file_name":"movie.mkv","file_size":1000,"stream_url":"https://filmstream.test/v1/playbacks/warm-playback/stream"}"#.utf8))
        case ("POST", "/v1/playbacks/warm-playback/hls/park"):
            increment("park")
            return (200, Data(#"{"status":"parked"}"#.utf8))
        case ("POST", "/v1/playbacks/warm-playback/hls"):
            increment("hls")
            return (201, Data(#"{"playback_id":"warm-playback","playlist_url":"https://filmstream.test/v1/playbacks/warm-playback/hls/index.m3u8","start_seconds":599.5,"duration_seconds":7200,"video_codec":"h264","subtitles":[]}"#.utf8))
        case ("DELETE", "/v1/playbacks/warm-playback/hls"):
            increment("stop")
            return (200, Data(#"{"status":"ok"}"#.utf8))
        default:
            return (404, Data(#"{"error":"not found"}"#.utf8))
        }
    }

    private func increment(_ key: String) {
        lock.lock()
        counts[key, default: 0] += 1
        lock.unlock()
    }

    private func count(_ key: String) -> Int {
        lock.lock()
        defer { lock.unlock() }
        return counts[key, default: 0]
    }
}

private final class MockURLProtocol: URLProtocol, @unchecked Sendable {
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
