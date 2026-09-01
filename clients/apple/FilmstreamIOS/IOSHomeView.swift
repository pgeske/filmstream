import FilmstreamCore
import SwiftUI

struct IOSRootView: View {
    @Environment(IOSAppModel.self) private var model
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    @State private var selectedTab = IOSRootTab.home
    @State private var sidebarSelection: IOSRootTab? = .home

    var body: some View {
        Group {
            if horizontalSizeClass == .regular {
                NavigationSplitView {
                    ZStack {
                        MobileTeaBackground()
                        List(selection: $sidebarSelection) {
                            Section {
                                Label("Home", systemImage: "house.fill")
                                    .tag(IOSRootTab.home)
                                Label("Search", systemImage: "magnifyingglass")
                                    .tag(IOSRootTab.search)
                            } header: {
                                MobileBrandHeader(compact: true)
                                    .padding(.bottom, 16)
                                    .textCase(nil)
                            }
                        }
                        .scrollContentBackground(.hidden)
                        .listStyle(.sidebar)
                        .onChange(of: sidebarSelection) { _, selection in
                            if let selection {
                                selectedTab = selection
                            }
                        }
                    }
                    .navigationSplitViewColumnWidth(min: 220, ideal: 250, max: 300)
                } detail: {
                    navigationStack(for: selectedTab)
                }
                .navigationSplitViewStyle(.balanced)
            } else {
                TabView(selection: $selectedTab) {
                    navigationStack(for: .home)
                        .tabItem {
                            Label("Home", systemImage: "house.fill")
                        }
                        .tag(IOSRootTab.home)

                    navigationStack(for: .search)
                        .tabItem {
                            Label("Search", systemImage: "magnifyingglass")
                        }
                        .tag(IOSRootTab.search)
                }
                .toolbarBackground(Color.mobileTeaBackground.opacity(0.96), for: .tabBar)
                .toolbarBackground(.visible, for: .tabBar)
                .toolbarColorScheme(.dark, for: .tabBar)
            }
        }
        .tint(Color.mobileTeaAccent)
        .onChange(of: selectedTab) { _, selection in
            sidebarSelection = selection
        }
        .task {
            await model.loadHome()
        }
    }

    private func navigationStack(for tab: IOSRootTab) -> some View {
        NavigationStack {
            Group {
                switch tab {
                case .home:
                    IOSHomeView {
                        selectedTab = .search
                    }
                case .search:
                    IOSSearchView()
                }
            }
            .navigationDestination(for: Movie.self) { movie in
                if movie.isShow {
                    IOSShowDetailView(show: movie)
                } else {
                    IOSMovieDetailView(movie: movie)
                }
            }
        }
    }
}

private enum IOSRootTab: Hashable {
    case home
    case search
}

struct IOSHomeView: View {
    @Environment(IOSAppModel.self) private var model
    @Environment(\.horizontalSizeClass) private var horizontalSizeClass
    let onOpenSearch: () -> Void

    private var shelfCardWidth: CGFloat {
        horizontalSizeClass == .regular ? 340 : 300
    }

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(
                    alignment: .leading,
                    spacing: horizontalSizeClass == .regular ? 44 : 36
                ) {
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
                .padding(.bottom, horizontalSizeClass == .regular ? 54 : 110)
            }
            .refreshable {
                await model.loadHome()
            }
        }
        .toolbar(.hidden, for: .navigationBar)
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
                                    contentRating: model.ratings(for: movie)?.contentRating,
                                    width: shelfCardWidth
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
                                    contentRating: model.ratings(for: movie)?.contentRating,
                                    width: shelfCardWidth
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
                    Text("Recommendations are still brewing")
                        .font(.headline)
                        .foregroundStyle(Color.mobileTeaCream)
                    Text("TeaStream will check again the next time Home refreshes.")
                        .font(.caption)
                        .foregroundStyle(Color.mobileTeaMuted)
                }
            }
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
                                    contentRating: model.ratings(for: movie)?.contentRating,
                                    width: shelfCardWidth
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
