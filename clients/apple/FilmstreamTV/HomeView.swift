import FilmstreamCore
import SwiftUI

struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var activeShelfID: String?
    @State private var lastFocusedShelfItem: NetflixShelfFocus?
    @State private var searchReturnFocus: NetflixShelfFocus?
    @State private var isShowingRecommendationPreferences = false
    @FocusState private var focusedShelfItem: NetflixShelfFocus?
    @FocusState private var isSearchFocused: Bool

    private let homeTopID = "home-top"
    private let continueShelfID = "continue-watching"
    private let recommendationSectionID = "recommendations"
    private let recommendedShowsShelfID = "recommended-shows"
    private let recommendedMoviesShelfID = "recommended-movies"
    private let finalShelfAlignmentSpace: CGFloat = 360

    var body: some View {
        NavigationStack {
            ZStack {
                TeaBackground()

                ScrollViewReader { scrollProxy in
                    ScrollView(.vertical, showsIndicators: false) {
                        VStack(alignment: .leading, spacing: 30) {
                            header
                                .id(homeTopID)
                            continueWatchingSection(scrollProxy: scrollProxy)
                            recommendationSections(scrollProxy: scrollProxy)
                            ForEach(model.discoverySections) { section in
                                discoverySection(section, scrollProxy: scrollProxy)
                            }
                            if let errorMessage = model.errorMessage {
                                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                                    .foregroundStyle(Color.teaAmber)
                                    .font(.headline)
                            }
                        }
                        .padding(.horizontal, 72)
                        .padding(.top, 54)
                        .padding(.bottom, finalShelfAlignmentSpace)
                    }
                    .ignoresSafeArea(.container, edges: .horizontal)
                }
            }
            .navigationDestination(for: Movie.self) { movie in
                if movie.isShow {
                    ShowDetailView(show: movie)
                } else {
                    MovieDetailView(movie: movie)
                }
            }
            .task {
                await model.loadHome()
            }
            .sheet(isPresented: $isShowingRecommendationPreferences) {
                RecommendationPreferencesView(prompt: model.recommendations?.prompt ?? "")
                    .environment(model)
            }
            .onChange(of: focusedShelfItem) { _, newFocus in
                guard let newFocus else { return }
                lastFocusedShelfItem = newFocus
                guard newFocus == searchReturnFocus, !isSearchFocused else { return }
                Task { @MainActor in
                    await Task.yield()
                    guard focusedShelfItem == newFocus else { return }
                    searchReturnFocus = nil
                }
            }
            .onChange(of: isSearchFocused) { wasFocused, isFocused in
                if isFocused {
                    searchReturnFocus = lastFocusedShelfItem
                } else if wasFocused, focusedShelfItem != nil, let searchReturnFocus {
                    focusedShelfItem = searchReturnFocus
                }
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
            }
            .buttonStyle(TeaActionButtonStyle())
            .focusEffectDisabled()
            .focused($isSearchFocused)
        }
        .padding(.horizontal, 16)
        .focusSection()
    }

    @ViewBuilder
    private func continueWatchingSection(scrollProxy: ScrollViewProxy) -> some View {
        if model.isLoading && model.continueWatching.isEmpty {
            VStack(alignment: .leading, spacing: 16) {
                shelfTitle("Continue Watching")
                HStack(spacing: 18) {
                    ProgressView()
                        .tint(Color.teaAccent)
                    Text("Steeping your next shelf…")
                        .foregroundStyle(Color.teaMuted)
                }
                .frame(height: 360)
            }
            .padding(.horizontal, 16)
        } else if model.continueWatching.isEmpty {
            VStack(alignment: .leading, spacing: 16) {
                shelfTitle("Continue Watching")
                emptyContinueWatching
            }
            .padding(.horizontal, 16)
        } else {
            NetflixMovieShelf(
                shelfID: continueShelfID,
                title: "Continue Watching",
                items: model.continueWatching.map {
                    let movie = $0.movie
                    return NetflixShelfItem(
                        movie: movie,
                        progress: $0.progress,
                        contentRating: model.ratings(for: movie)?.contentRating
                    )
                },
                focusBinding: $focusedShelfItem,
                preferredEntryFocus: searchReturnFocus,
                requestsInitialFocus: true,
                onFocus: loadRatings,
                onHorizontalFocusChange: {
                    alignShelf(
                        continueShelfID,
                        scrollProxy: scrollProxy,
                        animated: false
                    )
                },
                onShelfFocusChange: {
                    updateShelfFocus(
                        continueShelfID,
                        isFocused: $0,
                        scrollProxy: scrollProxy
                    )
                }
            )
            .id(continueShelfID)
        }
    }

    @ViewBuilder
    private func recommendationSections(scrollProxy: ScrollViewProxy) -> some View {
        VStack(alignment: .leading, spacing: 30) {
            recommendationHeader
                .padding(.horizontal, 16)

            if (model.isLoading && model.recommendations == nil)
                || (model.recommendations?.refreshing == true
                    && model.recommendations?.items.isEmpty == true) {
                recommendationLoadingState
                    .padding(.horizontal, 16)
            } else if let recommendations = model.recommendations,
                      !recommendations.items.isEmpty {
                recommendationShelf(
                    shelfID: recommendedShowsShelfID,
                    title: "Recommended Shows",
                    emptyMessage: "No show picks yet.",
                    items: recommendations.recommendedShows,
                    scrollProxy: scrollProxy
                )
                recommendationShelf(
                    shelfID: recommendedMoviesShelfID,
                    title: "Recommended Movies",
                    emptyMessage: "No movie picks yet.",
                    items: recommendations.recommendedMovies,
                    scrollProxy: scrollProxy
                )
            } else {
                emptyRecommendations
                    .padding(.horizontal, 16)
            }
        }
        .id(recommendationSectionID)
    }

    @ViewBuilder
    private func recommendationShelf(
        shelfID: String,
        title: String,
        emptyMessage: String,
        items: [Movie],
        scrollProxy: ScrollViewProxy
    ) -> some View {
        if items.isEmpty {
            VStack(alignment: .leading, spacing: 16) {
                shelfTitle(title)
                Text(emptyMessage)
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
                    .frame(maxWidth: .infinity, minHeight: 120, alignment: .leading)
            }
            .padding(.horizontal, 16)
            .id(shelfID)
        } else {
            NetflixMovieShelf(
                shelfID: shelfID,
                title: title,
                items: items.map {
                    NetflixShelfItem(
                        movie: $0,
                        contentRating: model.ratings(for: $0)?.contentRating
                    )
                },
                focusBinding: $focusedShelfItem,
                preferredEntryFocus: searchReturnFocus,
                onFocus: loadRatings,
                onHorizontalFocusChange: {
                    alignShelf(shelfID, scrollProxy: scrollProxy, animated: false)
                },
                onShelfFocusChange: {
                    updateShelfFocus(shelfID, isFocused: $0, scrollProxy: scrollProxy)
                }
            )
            .id(shelfID)
        }
    }

    private var recommendationHeader: some View {
        HStack(spacing: 18) {
            shelfTitle("For You")
            if model.recommendations?.refreshing == true,
               model.recommendations?.items.isEmpty == false {
                ProgressView()
                    .tint(Color.teaAccent)
                Text("Updating recommendations…")
                    .font(.headline)
                    .foregroundStyle(Color.teaMuted)
            }
            Spacer()
            Button {
                isShowingRecommendationPreferences = true
            } label: {
                Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                    .font(.headline.weight(.semibold))
            }
            .buttonStyle(TeaActionButtonStyle())
            .focusEffectDisabled()
        }
        .focusSection()
    }

    private var recommendationLoadingState: some View {
        HStack(spacing: 18) {
            ProgressView()
                .tint(Color.teaAccent)
            VStack(alignment: .leading, spacing: 6) {
                Text("Preparing recommendations…")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(Color.teaCream)
                Text("TeaStream is matching movies and shows to your taste.")
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 180, alignment: .leading)
    }

    private var emptyRecommendations: some View {
        HStack(spacing: 30) {
            Image(systemName: "wand.and.stars")
                .font(.system(size: 54, weight: .semibold))
                .foregroundStyle(Color.teaAccentLight)
                .frame(width: 76)
            VStack(alignment: .leading, spacing: 8) {
                Text(hasRecommendationPrompt ? "Recommendations are still brewing" : "Tell TeaStream what you like")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(Color.teaCream)
                Text(hasRecommendationPrompt
                    ? "TeaStream will check again the next time Home refreshes."
                    : "Share favorite genres, shows, pacing, and things to avoid to build recommendations.")
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
            }
            Spacer()
            Button {
                isShowingRecommendationPreferences = true
            } label: {
                Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                    .font(.headline.weight(.semibold))
            }
            .buttonStyle(TeaActionButtonStyle(prominent: true))
            .focusEffectDisabled()
        }
        .padding(36)
        .frame(maxWidth: .infinity, minHeight: 210)
        .background(
            LinearGradient(
                colors: [Color.teaPanelElevated, Color.teaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 28, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 28, style: .continuous)
                .stroke(Color.teaAccent.opacity(0.18), lineWidth: 1)
        }
    }

    private var hasRecommendationPrompt: Bool {
        !(model.recommendations?.prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ?? true)
    }

    @ViewBuilder
    private func discoverySection(
        _ section: DiscoverySection,
        scrollProxy: ScrollViewProxy
    ) -> some View {
        if !section.items.isEmpty {
            NetflixMovieShelf(
                shelfID: section.id,
                title: section.title,
                items: section.items.map {
                    NetflixShelfItem(
                        movie: $0,
                        contentRating: model.ratings(for: $0)?.contentRating
                    )
                },
                focusBinding: $focusedShelfItem,
                preferredEntryFocus: searchReturnFocus,
                onFocus: loadRatings,
                onHorizontalFocusChange: {
                    alignShelf(
                        section.id,
                        scrollProxy: scrollProxy,
                        animated: false
                    )
                },
                onShelfFocusChange: {
                    updateShelfFocus(
                        section.id,
                        isFocused: $0,
                        scrollProxy: scrollProxy
                    )
                }
            )
            .id(section.id)
        }
    }

    private func updateShelfFocus(
        _ shelfID: String,
        isFocused: Bool,
        scrollProxy: ScrollViewProxy
    ) {
        guard isFocused else {
            if activeShelfID == shelfID {
                activeShelfID = nil
            }
            return
        }

        guard activeShelfID != shelfID else { return }
        activeShelfID = shelfID
        alignShelf(shelfID, scrollProxy: scrollProxy)
    }

    private func alignShelf(
        _ shelfID: String,
        scrollProxy: ScrollViewProxy,
        animated: Bool = true
    ) {
        let targetID = shelfID == continueShelfID ? homeTopID : shelfID
        let anchor = shelfID == continueShelfID
            ? UnitPoint.top
            : UnitPoint(x: 0.5, y: 0.07)

        if animated {
            withAnimation(.easeInOut(duration: 0.24)) {
                scrollProxy.scrollTo(targetID, anchor: anchor)
            }
        } else {
            var transaction = Transaction()
            transaction.disablesAnimations = true
            withTransaction(transaction) {
                scrollProxy.scrollTo(targetID, anchor: anchor)
            }
        }
    }

    private func loadRatings(for movie: Movie) {
        Task {
            await model.loadRatings(for: movie)
        }
    }

    private func shelfTitle(_ title: String) -> some View {
        Text(title)
            .font(.system(size: 38, weight: .bold, design: .rounded))
            .foregroundStyle(Color.teaCream)
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 30) {
            TeaStreamMark(size: 76)
            VStack(alignment: .leading, spacing: 8) {
                Text("Your next story starts here")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(Color.teaCream)
                Text("Find a movie or show and TeaStream will remember where you left off.")
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
            }
            Spacer()
            NavigationLink {
                SearchView()
            } label: {
                Label("Find Something", systemImage: "magnifyingglass")
                    .font(.headline.weight(.semibold))
            }
            .buttonStyle(TeaActionButtonStyle())
            .focusEffectDisabled()
        }
        .padding(36)
        .frame(maxWidth: .infinity, minHeight: 210)
        .background(
            LinearGradient(
                colors: [Color.teaPanelElevated, Color.teaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 28, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 28, style: .continuous)
                .stroke(Color.teaAccent.opacity(0.18), lineWidth: 1)
        }
    }
}
