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
    var watchHistory: [WatchHistoryEntry] = []
    var discoverySections: [DiscoverySection] = []
    var recommendations: Recommendations?
    var recommendationErrorMessage: String?
    var activePlayback: MacPlaybackSession?
    private(set) var ratingsByMovieID: [String: MovieRatings] = [:]
    private var requestedRatingIDs: Set<String> = []
    private let recommendationPoller = RecommendationPoller()
    private var recommendationOperationID = 0
    var isLoading = false
    var errorMessage: String?

    init(api: FilmstreamAPI = FilmstreamAPI(baseURL: MacAppConfiguration.serverURL)) {
        self.api = api
    }

    func loadHome() async {
        isLoading = true
        defer { isLoading = false }

        let recommendationOperationID = self.recommendationOperationID
        async let continueRequest = api.continueWatching()
        async let historyRequest = api.watchHistory()
        async let discoveryRequest = api.discover()
        async let recommendationRequest = api.recommendations()
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
        do {
            let response = try await recommendationRequest
            if recommendationOperationID == self.recommendationOperationID {
                applyRecommendations(response, source: .refresh)
                recommendationErrorMessage = nil
                scheduleRecommendationPolling(
                    ifNeeded: recommendations?.refreshing == true,
                    operationID: recommendationOperationID
                )
            }
        } catch {
            if recommendationOperationID == self.recommendationOperationID {
                recommendationErrorMessage = error.localizedDescription
                errors.append(error.localizedDescription)
            }
        }
        errorMessage = errors.first
    }

    func updateRecommendationPrompt(_ prompt: String) async throws {
        recommendationOperationID += 1
        let operationID = recommendationOperationID
        recommendationPoller.cancel()
        recommendations = recommendations?.settingRefreshing(false)

        let previousError = recommendationErrorMessage
        let response = try await api.updateRecommendationPrompt(prompt)
        guard operationID == recommendationOperationID else { return }

        applyRecommendations(response, source: .promptSave)
        if errorMessage == previousError {
            errorMessage = nil
        }
        recommendationErrorMessage = nil
        scheduleRecommendationPolling(
            ifNeeded: recommendations?.refreshing == true,
            operationID: operationID
        )
    }

    private func applyRecommendations(
        _ response: Recommendations,
        source: RecommendationUpdateSource
    ) {
        recommendations = response.merged(with: recommendations, source: source)
    }

    private func scheduleRecommendationPolling(ifNeeded: Bool, operationID: Int) {
        guard ifNeeded else {
            recommendationPoller.cancel()
            return
        }

        recommendationPoller.start(
            fetch: { [weak self] in
                guard let self, operationID == self.recommendationOperationID else {
                    throw CancellationError()
                }
                return try await self.api.recommendations()
            },
            receive: { [weak self] response in
                guard let self, operationID == self.recommendationOperationID else { return false }
                let previousError = self.recommendationErrorMessage
                self.applyRecommendations(response, source: .refresh)
                if self.errorMessage == previousError {
                    self.errorMessage = nil
                }
                self.recommendationErrorMessage = nil
                return self.recommendations?.refreshing == true
            },
            onTimeout: { [weak self] in
                guard let self,
                      operationID == self.recommendationOperationID,
                      let recommendations = self.recommendations else { return }
                self.recommendations = recommendations.settingRefreshing(false)
            }
        )
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
        return try await api.prepareNativePlaybackWithRetry(
            playback,
            for: movie,
            startSeconds: startSeconds
        )
    }

    func presentPlayback(
        movie: Movie,
        prepared: PreparedPlayback,
        details: SeriesDetails? = nil,
        nextEpisode: Episode? = nil
    ) {
        activePlayback = MacPlaybackSession(
            movie: movie,
            prepared: prepared,
            details: details,
            nextEpisode: nextEpisode
        )
    }

    func advancePlayback(to episode: Episode) async throws {
        guard let details = activePlayback?.details else { return }
        let movie = episode.playbackMovie(in: details.show)
        let nextEpisodeTask = Task {
            try? await api.nextEpisode(after: episode, in: details)
        }
        let prepared = try await preparePlayback(
            for: movie,
            startSeconds: 0,
            onStage: { _ in }
        )
        presentPlayback(
            movie: movie,
            prepared: prepared,
            details: details,
            nextEpisode: await nextEpisodeTask.value
        )
    }

    func dismissPlayback() {
        activePlayback = nil
        Task { await loadContinueWatching() }
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

struct MacPlaybackSession: Identifiable {
    let movie: Movie
    let prepared: PreparedPlayback
    let details: SeriesDetails?
    let nextEpisode: Episode?

    var id: String { prepared.playback.id }
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
