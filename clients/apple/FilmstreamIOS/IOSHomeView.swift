import FilmstreamCore
import SwiftUI

struct IOSRootView: View {
    @Environment(IOSAppModel.self) private var model

    var body: some View {
        TabView {
            NavigationStack {
                IOSHomeView()
                    .navigationDestination(for: Movie.self) { movie in
                        IOSMovieDetailView(movie: movie)
                    }
            }
            .tabItem {
                Label("Home", systemImage: "house.fill")
            }

            NavigationStack {
                IOSSearchView()
                    .navigationDestination(for: Movie.self) { movie in
                        IOSMovieDetailView(movie: movie)
                    }
            }
            .tabItem {
                Label("Search", systemImage: "magnifyingglass")
            }
        }
        .tint(Color.mobileTeaAccent)
        .task {
            await model.loadHome()
        }
    }
}

struct IOSHomeView: View {
    @Environment(IOSAppModel.self) private var model
    @State private var removingEntryID: String?

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 34) {
                    header
                    continueWatchingSection
                    ForEach(model.discoverySections) { section in
                        discoverySection(section)
                    }
                    if let errorMessage = model.errorMessage {
                        Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                            .font(.footnote.weight(.semibold))
                            .foregroundStyle(Color.mobileTeaAmber)
                            .padding(.horizontal, 18)
                    }
                    Text("Movie data and images provided by TMDB.")
                        .font(.caption2)
                        .foregroundStyle(Color.mobileTeaMuted.opacity(0.75))
                        .padding(.horizontal, 18)
                }
                .padding(.top, 18)
                .padding(.bottom, 30)
            }
            .refreshable {
                await model.loadHome()
            }
        }
        .navigationTitle("Home")
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.94), for: .navigationBar)
        .toolbarBackground(.visible, for: .navigationBar)
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
        VStack(alignment: .leading, spacing: 14) {
            MobileSectionHeader(title: "Continue Watching")
            .padding(.horizontal, 18)

            if model.isLoading && model.continueWatching.isEmpty {
                loadingShelf
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
                    .padding(.horizontal, 18)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 15) {
                        ForEach(model.continueWatching) { entry in
                            continueWatchingCard(entry)
                        }
                    }
                    .padding(.horizontal, 18)
                    .padding(.vertical, 4)
                }
            }
        }
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            VStack(alignment: .leading, spacing: 14) {
                MobileSectionHeader(title: section.title)
                    .padding(.horizontal, 18)

                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 15) {
                        ForEach(section.items) { movie in
                            NavigationLink(value: movie) {
                                MobileMovieCard(movie: movie)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    .padding(.horizontal, 18)
                    .padding(.vertical, 4)
                }
            }
        }
    }

    private func continueWatchingCard(_ entry: WatchHistoryEntry) -> some View {
        ZStack(alignment: .topTrailing) {
            NavigationLink(value: entry.movie) {
                MobileMovieCard(movie: entry.movie, progress: entry.progress)
            }
            .buttonStyle(.plain)

            Menu {
                Button(role: .destructive) {
                    Task { await remove(entry) }
                } label: {
                    Label("Remove from Continue Watching", systemImage: "xmark")
                }
            } label: {
                ZStack {
                    Circle()
                        .fill(Color.mobileTeaBackground.opacity(0.86))
                    if removingEntryID == entry.id {
                        ProgressView()
                            .controlSize(.small)
                            .tint(Color.mobileTeaCream)
                    } else {
                        Image(systemName: "ellipsis")
                            .font(.footnote.weight(.bold))
                            .foregroundStyle(Color.mobileTeaCream)
                    }
                }
                .frame(width: 32, height: 32)
                .overlay {
                    Circle()
                        .stroke(Color.mobileTeaCream.opacity(0.16), lineWidth: 1)
                }
            }
            .disabled(removingEntryID != nil)
            .padding(8)
            .accessibilityLabel("Options for \(entry.title)")
        }
    }

    private var loadingShelf: some View {
        HStack(spacing: 12) {
            ProgressView()
                .tint(Color.mobileTeaAccent)
            Text("Steeping your movie shelf…")
                .font(.subheadline)
                .foregroundStyle(Color.mobileTeaMuted)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 15) {
            MobileTeaStreamMark(size: 50)
            VStack(alignment: .leading, spacing: 4) {
                Text("Your next movie starts here")
                    .font(.headline)
                    .foregroundStyle(Color.mobileTeaCream)
                Text("Find a favorite and TeaStream will remember your place.")
                    .font(.caption)
                    .foregroundStyle(Color.mobileTeaMuted)
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

    private func remove(_ entry: WatchHistoryEntry) async {
        removingEntryID = entry.id
        defer { removingEntryID = nil }
        do {
            try await model.removeFromContinueWatching(entry)
            model.errorMessage = nil
        } catch {
            model.errorMessage = error.localizedDescription
        }
    }
}
