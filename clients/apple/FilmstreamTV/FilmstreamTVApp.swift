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
    var isLoading = false
    var errorMessage: String?

    init(api: FilmstreamAPI = FilmstreamAPI(baseURL: AppConfiguration.serverURL)) {
        self.api = api
    }

    func loadContinueWatching() async {
        isLoading = true
        defer { isLoading = false }
        do {
            continueWatching = try await api.continueWatching()
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
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

enum AppConfiguration {
    static let serverURL: URL = {
        if let value = Bundle.main.object(forInfoDictionaryKey: "FilmstreamServerURL") as? String,
           let url = URL(string: value) {
            return url
        }
        return URL(string: "http://filmstream.home.alyoshukai.com")!
    }()
}
