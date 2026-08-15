import FilmstreamCore
import SwiftUI

struct MacSearchView: View {
    @Environment(MacAppModel.self) private var model
    @State private var query = ""
    @State private var results: [Movie] = []
    @State private var isSearching = false
    @State private var errorMessage: String?

    private let columns = [GridItem(.adaptive(minimum: 176, maximum: 196), spacing: 24)]

    var body: some View {
        ZStack {
            MacTeaBackground()
            ScrollView {
                VStack(alignment: .leading, spacing: 28) {
                    HStack(spacing: 15) {
                        MacTeaStreamMark(size: 44)
                        VStack(alignment: .leading, spacing: 3) {
                            Text("Find Your Next Movie")
                                .font(.system(size: 30, weight: .bold, design: .rounded))
                                .foregroundStyle(Color.macTeaCream)
                            Text("A cozy movie night starts here")
                                .foregroundStyle(Color.macTeaMuted)
                        }
                        if isSearching, !results.isEmpty {
                            ProgressView()
                                .controlSize(.small)
                                .tint(Color.macTeaAccent)
                        }
                    }

                    content

                    if !results.isEmpty {
                        Text("Movie data and images provided by TMDB.")
                            .font(.footnote)
                            .foregroundStyle(Color.macTeaMuted)
                    }
                }
                .padding(34)
            }
        }
        .navigationTitle("Search")
        .searchable(text: $query, placement: .toolbar, prompt: "Movie title")
        .tint(Color.macTeaAccent)
        .task(id: query) {
            await searchAfterTypingPause()
        }
    }

    @ViewBuilder
    private var content: some View {
        if !results.isEmpty {
            LazyVGrid(columns: columns, alignment: .leading, spacing: 28) {
                ForEach(results) { movie in
                    MacMovieCard(movie: movie)
                }
            }
            .padding(.vertical, 8)
        } else if isSearching {
            HStack(spacing: 12) {
                ProgressView()
                    .tint(Color.macTeaAccent)
                Text("Steeping the perfect results…")
                    .foregroundStyle(Color.macTeaMuted)
            }
            .frame(maxWidth: .infinity, minHeight: 320)
        } else if let errorMessage {
            ContentUnavailableView(
                "Search Failed",
                systemImage: "wifi.exclamationmark",
                description: Text(errorMessage)
            )
            .foregroundStyle(Color.macTeaCream)
            .frame(maxWidth: .infinity, minHeight: 320)
        } else if trimmedQuery.count < 2 {
            ContentUnavailableView(
                "Search by Title",
                systemImage: "magnifyingglass",
                description: Text("Type at least two letters of a movie title.")
            )
            .foregroundStyle(Color.macTeaCream)
            .frame(maxWidth: .infinity, minHeight: 320)
        } else {
            ContentUnavailableView.search(text: trimmedQuery)
                .foregroundStyle(Color.macTeaCream)
                .frame(maxWidth: .infinity, minHeight: 320)
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
            try await Task.sleep(for: .milliseconds(300))
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
