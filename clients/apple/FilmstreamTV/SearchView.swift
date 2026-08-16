import FilmstreamCore
import SwiftUI

struct SearchView: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""
    @State private var results: [Movie] = []
    @State private var isSearching = false
    @State private var selectedMovie: Movie?
    @State private var errorMessage: String?

    private let columns = [GridItem(.adaptive(minimum: 250, maximum: 250), spacing: 48)]

    var body: some View {
        ZStack {
            TeaBackground()
            ScrollView {
                VStack(alignment: .leading, spacing: 36) {
                    HStack(spacing: 18) {
                        TeaStreamMark(size: 52)
                        VStack(alignment: .leading, spacing: 4) {
                            Text("Find Your Next Story")
                                .font(.system(size: 46, weight: .bold, design: .rounded))
                                .foregroundStyle(Color.teaCream)
                            Text("A cozy night starts here")
                                .font(.headline)
                                .foregroundStyle(Color.teaMuted)
                        }
                        if isSearching, !results.isEmpty {
                            ProgressView()
                                .tint(Color.teaAccent)
                        }
                    }

                    content

                    if !results.isEmpty {
                        Text("Media data and images provided by TMDB.")
                            .font(.footnote)
                            .foregroundStyle(Color.teaMuted)
                            .padding(.top, 24)
                    }
                }
                .padding(.horizontal, 76)
                .padding(.vertical, 48)
            }
        }
        .navigationTitle("Search")
        .searchable(text: $query, prompt: "Movie or show title")
        .tint(Color.teaAccent)
        .navigationDestination(item: $selectedMovie) { movie in
            if movie.isShow {
                ShowDetailView(show: movie)
            } else {
                MovieDetailView(movie: movie)
            }
        }
        .task(id: query) {
            await searchAfterTypingPause()
        }
    }

    @ViewBuilder
    private var content: some View {
        if !results.isEmpty {
            LazyVGrid(columns: columns, alignment: .leading, spacing: 44) {
                ForEach(results) { movie in
                    SearchResultCard(movie: movie) {
                        selectedMovie = movie
                    }
                }
            }
            .padding(.vertical, 20)
        } else if isSearching {
            HStack(spacing: 18) {
                ProgressView()
                    .tint(Color.teaAccent)
                Text("Steeping the perfect results…")
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
            }
            .frame(maxWidth: .infinity, minHeight: 340)
        } else if let errorMessage {
            ContentUnavailableView(
                "Search Failed",
                systemImage: "wifi.exclamationmark",
                description: Text(errorMessage)
            )
            .frame(minHeight: 340)
        } else if trimmedQuery.count < 2 {
            ContentUnavailableView(
                "Search by Title",
                systemImage: "magnifyingglass",
                description: Text("Type at least two letters of a movie or show title.")
            )
            .frame(minHeight: 340)
        } else {
            ContentUnavailableView.search(text: trimmedQuery)
                .frame(minHeight: 340)
        }
    }

    private var trimmedQuery: String {
        query.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func searchAfterTypingPause() async {
        let submittedQuery = trimmedQuery
        guard submittedQuery.count >= 2 else {
            results = []
            errorMessage = nil
            isSearching = false
            return
        }

        do {
            try await Task.sleep(for: .milliseconds(350))
        } catch {
            return
        }
        guard !Task.isCancelled else { return }

        isSearching = true
        errorMessage = nil
        do {
            let movies = try await model.api.search(submittedQuery)
            guard !Task.isCancelled, submittedQuery == trimmedQuery else { return }
            results = movies
            isSearching = false
        } catch {
            guard !Task.isCancelled, submittedQuery == trimmedQuery else { return }
            results = []
            isSearching = false
            errorMessage = error.localizedDescription
        }
    }
}

private struct SearchResultCard: View {
    @Environment(\.dismissSearch) private var dismissSearch
    @Environment(\.isSearching) private var isSearching

    let movie: Movie
    let onSelect: () -> Void

    var body: some View {
        MovieNavigationCard(movie: movie) {
            guard isSearching else {
                onSelect()
                return
            }
            dismissSearch()
            Task { @MainActor in
                // Let tvOS remove its search presentation before pushing the detail view.
                try? await Task.sleep(for: .milliseconds(150))
                guard !Task.isCancelled else { return }
                onSelect()
            }
        }
    }
}
