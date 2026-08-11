import FilmstreamCore
import SwiftUI

struct SearchView: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""
    @State private var results: [Movie] = []
    @State private var isSearching = false
    @State private var selectedMovie: Movie?
    @State private var errorMessage: String?

    private let columns = [GridItem(.adaptive(minimum: 230, maximum: 230), spacing: 38)]

    var body: some View {
        ZStack {
            Color.filmstreamBackground.ignoresSafeArea()
            ScrollView {
                VStack(alignment: .leading, spacing: 36) {
                    HStack(spacing: 18) {
                        Text("Search")
                            .font(.system(size: 48, weight: .bold, design: .rounded))
                        if isSearching, !results.isEmpty {
                            ProgressView()
                        }
                    }

                    content

                    if !results.isEmpty {
                        Text("Movie data and images provided by TMDB.")
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                            .padding(.top, 24)
                    }
                }
                .padding(.horizontal, 76)
                .padding(.vertical, 48)
            }
        }
        .navigationTitle("Search")
        .searchable(text: $query, prompt: "Movie title")
        .navigationDestination(item: $selectedMovie) { movie in
            MovieDetailView(movie: movie)
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
                Text("Finding movies…")
                    .font(.title3)
                    .foregroundStyle(.secondary)
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
                "Find Your Next Movie",
                systemImage: "magnifyingglass",
                description: Text("Type at least two letters of a movie title.")
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
