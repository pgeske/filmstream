import FilmstreamCore
import SwiftUI

struct IOSRootView: View {
    @Environment(IOSAppModel.self) private var model
    @State private var selectedTab = IOSRootTab.home

    var body: some View {
        TabView(selection: $selectedTab) {
            NavigationStack {
                IOSHomeView {
                    selectedTab = .search
                }
                .navigationDestination(for: Movie.self) { movie in
                        if movie.isShow {
                            IOSShowDetailView(show: movie)
                        } else {
                            IOSMovieDetailView(movie: movie)
                        }
                    }
            }
            .tabItem {
                Label("Home", systemImage: "house.fill")
            }
            .tag(IOSRootTab.home)

            NavigationStack {
                IOSSearchView()
                    .navigationDestination(for: Movie.self) { movie in
                        if movie.isShow {
                            IOSShowDetailView(show: movie)
                        } else {
                            IOSMovieDetailView(movie: movie)
                        }
                    }
            }
            .tabItem {
                Label("Search", systemImage: "magnifyingglass")
            }
            .tag(IOSRootTab.search)
        }
        .tint(Color.mobileTeaAccent)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.96), for: .tabBar)
        .toolbarBackground(.visible, for: .tabBar)
        .toolbarColorScheme(.dark, for: .tabBar)
        .task {
            await model.loadHome()
        }
    }
}

private enum IOSRootTab: Hashable {
    case home
    case search
}

struct IOSHomeView: View {
    @Environment(IOSAppModel.self) private var model
    @State private var isShowingRecommendationPreferences = false
    let onOpenSearch: () -> Void

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 36) {
                    header
                    continueWatchingSection
                    recommendationSections
                    ForEach(model.discoverySections) { section in
                        discoverySection(section)
                    }
                    if let errorMessage = model.errorMessage {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Color.mobileTeaAmber)
                            .padding(14)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(
                                Color.mobileTeaPanel.opacity(0.72),
                                in: RoundedRectangle(cornerRadius: 16, style: .continuous)
                            )
                            .overlay {
                                RoundedRectangle(cornerRadius: 16, style: .continuous)
                                    .stroke(Color.mobileTeaAmber.opacity(0.24), lineWidth: 1)
                            }
                            .padding(.horizontal, 18)
                    }
                    Text("Media data and images provided by TMDB.")
                        .font(.caption2)
                        .foregroundStyle(Color.mobileTeaMuted.opacity(0.75))
                        .padding(.horizontal, 18)
                }
                .padding(.top, 10)
                .padding(.bottom, 110)
            }
            .refreshable {
                await model.loadHome()
            }
        }
        .toolbar(.hidden, for: .navigationBar)
        .sheet(isPresented: $isShowingRecommendationPreferences) {
            IOSRecommendationPreferencesView(prompt: model.recommendations?.prompt ?? "")
                .environment(model)
                .presentationDetents([.medium, .large])
                .presentationDragIndicator(.visible)
        }
    }

    private var header: some View {
        HStack {
            MobileBrandHeader()
            Spacer()
            if model.isLoading {
                ProgressView()
                    .tint(Color.mobileTeaAccent)
            }
        }
        .padding(.horizontal, 18)
    }

    @ViewBuilder
    private var continueWatchingSection: some View {
        VStack(alignment: .leading, spacing: 12) {
            MobileSectionHeader(title: "Continue Watching")
                .padding(.horizontal, 18)

            if model.isLoading && model.continueWatching.isEmpty {
                loadingShelf
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
                    .padding(.horizontal, 18)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 14) {
                        ForEach(model.continueWatching) { entry in
                            let movie = entry.movie
                            NavigationLink(value: movie) {
                                MobileMovieCard(
                                    movie: movie,
                                    progress: entry.progress,
                                    contentRating: model.ratings(for: movie)?.contentRating
                                )
                            }
                            .buttonStyle(MobileCardButtonStyle())
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 18)
                    .padding(.vertical, 5)
                    .scrollTargetLayout()
                }
                .scrollTargetBehavior(.viewAligned(limitBehavior: .alwaysByOne))
            }
        }
    }

    @ViewBuilder
    private var recommendationSections: some View {
        VStack(alignment: .leading, spacing: 30) {
            recommendationHeader
                .padding(.horizontal, 18)

            if (model.isLoading && model.recommendations == nil)
                || (model.recommendations?.refreshing == true
                    && model.recommendations?.items.isEmpty == true) {
                recommendationLoadingState
                    .padding(.horizontal, 18)
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
                    .padding(.horizontal, 18)
            }
        }
    }

    @ViewBuilder
    private func recommendationShelf(
        title: String,
        emptyMessage: String,
        items: [Movie]
    ) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            MobileSectionHeader(title: title)
                .padding(.horizontal, 18)

            if items.isEmpty {
                Text(emptyMessage)
                    .font(.subheadline)
                    .foregroundStyle(Color.mobileTeaMuted)
                    .frame(maxWidth: .infinity, minHeight: 90, alignment: .leading)
                    .padding(.horizontal, 18)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 14) {
                        ForEach(items) { movie in
                            NavigationLink(value: movie) {
                                MobileMovieCard(
                                    movie: movie,
                                    contentRating: model.ratings(for: movie)?.contentRating
                                )
                            }
                            .buttonStyle(MobileCardButtonStyle())
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 18)
                    .padding(.vertical, 5)
                    .scrollTargetLayout()
                }
                .scrollTargetBehavior(.viewAligned(limitBehavior: .alwaysByOne))
            }
        }
    }

    private var recommendationHeader: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(spacing: 10) {
                MobileSectionHeader(title: "For You")
                Spacer()
                Button {
                    isShowingRecommendationPreferences = true
                } label: {
                    Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                        .labelStyle(.titleAndIcon)
                        .font(.caption.weight(.bold))
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
                .buttonStyle(MobileCardButtonStyle())
            }

            if model.recommendations?.refreshing == true,
               model.recommendations?.items.isEmpty == false {
                HStack(spacing: 7) {
                    ProgressView()
                        .controlSize(.small)
                        .tint(Color.mobileTeaAccent)
                    Text("Updating recommendations…")
                        .font(.caption2)
                        .foregroundStyle(Color.mobileTeaMuted)
                }
            }
        }
    }

    private var recommendationLoadingState: some View {
        HStack(spacing: 14) {
            ProgressView()
                .tint(Color.mobileTeaAccent)
            VStack(alignment: .leading, spacing: 3) {
                Text("Preparing recommendations…")
                    .font(.headline)
                    .foregroundStyle(Color.mobileTeaCream)
                Text("Matching movies and shows to your taste.")
                    .font(.caption)
                    .foregroundStyle(Color.mobileTeaMuted)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 150, alignment: .leading)
    }

    private var emptyRecommendations: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(spacing: 15) {
                Image(systemName: "wand.and.stars")
                    .font(.system(size: 30, weight: .semibold))
                    .foregroundStyle(Color.mobileTeaAccentLight)
                    .frame(width: 50)
                VStack(alignment: .leading, spacing: 4) {
                    Text(hasRecommendationPrompt ? "Recommendations are still brewing" : "Tell TeaStream what you like")
                        .font(.headline)
                        .foregroundStyle(Color.mobileTeaCream)
                    Text(hasRecommendationPrompt
                        ? "TeaStream will check again the next time Home refreshes."
                        : "Share favorite genres, shows, pacing, and things to avoid to build recommendations.")
                        .font(.caption)
                        .foregroundStyle(Color.mobileTeaMuted)
                }
            }

            Button {
                isShowingRecommendationPreferences = true
            } label: {
                Label("Tune Recommendations", systemImage: "slider.horizontal.3")
                    .font(.subheadline.weight(.bold))
                    .foregroundStyle(Color.mobileTeaBackground)
                    .padding(.horizontal, 15)
                    .padding(.vertical, 10)
                    .background(Color.mobileTeaAccent, in: Capsule())
            }
            .buttonStyle(MobileCardButtonStyle())
        }
        .padding(18)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            LinearGradient(
                colors: [Color.mobileTeaPanelElevated, Color.mobileTeaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 20, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .stroke(Color.mobileTeaAccent.opacity(0.18), lineWidth: 1)
        }
    }

    private var hasRecommendationPrompt: Bool {
        !(model.recommendations?.prompt.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ?? true)
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            VStack(alignment: .leading, spacing: 12) {
                MobileSectionHeader(title: section.title)
                    .padding(.horizontal, 18)

                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 14) {
                        ForEach(section.items) { movie in
                            NavigationLink(value: movie) {
                                MobileMovieCard(
                                    movie: movie,
                                    contentRating: model.ratings(for: movie)?.contentRating
                                )
                            }
                            .buttonStyle(MobileCardButtonStyle())
                            .task(id: movie.id) {
                                await model.loadRatings(for: movie)
                            }
                        }
                    }
                    .padding(.horizontal, 18)
                    .padding(.vertical, 5)
                    .scrollTargetLayout()
                }
                .scrollTargetBehavior(.viewAligned(limitBehavior: .alwaysByOne))
            }
        }
    }

    private var loadingShelf: some View {
        HStack(spacing: 12) {
            ProgressView()
                .tint(Color.mobileTeaAccent)
            Text("Steeping your next shelf…")
                .font(.subheadline)
                .foregroundStyle(Color.mobileTeaMuted)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
    }

    private var emptyContinueWatching: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(spacing: 15) {
                MobileTeaStreamMark(size: 50)
                VStack(alignment: .leading, spacing: 4) {
                    Text("Your next story starts here")
                        .font(.headline)
                        .foregroundStyle(Color.mobileTeaCream)
                    Text("Find a movie or show and TeaStream will remember your place.")
                        .font(.caption)
                        .foregroundStyle(Color.mobileTeaMuted)
                }
            }

            Button(action: onOpenSearch) {
                Label("Find Something", systemImage: "magnifyingglass")
                    .font(.subheadline.weight(.bold))
                    .foregroundStyle(Color.mobileTeaBackground)
                    .padding(.horizontal, 15)
                    .padding(.vertical, 10)
                    .background(Color.mobileTeaAccent, in: Capsule())
            }
            .buttonStyle(MobileCardButtonStyle())
        }
        .padding(18)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            LinearGradient(
                colors: [Color.mobileTeaPanelElevated, Color.mobileTeaPanel],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            ),
            in: RoundedRectangle(cornerRadius: 20, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .stroke(Color.mobileTeaAccent.opacity(0.18), lineWidth: 1)
        }
    }
}
