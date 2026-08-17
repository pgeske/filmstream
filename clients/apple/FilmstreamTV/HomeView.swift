import FilmstreamCore
import SwiftUI

struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var activeShelfID: String?
    @State private var lastFocusedShelfItem: NetflixShelfFocus?
    @State private var searchReturnFocus: NetflixShelfFocus?
    @FocusState private var focusedShelfItem: NetflixShelfFocus?
    @FocusState private var isSearchFocused: Bool

    private let homeTopID = "home-top"
    private let continueShelfID = "continue-watching"
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
