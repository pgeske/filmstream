import Foundation

public struct FilmstreamAPI: Sendable {
    public let baseURL: URL
    private let session: URLSession
    private let encoder: JSONEncoder
    private let decoder: JSONDecoder

    public init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
        encoder = JSONEncoder()
        decoder = JSONDecoder()
    }

    public func health() async throws {
        let _: HealthResponse = try await send(path: "v1/health")
    }

    public func search(_ query: String) async throws -> [Movie] {
        var components = URLComponents(
            url: baseURL.appendingPathComponent("v1/catalog/search"),
            resolvingAgainstBaseURL: false
        )
        components?.queryItems = [URLQueryItem(name: "query", value: query)]
        guard let url = components?.url else {
            throw FilmstreamError.invalidURL
        }
        let response: CatalogResponse = try await send(url: url)
        return response.items
    }

    public func continueWatching() async throws -> [WatchHistoryEntry] {
        var components = URLComponents(
            url: baseURL.appendingPathComponent("v1/watch-history"),
            resolvingAgainstBaseURL: false
        )
        components?.queryItems = [URLQueryItem(name: "continue", value: "true")]
        guard let url = components?.url else {
            throw FilmstreamError.invalidURL
        }
        let response: HistoryResponse = try await send(url: url)
        return response.entries
    }

    public func createPlayback(for movie: Movie) async throws -> Playback {
        try await send(
            path: "v1/playbacks",
            method: "POST",
            body: CreatePlaybackRequest(
                query: movie.title,
                year: movie.year,
                preferences: .appleTV
            )
        )
    }

    public func prepareNativePlayback(
        _ playback: Playback,
        startSeconds: Double
    ) async throws -> PreparedPlayback {
        let hls: HLSPlayback = try await send(
            path: "v1/playbacks/\(playback.id)/hls",
            method: "POST",
            body: HLSRequest(startSeconds: max(0, startSeconds))
        )
        return PreparedPlayback(playback: playback, hls: hls)
    }

    public func stopNativePlayback(_ playbackID: String) async throws {
        var request = URLRequest(url: baseURL.appendingPathComponent("v1/playbacks/\(playbackID)/hls"))
        request.httpMethod = "DELETE"
        request.timeoutInterval = 15
        let _: HealthResponse = try await send(request)
    }

    public func startSubtitle(playbackID: String, track: HLSSubtitleTrack) async throws {
        var request = URLRequest(
            url: baseURL.appendingPathComponent(
                "v1/playbacks/\(playbackID)/hls/subtitles/\(track.index)"
            )
        )
        request.httpMethod = "POST"
        request.timeoutInterval = 15
        let _: HealthResponse = try await send(request)
    }

    public func subtitleCues(
        playbackID: String,
        track: HLSSubtitleTrack,
        offsetSeconds: Double
    ) async throws -> [SubtitleCue]? {
        let url = baseURL.appendingPathComponent(
            "v1/playbacks/\(playbackID)/hls/subtitle-\(track.index).vtt"
        )
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData)
        request.timeoutInterval = 15
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            throw FilmstreamError.network(error.localizedDescription)
        }
        guard let httpResponse = response as? HTTPURLResponse else {
            throw FilmstreamError.invalidResponse
        }
        if httpResponse.statusCode == 404 {
            return nil
        }
        guard (200..<300).contains(httpResponse.statusCode) else {
            let payload = try? decoder.decode(ErrorResponse.self, from: data)
            throw FilmstreamError.server(status: httpResponse.statusCode, message: payload?.error)
        }
        return WebVTTParser.parse(data, offsetSeconds: offsetSeconds)
    }

    @discardableResult
    public func updateProgress(
        for movie: Movie,
        positionSeconds: Double,
        durationSeconds: Double
    ) async throws -> WatchHistoryEntry {
        try await send(
            path: "v1/watch-history",
            method: "PUT",
            body: WatchProgress(
                movie: movie,
                positionSeconds: positionSeconds,
                durationSeconds: durationSeconds
            )
        )
    }

    private func send<Response: Decodable>(path: String) async throws -> Response {
        try await send(url: baseURL.appendingPathComponent(path))
    }

    private func send<Response: Decodable>(url: URL) async throws -> Response {
        var request = URLRequest(url: url)
        request.timeoutInterval = 30
        return try await send(request)
    }

    private func send<Request: Encodable, Response: Decodable>(
        path: String,
        method: String,
        body: Request
    ) async throws -> Response {
        var request = URLRequest(url: baseURL.appendingPathComponent(path))
        request.httpMethod = method
        request.timeoutInterval = 240
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try encoder.encode(body)
        return try await send(request)
    }

    private func send<Response: Decodable>(_ request: URLRequest) async throws -> Response {
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            throw FilmstreamError.network(error.localizedDescription)
        }
        guard let httpResponse = response as? HTTPURLResponse else {
            throw FilmstreamError.invalidResponse
        }
        guard (200..<300).contains(httpResponse.statusCode) else {
            let payload = try? decoder.decode(ErrorResponse.self, from: data)
            throw FilmstreamError.server(status: httpResponse.statusCode, message: payload?.error)
        }
        do {
            return try decoder.decode(Response.self, from: data)
        } catch {
            throw FilmstreamError.decoding(error.localizedDescription)
        }
    }
}

public enum FilmstreamError: LocalizedError, Equatable, Sendable {
    case invalidURL
    case invalidResponse
    case network(String)
    case server(status: Int, message: String?)
    case decoding(String)

    public var errorDescription: String? {
        switch self {
        case .invalidURL:
            "TeaStream returned an invalid URL."
        case .invalidResponse:
            "TeaStream returned an invalid response."
        case let .network(message):
            "Could not reach TeaStream: \(message)"
        case let .server(_, message):
            message ?? "TeaStream could not complete the request."
        case let .decoding(message):
            "Could not read the TeaStream response: \(message)"
        }
    }
}

private struct HealthResponse: Decodable {
    let status: String
}

private struct CatalogResponse: Decodable {
    let items: [Movie]
}

private struct HistoryResponse: Decodable {
    let entries: [WatchHistoryEntry]
}

private struct CreatePlaybackRequest: Encodable {
    let query: String
    let year: Int?
    let preferences: PlaybackPreferences
}

private struct PlaybackPreferences: Encodable {
    let resolution: String
    let codecs: [String]
    let languages: [String]
    let maxSizeBytes: Int64
    let streamingOptimized: Bool

    static let appleTV = PlaybackPreferences(
        resolution: "1080p",
        codecs: ["h264", "h265"],
        languages: ["en", "english"],
        maxSizeBytes: 50 * 1_073_741_824,
        streamingOptimized: true
    )

    private enum CodingKeys: String, CodingKey {
        case resolution, codecs, languages
        case maxSizeBytes = "max_size_bytes"
        case streamingOptimized = "streaming_optimized"
    }
}

private struct HLSRequest: Encodable {
    let startSeconds: Double

    private enum CodingKeys: String, CodingKey {
        case startSeconds = "start_seconds"
    }
}

private struct ErrorResponse: Decodable {
    let error: String
}
