import FilmstreamCore
import SwiftUI

struct IOSSearchView: View {
    @Environment(IOSAppModel.self) private var model
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @State private var query = ""
    @State private var results: [Movie] = []
    @State private var isSearching = false
    @State private var errorMessage: String?

    private let columns = [
        GridItem(.adaptive(minimum: 150, maximum: 230), spacing: 18),
    ]

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    HStack(spacing: 12) {
                        MobileTeaStreamMark(size: 42)
                        VStack(alignment: .leading, spacing: 2) {
                            Text("Find Your Next Story")
                                .font(.title2.weight(.bold))
                                .foregroundStyle(Color.mobileTeaCream)
                            Text("A cozy night starts here")
                                .font(.subheadline)
                                .foregroundStyle(Color.mobileTeaMuted)
                        }
                    }

                    content

                    if !results.isEmpty {
                        Text("Media data and images provided by TMDB.")
                            .font(.caption2)
                            .foregroundStyle(Color.mobileTeaMuted.opacity(0.75))
                    }
                }
                .frame(maxWidth: 1_120, alignment: .leading)
                .padding(.horizontal, horizontalSizeClass == .regular ? 30 : 18)
                .padding(.top, 16)
                .padding(.bottom, 32)
                .frame(maxWidth: .infinity)
            }
        }
        .navigationTitle("Search")
        .navigationBarTitleDisplayMode(.inline)
        .searchable(text: $query, prompt: "Movie or show title")
        .tint(Color.mobileTeaAccent)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.94), for: .navigationBar)
        .toolbarBackground(.visible, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
        .task(id: query) {
            await searchAfterTypingPause()
        }
    }

    @ViewBuilder
    private var content: some View {
        if !results.isEmpty {
            LazyVGrid(columns: columns, alignment: .leading, spacing: 24) {
                ForEach(results) { movie in
                    NavigationLink(value: movie) {
                        MobileGridMovieCard(movie: movie)
                    }
                    .buttonStyle(MobileCardButtonStyle())
                }
            }
        } else if isSearching {
            HStack(spacing: 12) {
                ProgressView()
                    .tint(Color.mobileTeaAccent)
                Text("Steeping the perfect results…")
                    .foregroundStyle(Color.mobileTeaMuted)
            }
            .frame(maxWidth: .infinity, minHeight: 330)
        } else if let errorMessage {
            ContentUnavailableView(
                "Search Failed",
                systemImage: "wifi.exclamationmark",
                description: Text(errorMessage)
            )
            .foregroundStyle(Color.mobileTeaCream)
            .frame(maxWidth: .infinity, minHeight: 330)
        } else if trimmedQuery.count < 2 {
            ContentUnavailableView(
                "Search by Title",
                systemImage: "magnifyingglass",
                description: Text("Type at least two letters of a movie or show title.")
            )
            .foregroundStyle(Color.mobileTeaCream)
            .frame(maxWidth: .infinity, minHeight: 330)
        } else {
            ContentUnavailableView.search(text: trimmedQuery)
                .foregroundStyle(Color.mobileTeaCream)
                .frame(maxWidth: .infinity, minHeight: 330)
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

private struct MobileGridMovieCard: View {
    let movie: Movie

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            MobilePosterImage(movie: movie)
                .aspectRatio(2 / 3, contentMode: .fit)
                .overlay {
                    RoundedRectangle(cornerRadius: 14, style: .continuous)
                        .stroke(Color.mobileTeaCream.opacity(0.12), lineWidth: 1)
                }
                .overlay(alignment: .topTrailing) {
                    Image(systemName: "chevron.right")
                        .font(.caption2.weight(.black))
                        .foregroundStyle(Color.mobileTeaCream)
                        .frame(width: 27, height: 27)
                        .background(Color.mobileTeaBackground.opacity(0.8), in: Circle())
                        .padding(9)
                        .accessibilityHidden(true)
                }
                .shadow(color: .black.opacity(0.3), radius: 8, y: 5)

            Text(movie.title)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(Color.mobileTeaCream)
                .lineLimit(2, reservesSpace: true)

            Text(movie.catalogMetadata)
                .font(.caption)
                .foregroundStyle(Color.mobileTeaMuted)
                .lineLimit(2, reservesSpace: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
