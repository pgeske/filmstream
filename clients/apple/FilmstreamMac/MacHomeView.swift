import FilmstreamCore
import SwiftUI

struct MacHomeView: View {
    @Environment(MacAppModel.self) private var model
    @State private var selection: Destination? = .home

    var body: some View {
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
                        MacMovieDetailView(movie: movie)
                    }
                }
            case .search:
                NavigationStack {
                    MacSearchView()
                        .navigationDestination(for: Movie.self) { movie in
                            MacMovieDetailView(movie: movie)
                        }
                }
            }
        }
        .navigationSplitViewStyle(.balanced)
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
                LazyVStack(alignment: .leading, spacing: 42) {
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
        VStack(alignment: .leading, spacing: 18) {
            sectionHeader(
                title: "Continue Watching",
                subtitle: "Settle in and pick up where you left off"
            )

            if model.isLoading && model.continueWatching.isEmpty {
                HStack(spacing: 12) {
                    ProgressView()
                        .tint(Color.macTeaAccent)
                    Text("Steeping your movie shelf…")
                        .foregroundStyle(Color.macTeaMuted)
                }
                .frame(height: 180)
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 24) {
                        ForEach(model.continueWatching) { entry in
                            MacMovieCard(movie: entry.movie, progress: entry.progress)
                                .contextMenu {
                                    Button("Remove from Continue Watching", role: .destructive) {
                                        Task { await removeFromContinueWatching(entry) }
                                    }
                                }
                        }
                    }
                    .padding(.horizontal, 3)
                    .padding(.vertical, 12)
                }
            }
        }
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            VStack(alignment: .leading, spacing: 18) {
                sectionHeader(title: section.title, subtitle: section.subtitle)

                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 24) {
                        ForEach(section.items) { movie in
                            MacMovieCard(movie: movie)
                        }
                    }
                    .padding(.horizontal, 3)
                    .padding(.vertical, 12)
                }
            }
        }
    }

    private func sectionHeader(title: String, subtitle: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title)
                .font(.system(size: 27, weight: .bold, design: .rounded))
                .foregroundStyle(Color.macTeaCream)
            Text(subtitle)
                .font(.subheadline)
                .foregroundStyle(Color.macTeaMuted)
        }
    }

    private func removeFromContinueWatching(_ entry: WatchHistoryEntry) async {
        do {
            try await model.removeFromContinueWatching(entry)
            model.errorMessage = nil
        } catch {
            model.errorMessage = error.localizedDescription
        }
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 20) {
            MacTeaStreamMark(size: 58)
            VStack(alignment: .leading, spacing: 5) {
                Text("Your next movie starts here")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(Color.macTeaCream)
                Text("Find a favorite and TeaStream will remember where you left off.")
                    .foregroundStyle(Color.macTeaMuted)
            }
            Spacer()
            Button(action: showSearch) {
                Label("Find a Movie", systemImage: "magnifyingglass")
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
