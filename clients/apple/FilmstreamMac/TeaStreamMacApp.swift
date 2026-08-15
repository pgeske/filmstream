import FilmstreamCore
import Observation
import SwiftUI

@main
struct TeaStreamMacApp: App {
    @State private var model = MacAppModel()

    var body: some Scene {
        WindowGroup {
            MacHomeView()
                .environment(model)
                .preferredColorScheme(.dark)
                .frame(minWidth: 900, minHeight: 620)
        }
        .defaultSize(width: 1_180, height: 780)
        .windowToolbarStyle(.unifiedCompact)
    }
}

@MainActor
@Observable
final class MacAppModel {
    let api: FilmstreamAPI
    var continueWatching: [WatchHistoryEntry] = []
    var discoverySections: [DiscoverySection] = []
    private(set) var ratingsByMovieID: [String: MovieRatings] = [:]
    private var requestedRatingIDs: Set<String> = []
    var isLoading = false
    var errorMessage: String?

    init(api: FilmstreamAPI = FilmstreamAPI(baseURL: MacAppConfiguration.serverURL)) {
        self.api = api
    }

    func loadHome() async {
        isLoading = true
        defer { isLoading = false }

        async let historyRequest = api.continueWatching()
        async let discoveryRequest = api.discover()
        var errors: [String] = []
        do {
            continueWatching = try await historyRequest
        } catch {
            errors.append(error.localizedDescription)
        }
        do {
            discoverySections = try await discoveryRequest
        } catch {
            errors.append(error.localizedDescription)
        }
        errorMessage = errors.first
    }

    func loadContinueWatching() async {
        do {
            continueWatching = try await api.continueWatching()
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    func removeFromContinueWatching(_ entry: WatchHistoryEntry) async throws {
        try await api.removeFromContinueWatching(entry)
        withAnimation(.snappy(duration: 0.25)) {
            continueWatching.removeAll { $0.id == entry.id }
        }
    }

    func ratings(for movie: Movie) -> MovieRatings? {
        ratingsByMovieID[movie.id]
    }

    func loadRatings(for movie: Movie) async {
        guard requestedRatingIDs.insert(movie.id).inserted else { return }
        do {
            let ratings = try await api.ratings(for: movie)
            if !ratings.isEmpty {
                ratingsByMovieID[movie.id] = ratings
            }
        } catch {
            requestedRatingIDs.remove(movie.id)
            // External ratings are optional and should never interrupt browsing.
        }
    }

    func history(for movie: Movie) -> WatchHistoryEntry? {
        continueWatching.first {
            if let mediaID = $0.mediaID {
                return mediaID == movie.id
            }
            return $0.title.caseInsensitiveCompare(movie.title) == .orderedSame && $0.year == movie.year
        }
    }
}

enum MacAppConfiguration {
    static let serverURL: URL = {
        if let value = Bundle.main.object(forInfoDictionaryKey: "FilmstreamServerURL") as? String,
           let url = URL(string: value) {
            return url
        }
        return URL(string: "http://filmstream.home.alyoshukai.com")!
    }()
}
