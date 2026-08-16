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
    let showSearch: () -> Void

    var body: some View {
        ZStack {
            MacTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 38) {
                    header
                    continueWatchingSection
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
