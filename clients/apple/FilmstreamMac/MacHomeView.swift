import FilmstreamCore
import SwiftUI

struct MacHomeView: View {
    @Environment(MacAppModel.self) private var model
    @State private var selection: Destination? = .home

    var body: some View {
        ZStack {
            NavigationSplitView {
                VStack(spacing: 0) {
                    MacBrandHeader(compact: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 16)
                        .padding(.vertical, 18)

                    List(selection: $selection) {
                        Label("Home", systemImage: "house.fill")
                            .tag(Destination.home)
                        Label("Search", systemImage: "magnifyingglass")
                            .tag(Destination.search)
                    }
                    .listStyle(.sidebar)
                }
                .background(Color.macTeaBackground.opacity(0.94))
                .navigationSplitViewColumnWidth(min: 190, ideal: 220, max: 260)
            } detail: {
                switch selection ?? .home {
                case .home:
                    NavigationStack {
                        MacLibraryView {
                            selection = .search
                        }
                        .navigationDestination(for: Movie.self) { movie in
                            if movie.isShow {
                                MacShowDetailView(show: movie)
                            } else {
                                MacMovieDetailView(movie: movie)
                            }
                        }
                    }
                case .search:
                    NavigationStack {
                        MacSearchView()
                            .navigationDestination(for: Movie.self) { movie in
                                if movie.isShow {
                                    MacShowDetailView(show: movie)
                                } else {
                                    MacMovieDetailView(movie: movie)
                                }
                            }
                    }
                }
            }
            .navigationSplitViewStyle(.balanced)

            if let session = model.activePlayback {
                MacPlayerView(
                    movie: session.movie,
                    prepared: session.prepared,
                    api: model.api,
                    nextEpisode: session.nextEpisode,
                    onPlayNext: { episode in
                        try await model.advancePlayback(to: episode)
                    },
                    onClose: model.dismissPlayback
                )
                .id(session.id)
                .transition(.opacity)
                .zIndex(1)
            }
        }
        .toolbarVisibility(model.activePlayback == nil ? .automatic : .hidden, for: .windowToolbar)
        .task {
            await model.loadHome()
        }
    }
}

private extension MacHomeView {
    enum Destination: Hashable {
        case home
        case search
    }
}

private struct MacLibraryView: View {
    @Environment(MacAppModel.self) private var model
    @State private var isShowingRecommendationPreferences = false
    let showSearch: () -> Void

    var body: some View {
        ZStack {
            MacTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 38) {
                    header
                    continueWatchingSection
                    recommendationSections
                    ForEach(model.discoverySections) { section in
                        discoverySection(section)
                    }
                    if let errorMessage = model.errorMessage {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(Color.macTeaAmber)
                            .font(.headline)
                    }
                }
                .padding(.horizontal, 34)
                .padding(.top, 28)
                .padding(.bottom, 60)
            }
        }
        .navigationTitle("Home")
        .toolbar {
            ToolbarItemGroup {
                Button {
                    showSearch()
                } label: {
                    Label("Search", systemImage: "magnifyingglass")
                }
                Button {
                    Task { await model.loadHome() }
                } label: {
                    Label("Refresh", systemImage: "arrow.clockwise")
                }
                .disabled(model.isLoading)
            }
        }
        .refreshable {
            await model.loadHome()
        }
        .sheet(isPresented: $isShowingRecommendationPreferences) {
            MacRecommendationPreferencesView(prompt: model.recommendations?.prompt ?? "")
                .environment(model)
        }
    }

    private var header: some View {
        HStack(alignment: .center, spacing: 18) {
            MacBrandHeader()
            Spacer()
            if model.isLoading {
                ProgressView()
                    .controlSize(.small)
                    .tint(Color.macTeaAccent)
            }
        }
    }

    @ViewBuilder
    private var continueWatchingSection: some View {
        VStack(alignment: .leading, spacing: 14) {
            sectionHeader("Continue Watching")

            if model.isLoading && model.continueWatching.isEmpty {
                HStack(spacing: 12) {
                    ProgressView()
                        .tint(Color.macTeaAccent)
                    Text("Steeping your next shelf…")
                        .foregroundStyle(Color.macTeaMuted)
                }
                .frame(height: 180)
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 18) {
                        ForEach(model.continueWatching) { entry in
                            let movie = entry.movie
                            MacMovieCard(
                                movie: movie,
                                progress: entry.progress,
                                contentRating: model.ratings(for: movie)?.contentRating
                            )
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 3)
                    .padding(.vertical, 10)
                }
            }
        }
    }

    @ViewBuilder
    private var recommendationSections: some View {
        VStack(alignment: .leading, spacing: 32) {
            recommendationHeader

            if (model.isLoading && model.recommendations == nil)
                || (model.recommendations?.refreshing == true
                    && model.recommendations?.items.isEmpty == true) {
                recommendationLoadingState
            } else if let recommendations = model.recommendations,
                      !recommendations.items.isEmpty {
                recommendationShelf(
                    title: "Recommended Shows",
                    emptyMessage: "No show picks yet.",
                    items: recommendations.recommendedShows
                )
                recommendationShelf(
                    title: "Recommended Movies",
                    emptyMessage: "No movie picks yet.",
                    items: recommendations.recommendedMovies
                )
            } else {
                emptyRecommendations
            }
        }
    }

    @ViewBuilder
    private func recommendationShelf(
        title: String,
        emptyMessage: String,
        items: [Movie]
    ) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            sectionHeader(title)

            if items.isEmpty {
                Text(emptyMessage)
                    .foregroundStyle(Color.macTeaMuted)
                    .frame(maxWidth: .infinity, minHeight: 90, alignment: .leading)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 18) {
                        ForEach(items) { movie in
                            MacMovieCard(
                                movie: movie,
                                contentRating: model.ratings(for: movie)?.contentRating
                            )
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 3)
                    .padding(.vertical, 10)
                }
            }
        }
    }

    private var recommendationHeader: some View {
        HStack(spacing: 12) {
            sectionHeader("For You")
            if model.recommendations?.refreshing == true,
               model.recommendations?.items.isEmpty == false {
                ProgressView()
                    .controlSize(.small)
                    .tint(Color.macTeaAccent)
                Text("Updating recommendations…")
                    .font(.caption)
                    .foregroundStyle(Color.macTeaMuted)
            }
            Spacer()
            Button {
                isShowingRecommendationPreferences = true
            } label: {
                Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                    .font(.subheadline.weight(.semibold))
            }
            .buttonStyle(.plain)
            .foregroundStyle(Color.macTeaAccentLight)
        }
    }

    private var recommendationLoadingState: some View {
        HStack(spacing: 14) {
            ProgressView()
                .tint(Color.macTeaAccent)
            VStack(alignment: .leading, spacing: 4) {
                Text("Preparing recommendations…")
                    .font(.headline)
                    .foregroundStyle(Color.macTeaCream)
                Text("TeaStream is matching movies and shows to your taste.")
                    .font(.subheadline)
                    .foregroundStyle(Color.macTeaMuted)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 150, alignment: .leading)
    }

    private var emptyRecommendations: some View {
        HStack(spacing: 20) {
            Image(systemName: "wand.and.stars")
                .font(.system(size: 34, weight: .semibold))
                .foregroundStyle(Color.macTeaAccentLight)
                .frame(width: 58)
            VStack(alignment: .leading, spacing: 5) {
                Text(hasRecommendationPrompt ? "Recommendations are still brewing" : "Tell TeaStream what you like")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(Color.macTeaCream)
                Text(hasRecommendationPrompt
                    ? "TeaStream will check again the next time Home refreshes."
                    : "Share favorite genres, shows, pacing, and things to avoid to build recommendations.")
                    .foregroundStyle(Color.macTeaMuted)
            }
            Spacer()
            Button {
                isShowingRecommendationPreferences = true
            } label: {
                Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                    .font(.headline.weight(.semibold))
            }
            .buttonStyle(MacTeaActionButtonStyle(prominent: true))
        }
        .padding(24)
        .frame(maxWidth: .infinity, minHeight: 130)
        .background(
            LinearGradient(
                colors: [Color.macTeaPanelElevated, Color.macTeaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 20, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .stroke(Color.macTeaAccent.opacity(0.18), lineWidth: 1)
        }
    }

    private var hasRecommendationPrompt: Bool {
        !(model.recommendations?.prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ?? true)
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            VStack(alignment: .leading, spacing: 14) {
                sectionHeader(section.title)

                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 18) {
                        ForEach(section.items) { movie in
                            MacMovieCard(
                                movie: movie,
                                contentRating: model.ratings(for: movie)?.contentRating
                            )
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 3)
                    .padding(.vertical, 10)
                }
            }
        }
    }

    private func sectionHeader(_ title: String) -> some View {
        Text(title)
            .font(.system(size: 27, weight: .bold, design: .rounded))
            .foregroundStyle(Color.macTeaCream)
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 20) {
            MacTeaStreamMark(size: 58)
            VStack(alignment: .leading, spacing: 5) {
                Text("Your next story starts here")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(Color.macTeaCream)
                Text("Find a movie or show and TeaStream will remember where you left off.")
                    .foregroundStyle(Color.macTeaMuted)
            }
            Spacer()
            Button(action: showSearch) {
                Label("Find Something", systemImage: "magnifyingglass")
                    .font(.headline.weight(.semibold))
            }
            .buttonStyle(MacTeaActionButtonStyle())
        }
        .padding(24)
        .frame(maxWidth: .infinity, minHeight: 130)
        .background(
            LinearGradient(
                colors: [Color.macTeaPanelElevated, Color.macTeaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 20, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .stroke(Color.macTeaAccent.opacity(0.18), lineWidth: 1)
        }
    }
}
