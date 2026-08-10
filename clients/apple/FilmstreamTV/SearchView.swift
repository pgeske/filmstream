import FilmstreamCore
import SwiftUI

struct SearchView: View {
    @Environment(AppModel.self) private var model
    @State private var query = ""
    @State private var results: [Movie] = []
    @State private var isSearching = false
    @State private var errorMessage: String?

    private let columns = [GridItem(.adaptive(minimum: 230, maximum: 230), spacing: 38)]

    var body: some View {
        ZStack {
            Color.filmstreamBackground.ignoresSafeArea()
            ScrollView {
                VStack(alignment: .leading, spacing: 36) {
                    Text("Search")
                        .font(.system(size: 48, weight: .bold, design: .rounded))

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
        .searchable(text: $query, prompt: "Movie title or description")
        .onSubmit(of: .search) {
            Task { await search() }
        }
    }

    @ViewBuilder
    private var content: some View {
        if isSearching {
            HStack(spacing: 18) {
                ProgressView()
                Text("Searching Filmstream…")
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
        } else if query.isEmpty && results.isEmpty {
            ContentUnavailableView(
                "Find Your Next Movie",
                systemImage: "magnifyingglass",
                description: Text("Search by title, or describe the movie you remember.")
            )
            .frame(minHeight: 340)
        } else if results.isEmpty {
            ContentUnavailableView.search(text: query)
                .frame(minHeight: 340)
        } else {
            LazyVGrid(columns: columns, alignment: .leading, spacing: 44) {
                ForEach(results) { movie in
                    MovieNavigationCard(movie: movie)
                }
            }
            .padding(.vertical, 20)
        }
    }

    private func search() async {
        let trimmedQuery = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedQuery.isEmpty else { return }
        isSearching = true
        defer { isSearching = false }
        do {
            results = try await model.api.search(trimmedQuery)
            errorMessage = nil
        } catch {
            results = []
            errorMessage = error.localizedDescription
        }
    }
}
