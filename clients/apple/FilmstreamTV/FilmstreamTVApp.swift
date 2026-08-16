import FilmstreamCore
import Observation
import SwiftUI

@main
struct FilmstreamTVApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        WindowGroup {
            HomeView()
                .environment(model)
                .preferredColorScheme(.dark)
        }
    }
}

@MainActor
@Observable
final class AppModel {
    let api: FilmstreamAPI
    var continueWatching: [WatchHistoryEntry] = []
    var watchHistory: [WatchHistoryEntry] = []
    var discoverySections: [DiscoverySection] = []
    private(set) var ratingsByMovieID: [String: MovieRatings] = [:]
    private var requestedRatingIDs: Set<String> = []
    var isLoading = false
    var errorMessage: String?

    init(api: FilmstreamAPI = FilmstreamAPI(baseURL: AppConfiguration.serverURL)) {
        self.api = api
    }

    func loadHome() async {
        isLoading = true
        defer { isLoading = false }

        async let continueRequest = api.continueWatching()
        async let historyRequest = api.watchHistory()
        async let discoveryRequest = api.discover()
        var errors: [String] = []
        do {
            continueWatching = try await continueRequest
        } catch {
            errors.append(error.localizedDescription)
        }
        do {
            watchHistory = try await historyRequest
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
            async let continueRequest = api.continueWatching()
            async let historyRequest = api.watchHistory()
            continueWatching = try await continueRequest
            watchHistory = try await historyRequest
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    func removeFromContinueWatching(_ entry: WatchHistoryEntry) async throws {
        try await api.removeFromContinueWatching(entry)
        withAnimation(.snappy(duration: 0.25)) {
            if let seriesID = entry.seriesID {
                continueWatching.removeAll { $0.seriesID == seriesID }
                watchHistory.removeAll { $0.seriesID == seriesID }
            } else {
                continueWatching.removeAll { $0.id == entry.id }
                watchHistory.removeAll { $0.id == entry.id }
            }
        }
    }

    func prewarmPlayback(for movie: Movie, startSeconds: Double) async {
        try? await api.prewarmPlayback(for: movie, startSeconds: startSeconds)
    }

    func preparePlayback(
        for movie: Movie,
        startSeconds: Double,
        onStage: (PlaybackPreparationStage) -> Void
    ) async throws -> PreparedPlayback {
        onStage(.findingRelease)
        let playback = try await api.createPlayback(for: movie, startSeconds: startSeconds)
        onStage(.bufferingVideo)
        return try await api.prepareNativePlayback(playback, startSeconds: startSeconds)
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
        if movie.isShow {
            return continueWatching.first { $0.seriesID == movie.id }
        }
        return watchHistory.first {
            guard !$0.completed, $0.positionSeconds >= 30 else { return false }
            if let mediaID = $0.mediaID {
                return mediaID == movie.id
            }
            return $0.title.caseInsensitiveCompare(movie.title) == .orderedSame && $0.year == movie.year
        }
    }

    func histories(for show: Movie) -> [WatchHistoryEntry] {
        watchHistory.filter { $0.seriesID == show.id }
    }
}

enum AppConfiguration {
    static let serverURL: URL = {
        if let value = Bundle.main.object(forInfoDictionaryKey: "FilmstreamServerURL") as? String,
           let url = URL(string: value) {
            return url
        }
        return URL(string: "http://filmstream.home.alyoshukai.com")!
    }()
}
