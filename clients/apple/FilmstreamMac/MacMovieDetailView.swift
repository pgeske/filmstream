import FilmstreamCore
import SwiftUI

struct MacMovieDetailView: View {
    @Environment(MacAppModel.self) private var model
    let movie: Movie

    @State private var isPreparing = false
    @State private var isRemoving = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? {
        model.history(for: movie)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: movie)
    }

    var body: some View {
        GeometryReader { geometry in
            ZStack(alignment: .leading) {
                MacTeaBackground()

                MacBackdropImage(movie: movie)
                    .frame(width: geometry.size.width, height: geometry.size.height)
                    .clipped()

                LinearGradient(
                    stops: [
                        .init(color: Color.macTeaBackground.opacity(0.98), location: 0),
                        .init(color: Color.macTeaBackground.opacity(0.86), location: 0.38),
                        .init(color: Color.macTeaBackground.opacity(0.18), location: 0.78),
                        .init(color: .clear, location: 1),
                    ],
                    startPoint: .leading,
                    endPoint: .trailing
                )

                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.5),
                        .init(color: Color.macTeaBackground.opacity(0.7), location: 0.86),
                        .init(color: Color.macTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )

                ScrollView {
                    VStack(alignment: .leading, spacing: 17) {
                        Spacer(minLength: 70)

                        MacTeaStreamMark(size: 40)

                        Text(movie.title)
                            .font(.system(size: 46, weight: .black, design: .rounded))
                            .foregroundStyle(Color.macTeaCream)
                            .lineLimit(2)
                            .frame(maxWidth: 720, alignment: .leading)

                        metadataLine

                        if let overview = movie.overview, !overview.isEmpty {
                            Text(overview)
                                .font(.body)
                                .foregroundStyle(Color.macTeaCream.opacity(0.88))
                                .lineSpacing(4)
                                .frame(maxWidth: 680, alignment: .leading)
                        }

                        if let errorMessage {
                            Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(Color.macTeaAmber)
                                .font(.headline)
                                .frame(maxWidth: 680, alignment: .leading)
                        }

                        actionButtons
                    }
                    .padding(.horizontal, 46)
                    .padding(.vertical, 36)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(width: geometry.size.width, height: geometry.size.height)
            }
        }
        .background(Color.macTeaBackground)
        .navigationTitle(movie.title)
        .task(id: movie.id) {
            await model.loadRatings(for: movie)
        }
    }

    @ViewBuilder
    private var metadataLine: some View {
        HStack(spacing: 11) {
            Text("Movie")
            if let year = movie.year {
                Text("•")
                Text(String(year))
            }
            MacMovieRatingBadges(ratings: ratings, tmdbRating: movie.voteAverage)
            if let history, history.progress > 0 {
                Text("•")
                Text("\(Int(history.progress * 100))% watched")
                    .foregroundStyle(Color.macTeaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.macTeaMuted)
    }

    private var actionButtons: some View {
        VStack(alignment: .leading, spacing: 10) {
            Button {
                Task { await preparePlayback(startSeconds: history?.positionSeconds ?? 0) }
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isPreparing
                )
            }
            .buttonStyle(MacDetailButtonStyle(kind: .prominent))
            .disabled(isPreparing || isRemoving)
            .keyboardShortcut(.defaultAction)

            if history != nil {
                Button {
                    Task { await preparePlayback(startSeconds: 0) }
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(MacDetailButtonStyle(kind: .standard))
                .disabled(isPreparing || isRemoving)

                Button(role: .destructive) {
                    Task { await removeFromContinueWatching() }
                } label: {
                    actionLabel(
                        title: isRemoving ? "Removing…" : "Remove from Continue Watching",
                        systemImage: "xmark",
                        showsProgress: isRemoving
                    )
                }
                .buttonStyle(MacDetailButtonStyle(kind: .destructive))
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 3)
    }

    private var primaryButtonTitle: String {
        if isPreparing {
            return "Preparing Stream…"
        }
        return history == nil ? "Play" : "Resume"
    }

    private func actionLabel(
        title: String,
        systemImage: String,
        showsProgress: Bool = false
    ) -> some View {
        HStack(spacing: 11) {
            if showsProgress {
                ZStack {
                    Circle()
                        .fill(Color.macTeaBackground.opacity(0.08))
                        .frame(width: 26, height: 26)
                    ProgressView()
                        .controlSize(.small)
                        .tint(Color.macTeaBackground)
                }
                .frame(width: 28, height: 28)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 20)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
        .frame(width: 420)
    }

    private func preparePlayback(startSeconds: Double) async {
        isPreparing = true
        defer { isPreparing = false }
        do {
            let playback = try await model.api.createPlayback(for: movie)
            let prepared = try await model.api.prepareNativePlayback(
                playback,
                startSeconds: startSeconds
            )
            model.presentPlayback(movie: movie, prepared: prepared)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    private func removeFromContinueWatching() async {
        guard let history else { return }
        isRemoving = true
        defer { isRemoving = false }
        do {
            try await model.removeFromContinueWatching(history)
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
