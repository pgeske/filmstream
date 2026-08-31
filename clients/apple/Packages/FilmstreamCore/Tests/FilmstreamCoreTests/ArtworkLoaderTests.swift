import Foundation
import Testing
@testable import FilmstreamCore

@Suite(.serialized)
struct ArtworkLoaderTests {
    @Test @MainActor
    func unfocusedArtworkSurvivesFocusLayoutTransitions() async throws {
        let counter = ArtworkRequestCounter()
        ArtworkURLProtocol.stub.reset { request in
            let count = counter.increment()
            return ArtworkStubResponse(
                status: 200,
                data: validPNGData,
                delay: request.url?.path == "/backdrop" && count == 2 ? 0.12 : 0
            )
        }
        let loader = makeLoader()
        let posterURL = URL(string: "https://artwork.test/poster")!
        let backdropURL = URL(string: "https://artwork.test/backdrop")!
        let portrait = ArtworkRequest(
            posterURL: posterURL,
            backdropURL: backdropURL,
            preference: .poster
        )
        let landscape = ArtworkRequest(
            posterURL: posterURL,
            backdropURL: backdropURL,
            preference: .backdrop
        )
        let state = ArtworkLoadState()

        await state.load(portrait, using: loader)
        #expect(state.resource?.sourceURL == posterURL)
        #expect(!state.isLoading)

        // Replacing the movie model with unchanged artwork URLs must not reset successful state.
        await state.load(portrait, using: loader)
        #expect(state.resource?.sourceURL == posterURL)
        #expect(counter.current == 1)

        let expansion = Task { @MainActor in
            await state.load(landscape, using: loader)
        }
        try await waitUntil { counter.current == 2 }
        #expect(state.resource?.sourceURL == posterURL)
        #expect(!state.isLoading)

        await expansion.value
        #expect(state.resource?.sourceURL == backdropURL)

        await state.load(portrait, using: loader)
        #expect(state.resource?.sourceURL == posterURL)
        #expect(counter.current == 2)
    }

    @Test
    func transientFailureRetriesOnce() async throws {
        let counter = ArtworkRequestCounter()
        ArtworkURLProtocol.stub.reset { _ in
            if counter.increment() == 1 {
                return ArtworkStubResponse(status: 503, data: Data())
            }
            return ArtworkStubResponse(status: 200, data: validPNGData)
        }
        let loader = makeLoader()

        let resource = try await loader.artwork(
            for: ArtworkRequest(urls: [URL(string: "https://artwork.test/transient")!])
        )

        #expect(resource.data == validPNGData)
        #expect(counter.current == 2)
    }

    @Test
    func failedPosterFallsBackToBackdrop() async throws {
        let counter = ArtworkRequestCounter()
        ArtworkURLProtocol.stub.reset { request in
            _ = counter.increment()
            if request.url?.path == "/poster" {
                return ArtworkStubResponse(status: 404, data: Data())
            }
            return ArtworkStubResponse(status: 200, data: validPNGData)
        }
        let loader = makeLoader()
        let posterURL = URL(string: "https://artwork.test/poster")!
        let backdropURL = URL(string: "https://artwork.test/backdrop")!

        let resource = try await loader.artwork(
            for: ArtworkRequest(
                posterURL: posterURL,
                backdropURL: backdropURL,
                preference: .poster
            )
        )

        #expect(resource.sourceURL == backdropURL)
        #expect(counter.current == 2)
    }

    @Test
    func recreatedConsumersShareInFlightRequestAndMemoryCache() async throws {
        let counter = ArtworkRequestCounter()
        ArtworkURLProtocol.stub.reset { _ in
            _ = counter.increment()
            return ArtworkStubResponse(status: 200, data: validPNGData, delay: 0.12)
        }
        let loader = makeLoader()
        let request = ArtworkRequest(
            urls: [URL(string: "https://artwork.test/shared")!]
        )

        async let first = loader.artwork(for: request)
        async let second = loader.artwork(for: request)
        let (firstResource, secondResource) = try await (first, second)
        #expect(firstResource.data == secondResource.data)
        #expect(counter.current == 1)

        let recreatedResource = try await loader.artwork(for: request)
        #expect(recreatedResource.data == validPNGData)
        #expect(counter.current == 1)
    }

    @Test
    func cancellationOnlyStopsRequestAfterLastConsumerLeaves() async throws {
        let counter = ArtworkRequestCounter()
        ArtworkURLProtocol.stub.reset { _ in
            _ = counter.increment()
            return ArtworkStubResponse(status: 200, data: validPNGData, delay: 0.2)
        }
        let loader = makeLoader()
        let request = ArtworkRequest(
            urls: [URL(string: "https://artwork.test/cancellation")!]
        )
        let first = Task { try await loader.artwork(for: request) }
        let second = Task { try await loader.artwork(for: request) }

        try await waitUntil { counter.current == 1 }
        first.cancel()
        let secondResource = try await second.value
        #expect(secondResource.data == validPNGData)
        #expect(counter.current == 1)
        #expect(ArtworkURLProtocol.stub.stopCount == 0)
        do {
            _ = try await first.value
            Issue.record("The cancelled consumer unexpectedly completed")
        } catch is CancellationError {
            // Expected: the remaining consumer kept the shared request alive.
        }

        ArtworkURLProtocol.stub.reset { _ in
            _ = counter.increment()
            return ArtworkStubResponse(status: 200, data: validPNGData, delay: 2)
        }
        let soleRequest = ArtworkRequest(
            urls: [URL(string: "https://artwork.test/sole-cancellation")!]
        )
        let soleConsumer = Task { try await loader.artwork(for: soleRequest) }
        try await waitUntil { counter.current == 2 }
        soleConsumer.cancel()
        do {
            _ = try await soleConsumer.value
            Issue.record("The sole cancelled consumer unexpectedly completed")
        } catch is CancellationError {
            // Expected: no consumer remained to keep the request alive.
        }
        try await waitUntil { ArtworkURLProtocol.stub.stopCount == 1 }
    }
}

private let validPNGData = Data(
    base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)!

private func makeLoader() -> ArtworkLoader {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [ArtworkURLProtocol.self]
    return ArtworkLoader(
        session: URLSession(configuration: configuration),
        retryDelayNanoseconds: 0
    )
}

private func waitUntil(
    timeout: Duration = .seconds(1),
    condition: @escaping @Sendable () -> Bool
) async throws {
    let clock = ContinuousClock()
    let deadline = clock.now.advanced(by: timeout)
    while !condition() {
        guard clock.now < deadline else {
            throw ArtworkTestError.timedOut
        }
        try await Task.sleep(for: .milliseconds(10))
    }
}

private enum ArtworkTestError: Error {
    case timedOut
}

private struct ArtworkStubResponse: Sendable {
    let status: Int
    let data: Data
    let delay: TimeInterval

    init(status: Int, data: Data, delay: TimeInterval = 0) {
        self.status = status
        self.data = data
        self.delay = delay
    }
}

private final class ArtworkURLProtocolStub: @unchecked Sendable {
    private let lock = NSLock()
    private var handler: @Sendable (URLRequest) -> ArtworkStubResponse = { _ in
        ArtworkStubResponse(status: 500, data: Data())
    }
    private var stops = 0

    var stopCount: Int {
        lock.withLock { stops }
    }

    func reset(handler: @escaping @Sendable (URLRequest) -> ArtworkStubResponse) {
        lock.withLock {
            self.handler = handler
            stops = 0
        }
    }

    func response(for request: URLRequest) -> ArtworkStubResponse {
        lock.withLock { handler(request) }
    }

    func recordStop() {
        lock.withLock { stops += 1 }
    }
}

private final class ArtworkURLProtocol: URLProtocol, @unchecked Sendable {
    static let stub = ArtworkURLProtocolStub()

    private let lock = NSLock()
    private var workItem: DispatchWorkItem?
    private var finished = false

    override class func canInit(with request: URLRequest) -> Bool { true }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let url = request.url else {
            client?.urlProtocol(self, didFailWithError: URLError(.badURL))
            return
        }
        let response = Self.stub.response(for: request)
        let workItem = DispatchWorkItem { [weak self] in
            guard let self, !self.isFinished else { return }
            let httpResponse = HTTPURLResponse(
                url: url,
                statusCode: response.status,
                httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "image/png"]
            )!
            client?.urlProtocol(self, didReceive: httpResponse, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: response.data)
            client?.urlProtocolDidFinishLoading(self)
            markFinished()
        }
        lock.withLock { self.workItem = workItem }
        if response.delay == 0 {
            workItem.perform()
        } else {
            DispatchQueue.global().asyncAfter(deadline: .now() + response.delay, execute: workItem)
        }
    }

    override func stopLoading() {
        let shouldRecord = lock.withLock {
            guard !finished else { return false }
            finished = true
            workItem?.cancel()
            return true
        }
        if shouldRecord {
            Self.stub.recordStop()
        }
    }

    private var isFinished: Bool {
        lock.withLock { finished }
    }

    private func markFinished() {
        lock.withLock { finished = true }
    }
}

private final class ArtworkRequestCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0

    var current: Int {
        lock.withLock { count }
    }

    @discardableResult
    func increment() -> Int {
        lock.withLock {
            count += 1
            return count
        }
    }
}
