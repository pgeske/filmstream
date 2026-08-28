import Foundation
import Testing
@testable import FilmstreamCore

@Test func decodesRecommendationStateWithOptionalRFC3339DatesAndMixedItems() throws {
    let datedData = Data(#"{"generated_at":"2026-08-21T14:32:10.125Z","prompt":"Slow-burn science fiction and clever mysteries. Avoid gore.","refreshing":true,"items":[{"id":"tmdb:335984","media_type":"movie","title":"Blade Runner 2049"},{"id":"tmdb-tv:66732","media_type":"show","title":"Stranger Things"}]}"#.utf8)
    let dated = try JSONDecoder().decode(Recommendations.self, from: datedData)
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]

    #expect(dated.generatedAt == formatter.date(from: "2026-08-21T14:32:10.125Z"))
    #expect(dated.prompt == "Slow-burn science fiction and clever mysteries. Avoid gore.")
    #expect(dated.refreshing)
    #expect(dated.items.map(\.mediaType) == [.movie, .show])

    let undatedData = Data(#"{"prompt":"","refreshing":false,"items":[]}"#.utf8)
    let undated = try JSONDecoder().decode(Recommendations.self, from: undatedData)
    #expect(undated.generatedAt == nil)
    #expect(undated.prompt.isEmpty)
    #expect(undated.items.isEmpty)
}

@Test func splitsRecommendationShelvesStablyWithUnknownTypesFallingBackToMovies() throws {
    let data = Data(#"{"prompt":"Mixed picks","refreshing":false,"items":[{"id":"show-1","media_type":"show","title":"First Show"},{"id":"movie-1","media_type":"movie","title":"First Movie"},{"id":"legacy-1","title":"Legacy Pick"},{"id":"future-1","media_type":"limited_series","title":"Future Pick"},{"id":"show-2","media_type":"show","title":"Second Show"}]}"#.utf8)
    let recommendations = try JSONDecoder().decode(Recommendations.self, from: data)

    #expect(recommendations.recommendedShows.map(\.id) == ["show-1", "show-2"])
    #expect(recommendations.recommendedMovies.map(\.id) == ["movie-1", "legacy-1", "future-1"])
    #expect(recommendations.items[3].mediaType == .movie)
}

@Test func mergesRecommendationGenerationsWithoutDiscardingCachedState() {
    let current = Recommendations(
        generatedAt: Date(timeIntervalSince1970: 200),
        prompt: "Saved taste",
        refreshing: true,
        items: [Movie(id: "cached", mediaType: .movie, title: "Cached Pick")]
    )
    let emptyPoll = Recommendations(
        generatedAt: Date(timeIntervalSince1970: 200),
        prompt: "Stale prompt",
        refreshing: false,
        items: []
    )
    let olderPoll = Recommendations(
        generatedAt: Date(timeIntervalSince1970: 100),
        prompt: "Older prompt",
        refreshing: false,
        items: [Movie(id: "older", mediaType: .show, title: "Older Pick")]
    )
    let newerPoll = Recommendations(
        generatedAt: Date(timeIntervalSince1970: 300),
        prompt: "Saved taste",
        refreshing: false,
        items: [Movie(id: "new", mediaType: .show, title: "New Pick")]
    )
    let savedPrompt = Recommendations(
        generatedAt: nil,
        prompt: "Updated taste",
        refreshing: true,
        items: []
    )

    let mergedEmpty = emptyPoll.merged(with: current, source: .refresh)
    #expect(mergedEmpty.items.map(\.id) == ["cached"])
    #expect(mergedEmpty.prompt == "Saved taste")
    #expect(!mergedEmpty.refreshing)

    let mergedOlder = olderPoll.merged(with: current, source: .refresh)
    #expect(olderPoll.isOlderGeneration(than: current))
    #expect(mergedOlder.items.map(\.id) == ["cached"])
    #expect(!mergedOlder.refreshing)

    let mergedNewer = newerPoll.merged(with: current, source: .refresh)
    #expect(!newerPoll.isOlderGeneration(than: current))
    #expect(mergedNewer.items.map(\.id) == ["new"])

    let mergedSave = savedPrompt.merged(with: current, source: .promptSave)
    #expect(mergedSave.prompt == "Updated taste")
    #expect(mergedSave.items.map(\.id) == ["cached"])
    #expect(mergedSave.refreshing)
    #expect(mergedSave.generatedAt == current.generatedAt)
    #expect(!savedPrompt.isOlderGeneration(than: current))
}

@Test func recommendationAPIUsesGetAndPutContractIncludingEmptyPrompt() async throws {
    let recorder = RecommendationRequestRecorder()
    RecommendationURLProtocol.handler = { request in recorder.response(for: request) }
    defer { RecommendationURLProtocol.handler = nil }

    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [RecommendationURLProtocol.self]
    let session = URLSession(configuration: configuration)
    let api = FilmstreamAPI(
        baseURL: URL(string: "https://recommendations.filmstream.test")!,
        session: session
    )

    let fetched = try await api.recommendations()
    let updated = try await api.updateRecommendationPrompt("Warm comedies with quick pacing")
    let cleared = try await api.updateRecommendationPrompt("")

    #expect(fetched.items.first?.title == "Arrival")
    #expect(updated.refreshing)
    #expect(cleared.prompt.isEmpty)
    #expect(recorder.requests.map(\.path) == [
        "/v1/recommendations",
        "/v1/recommendations/prompt",
        "/v1/recommendations/prompt",
    ])
    #expect(recorder.requests.map(\.method) == ["GET", "PUT", "PUT"])
    #expect(recorder.requests[0].body == nil)
    #expect(recorder.requests[1].body?["prompt"] as? String == "Warm comedies with quick pacing")
    #expect(recorder.requests[2].body?["prompt"] as? String == "")

    let malformedAPI = FilmstreamAPI(
        baseURL: URL(string: "https://malformed-recommendations.filmstream.test")!,
        session: session
    )
    var receivedDecodingError = false
    do {
        _ = try await malformedAPI.recommendations()
    } catch let error as FilmstreamError {
        if case .decoding = error {
            receivedDecodingError = true
        }
    }
    #expect(receivedDecodingError)
}

@Test func rejectsMalformedRecommendationResponses() {
    let missingItems = Data(#"{"prompt":"science fiction","refreshing":false}"#.utf8)
    let invalidDate = Data(#"{"generated_at":"yesterday","prompt":"science fiction","refreshing":false,"items":[]}"#.utf8)

    for data in [missingItems, invalidDate] {
        var rejected = false
        do {
            _ = try JSONDecoder().decode(Recommendations.self, from: data)
        } catch {
            rejected = true
        }
        #expect(rejected)
    }
}

private struct RecordedRecommendationRequest: @unchecked Sendable {
    let path: String
    let method: String
    let body: [String: Any]?
}

private final class RecommendationRequestRecorder: @unchecked Sendable {
    private let lock = NSLock()
    private var recordedRequests: [RecordedRecommendationRequest] = []

    var requests: [RecordedRecommendationRequest] {
        lock.withLock { recordedRequests }
    }

    func response(for request: URLRequest) -> (Int, Data) {
        let body = requestBody(request).flatMap { try? JSONSerialization.jsonObject(with: $0) }
            as? [String: Any]
        lock.withLock {
            recordedRequests.append(
                RecordedRecommendationRequest(
                    path: request.url?.path ?? "",
                    method: request.httpMethod ?? "GET",
                    body: body
                )
            )
        }
        if request.url?.host == "malformed-recommendations.filmstream.test" {
            return (200, Data(#"{"prompt":"science fiction","refreshing":false}"#.utf8))
        }
        let prompt = body?["prompt"] as? String ?? "Saved taste"
        let response = #"{"generated_at":"2026-08-21T14:32:10Z","prompt":"\#(prompt)","refreshing":true,"items":[{"id":"tmdb:329865","media_type":"movie","title":"Arrival"}]}"#
        return (200, Data(response.utf8))
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

private final class RecommendationURLProtocol: URLProtocol, @unchecked Sendable {
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
