import FilmstreamCore
import SwiftUI

struct HomeView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        NavigationStack {
            ZStack {
                Color.filmstreamBackground.ignoresSafeArea()
                ScrollView {
                    VStack(alignment: .leading, spacing: 52) {
                        header
                        continueWatchingSection
                        if let errorMessage = model.errorMessage {
                            Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(.orange)
                                .font(.headline)
                        }
                        Spacer(minLength: 60)
                    }
                    .padding(.horizontal, 76)
                    .padding(.vertical, 44)
                }
            }
            .navigationDestination(for: Movie.self) { movie in
                MovieDetailView(movie: movie)
            }
            .task {
                await model.loadContinueWatching()
            }
        }
    }

    private var header: some View {
        HStack {
            BrandHeader()
            Spacer()
            NavigationLink {
                SearchView()
            } label: {
                Label("Search", systemImage: "magnifyingglass")
                    .font(.title3.weight(.semibold))
                    .padding(.horizontal, 18)
            }
        }
        .focusSection()
    }

    @ViewBuilder
    private var continueWatchingSection: some View {
        VStack(alignment: .leading, spacing: 24) {
            Text("Continue Watching")
                .font(.system(size: 40, weight: .bold, design: .rounded))

            if model.isLoading && model.continueWatching.isEmpty {
                HStack(spacing: 18) {
                    ProgressView()
                    Text("Loading your movies…")
                        .foregroundStyle(.secondary)
                }
                .frame(height: 360)
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 34) {
                        ForEach(model.continueWatching) { entry in
                            MovieNavigationCard(movie: entry.movie, progress: entry.progress)
                        }
                    }
                    .padding(.horizontal, 10)
                    .padding(.vertical, 24)
                }
                .contentMargins(.horizontal, -10, for: .scrollContent)
                .scrollClipDisabled()
            }
        }
        .focusSection()
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 28) {
            Image(systemName: "play.circle")
                .font(.system(size: 76))
                .foregroundStyle(Color.filmstreamAccent)
            VStack(alignment: .leading, spacing: 8) {
                Text("Your next movie starts here")
                    .font(.title2.weight(.semibold))
                Text("Search for a movie and Filmstream will remember where you left off.")
                    .font(.title3)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            NavigationLink {
                SearchView()
            } label: {
                Label("Find a Movie", systemImage: "magnifyingglass")
            }
        }
        .padding(36)
        .frame(maxWidth: .infinity, minHeight: 210)
        .background(Color.filmstreamPanel, in: RoundedRectangle(cornerRadius: 28, style: .continuous))
    }
}
