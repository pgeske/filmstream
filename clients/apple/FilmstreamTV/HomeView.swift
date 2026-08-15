import FilmstreamCore
import SwiftUI

struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var pendingRemoval: WatchHistoryEntry?
    @State private var isRemovingFromContinueWatching = false

    var body: some View {
        ZStack {
            NavigationStack {
                ZStack {
                    TeaBackground()

                    ScrollView(.vertical, showsIndicators: false) {
                        VStack(alignment: .leading, spacing: 34) {
                            header
                            continueWatchingSection
                            ForEach(model.discoverySections) { section in
                                discoverySection(section)
                            }
                            if let errorMessage = model.errorMessage {
                                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                                    .foregroundStyle(Color.teaAmber)
                                    .font(.headline)
                            }
                        }
                        .padding(.horizontal, 88)
                        .padding(.top, 64)
                        .padding(.bottom, 110)
                    }
                }
                .navigationDestination(for: Movie.self) { movie in
                    MovieDetailView(movie: movie)
                }
                .task {
                    await model.loadHome()
                }
            }
            .disabled(pendingRemoval != nil)

            if let pendingRemoval {
                ContinueWatchingOptionsDialog(
                    isRemoving: isRemovingFromContinueWatching,
                    onCancel: {
                        self.pendingRemoval = nil
                    },
                    onRemove: {
                        Task { await removeFromContinueWatching(pendingRemoval) }
                    }
                )
            }
        }
        .animation(.snappy(duration: 0.22), value: pendingRemoval != nil)
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
        }
        .padding(.horizontal, 16)
        .focusSection()
    }

    @ViewBuilder
    private var continueWatchingSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Continue Watching")
                .font(.system(size: 42, weight: .bold, design: .rounded))
                .foregroundStyle(Color.teaCream)

            if model.isLoading && model.continueWatching.isEmpty {
                HStack(spacing: 18) {
                    ProgressView()
                        .tint(Color.teaAccent)
                    Text("Steeping your movie shelf…")
                        .foregroundStyle(Color.teaMuted)
                }
                .frame(height: 360)
            } else if model.continueWatching.isEmpty {
                emptyContinueWatching
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 48) {
                        ForEach(model.continueWatching) { entry in
                            MovieNavigationCard(
                                movie: entry.movie,
                                progress: entry.progress,
                                showsOptionsIndicator: true,
                                requestsInitialFocus: entry.id == model.continueWatching.first?.id
                            )
                            .highPriorityGesture(
                                LongPressGesture(minimumDuration: 0.65)
                                    .onEnded { _ in
                                        pendingRemoval = entry
                                    }
                            )
                            .accessibilityHint("Press and hold the center button for options")
                        }
                    }
                    .padding(.top, 14)
                    .padding(.bottom, 18)
                }
            }
        }
        .padding(.horizontal, 16)
        .focusSection()
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                Text(section.title)
                    .font(.system(size: 42, weight: .bold, design: .rounded))
                    .foregroundStyle(Color.teaCream)

                ScrollView(.horizontal, showsIndicators: false) {
                    LazyHStack(alignment: .top, spacing: 48) {
                        ForEach(section.items) { movie in
                            MovieNavigationCard(movie: movie)
                        }
                    }
                    .padding(.top, 14)
                    .padding(.bottom, 18)
                }
            }
            .padding(.horizontal, 16)
            .focusSection()
        }
    }

    private func removeFromContinueWatching(_ entry: WatchHistoryEntry) async {
        isRemovingFromContinueWatching = true
        defer { isRemovingFromContinueWatching = false }
        do {
            try await model.removeFromContinueWatching(entry)
            pendingRemoval = nil
            model.errorMessage = nil
        } catch {
            pendingRemoval = nil
            model.errorMessage = error.localizedDescription
        }
    }

    private var emptyContinueWatching: some View {
        HStack(spacing: 30) {
            TeaStreamMark(size: 76)
            VStack(alignment: .leading, spacing: 8) {
                Text("Your next movie starts here")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(Color.teaCream)
                Text("Find a favorite and TeaStream will remember where you left off.")
                    .font(.title3)
                    .foregroundStyle(Color.teaMuted)
            }
            Spacer()
            NavigationLink {
                SearchView()
            } label: {
                Label("Find a Movie", systemImage: "magnifyingglass")
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
