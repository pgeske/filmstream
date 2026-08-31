import Foundation
import ImageIO
import Observation

public enum ArtworkPreference: Hashable, Sendable {
    case poster
    case backdrop
}

public struct ArtworkRequest: Hashable, Sendable {
    public let urls: [URL]

    public init(
        posterURL: URL?,
        backdropURL: URL?,
        preference: ArtworkPreference
    ) {
        switch preference {
        case .poster:
            self.init(urls: [posterURL, backdropURL].compactMap { $0 })
        case .backdrop:
            self.init(urls: [backdropURL, posterURL].compactMap { $0 })
        }
    }

    public init(urls: [URL]) {
        var seen = Set<URL>()
        self.urls = urls.filter { seen.insert($0).inserted }
    }
}

public struct ArtworkResource: Sendable {
    public let data: Data
    public let sourceURL: URL

    public init(data: Data, sourceURL: URL) {
        self.data = data
        self.sourceURL = sourceURL
    }
}

public enum ArtworkLoadingError: Error, Equatable, Sendable {
    case noCandidates
    case invalidResponse
    case httpStatus(Int)
    case invalidImageData
}

public actor ArtworkLoader {
    public static let shared: ArtworkLoader = {
        let configuration = URLSessionConfiguration.default
        configuration.requestCachePolicy = .returnCacheDataElseLoad
        configuration.urlCache = URLCache(
            memoryCapacity: 32 * 1_024 * 1_024,
            diskCapacity: 256 * 1_024 * 1_024
        )
        configuration.httpMaximumConnectionsPerHost = 6
        configuration.timeoutIntervalForRequest = 20
        return ArtworkLoader(session: URLSession(configuration: configuration))
    }()

    private struct InFlightRequest {
        let id: UUID
        let task: Task<Data, Error>
        var consumers: Set<UUID>
    }

    private struct RequestTicket: Sendable {
        let requestID: UUID
        let consumerID: UUID
        let task: Task<Data, Error>
        let url: URL
    }

    private let session: URLSession
    private let retryDelayNanoseconds: UInt64
    private let memoryCache = NSCache<NSURL, NSData>()
    private var inFlightRequests: [URL: InFlightRequest] = [:]

    public init(
        session: URLSession,
        memoryCapacity: Int = 64 * 1_024 * 1_024,
        retryDelayNanoseconds: UInt64 = 350_000_000
    ) {
        self.session = session
        self.retryDelayNanoseconds = retryDelayNanoseconds
        memoryCache.totalCostLimit = memoryCapacity
    }

    public func artwork(for request: ArtworkRequest) async throws -> ArtworkResource {
        var lastError: Error = ArtworkLoadingError.noCandidates

        for url in request.urls {
            do {
                return ArtworkResource(data: try await data(for: url), sourceURL: url)
            } catch is CancellationError {
                throw CancellationError()
            } catch let error as URLError where error.code == .cancelled && Task.isCancelled {
                throw CancellationError()
            } catch {
                lastError = error
            }
        }

        throw lastError
    }

    private func data(for url: URL) async throws -> Data {
        try Task.checkCancellation()
        if let data = memoryCache.object(forKey: url as NSURL) {
            return data as Data
        }

        let ticket = ticket(for: url)
        return try await withTaskCancellationHandler {
            do {
                let data = try await ticket.task.value
                try Task.checkCancellation()
                complete(ticket, with: data)
                return data
            } catch {
                release(ticket, cancelIfUnused: false)
                if Task.isCancelled {
                    throw CancellationError()
                }
                throw error
            }
        } onCancel: {
            Task {
                await self.release(ticket, cancelIfUnused: true)
            }
        }
    }

    private func ticket(for url: URL) -> RequestTicket {
        let consumerID = UUID()
        if var request = inFlightRequests[url] {
            request.consumers.insert(consumerID)
            inFlightRequests[url] = request
            return RequestTicket(
                requestID: request.id,
                consumerID: consumerID,
                task: request.task,
                url: url
            )
        }

        let requestID = UUID()
        let session = session
        let retryDelayNanoseconds = retryDelayNanoseconds
        let task = Task<Data, Error>.detached(priority: .utility) {
            try await Self.fetch(
                url: url,
                session: session,
                retryDelayNanoseconds: retryDelayNanoseconds
            )
        }
        inFlightRequests[url] = InFlightRequest(
            id: requestID,
            task: task,
            consumers: [consumerID]
        )
        return RequestTicket(
            requestID: requestID,
            consumerID: consumerID,
            task: task,
            url: url
        )
    }

    private func complete(_ ticket: RequestTicket, with data: Data) {
        guard inFlightRequests[ticket.url]?.id == ticket.requestID else { return }
        memoryCache.setObject(data as NSData, forKey: ticket.url as NSURL, cost: data.count)
        inFlightRequests.removeValue(forKey: ticket.url)
    }

    private func release(_ ticket: RequestTicket, cancelIfUnused: Bool) {
        guard var request = inFlightRequests[ticket.url], request.id == ticket.requestID else {
            return
        }
        request.consumers.remove(ticket.consumerID)
        guard request.consumers.isEmpty else {
            inFlightRequests[ticket.url] = request
            return
        }
        if cancelIfUnused {
            request.task.cancel()
        }
        inFlightRequests.removeValue(forKey: ticket.url)
    }

    private nonisolated static func fetch(
        url: URL,
        session: URLSession,
        retryDelayNanoseconds: UInt64
    ) async throws -> Data {
        var lastError: Error = ArtworkLoadingError.invalidResponse

        for attempt in 0..<2 {
            do {
                var request = URLRequest(
                    url: url,
                    cachePolicy: attempt == 0
                        ? .returnCacheDataElseLoad
                        : .reloadIgnoringLocalCacheData,
                    timeoutInterval: 20
                )
                request.setValue(
                    "image/avif,image/webp,image/*,*/*;q=0.8",
                    forHTTPHeaderField: "Accept"
                )
                let (data, response) = try await session.data(for: request)
                guard let response = response as? HTTPURLResponse else {
                    throw ArtworkLoadingError.invalidResponse
                }
                guard (200..<300).contains(response.statusCode) else {
                    throw ArtworkLoadingError.httpStatus(response.statusCode)
                }
                guard isDecodableImage(data) else {
                    throw ArtworkLoadingError.invalidImageData
                }
                return data
            } catch is CancellationError {
                throw CancellationError()
            } catch let error as URLError where error.code == .cancelled && Task.isCancelled {
                throw CancellationError()
            } catch {
                lastError = error
                guard attempt == 0, isTransient(error) else { throw error }
                if retryDelayNanoseconds > 0 {
                    try await Task.sleep(nanoseconds: retryDelayNanoseconds)
                }
            }
        }

        throw lastError
    }

    private nonisolated static func isDecodableImage(_ data: Data) -> Bool {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil),
              CGImageSourceGetCount(source) > 0 else {
            return false
        }
        return CGImageSourceCreateImageAtIndex(source, 0, nil) != nil
    }

    private nonisolated static func isTransient(_ error: Error) -> Bool {
        if let error = error as? ArtworkLoadingError {
            switch error {
            case .invalidResponse, .invalidImageData:
                return true
            case let .httpStatus(status):
                return status == 408 || status == 425 || status == 429 || (500...599).contains(status)
            case .noCandidates:
                return false
            }
        }
        guard let error = error as? URLError else { return false }
        return [
            .cannotConnectToHost,
            .cannotFindHost,
            .dnsLookupFailed,
            .networkConnectionLost,
            .notConnectedToInternet,
            .resourceUnavailable,
            .timedOut,
        ].contains(error.code)
    }
}

@MainActor
@Observable
public final class ArtworkLoadState {
    public private(set) var resource: ArtworkResource?
    public private(set) var isLoading = false
    public private(set) var hasFailed = false

    @ObservationIgnored private var activeLoadID: UUID?

    public init() {}

    public func load(
        _ request: ArtworkRequest,
        using loader: ArtworkLoader = .shared
    ) async {
        let loadID = UUID()
        activeLoadID = loadID

        if let resource, !request.urls.contains(resource.sourceURL) {
            self.resource = nil
        }
        if resource?.sourceURL == request.urls.first {
            isLoading = false
            hasFailed = false
            activeLoadID = nil
            return
        }

        isLoading = resource == nil
        hasFailed = false
        defer {
            if activeLoadID == loadID {
                isLoading = false
                activeLoadID = nil
            }
        }

        do {
            let resource = try await loader.artwork(for: request)
            guard activeLoadID == loadID, !Task.isCancelled else { return }
            self.resource = resource
        } catch is CancellationError {
            return
        } catch {
            guard activeLoadID == loadID else { return }
            hasFailed = resource == nil
        }
    }
}
