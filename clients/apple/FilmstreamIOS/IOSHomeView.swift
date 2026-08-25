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
    let onOpenSearch: () -> Void

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 36) {
                    header
                    continueWatchingSection
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
