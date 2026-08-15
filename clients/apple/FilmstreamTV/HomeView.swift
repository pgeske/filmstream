import FilmstreamCore
import SwiftUI

struct HomeView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        NavigationStack {
            ZStack {
                TeaBackground()

                ScrollView(.vertical, showsIndicators: false) {
                    VStack(alignment: .leading, spacing: 30) {
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
                    .padding(.horizontal, 72)
                    .padding(.top, 54)
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
        if model.isLoading && model.continueWatching.isEmpty {
            VStack(alignment: .leading, spacing: 16) {
                shelfTitle("Continue Watching")
                HStack(spacing: 18) {
                    ProgressView()
                        .tint(Color.teaAccent)
                    Text("Steeping your movie shelf…")
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
                title: "Continue Watching",
                items: model.continueWatching.map {
                    NetflixShelfItem(movie: $0.movie, progress: $0.progress)
                },
                requestsInitialFocus: true
            )
        }
    }

    @ViewBuilder
    private func discoverySection(_ section: DiscoverySection) -> some View {
        if !section.items.isEmpty {
            NetflixMovieShelf(
                title: section.title,
                items: section.items.map { NetflixShelfItem(movie: $0) }
            )
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
