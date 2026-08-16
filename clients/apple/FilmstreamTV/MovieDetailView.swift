import FilmstreamCore
import SwiftUI

struct MovieDetailView: View {
    @Environment(AppModel.self) private var model
    let movie: Movie

    @State private var preparedPlayback: PreparedPlayback?
    @State private var isPreparing = false
    @State private var preparationStage: PlaybackPreparationStage?
    @State private var isRemoving = false
    @State private var errorMessage: String?
    @FocusState private var focusedAction: DetailAction?

    private enum DetailAction: Hashable {
        case play
        case startOver
        case remove
    }

    private var history: WatchHistoryEntry? {
        model.history(for: movie)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: movie)
    }

    var body: some View {
        ZStack(alignment: .leading) {
            BackdropImage(movie: movie)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .clipped()
                .ignoresSafeArea()

            LinearGradient(
                stops: [
                    .init(color: Color.teaBackground.opacity(0.98), location: 0),
                    .init(color: Color.teaBackground.opacity(0.88), location: 0.32),
                    .init(color: Color.teaBackground.opacity(0.22), location: 0.72),
                    .init(color: .clear, location: 1),
                ],
                startPoint: .leading,
                endPoint: .trailing
            )
            .ignoresSafeArea()

            LinearGradient(
                stops: [
                    .init(color: .clear, location: 0.48),
                    .init(color: Color.teaBackground.opacity(0.62), location: 0.82),
                    .init(color: Color.teaBackground, location: 1),
                ],
                startPoint: .top,
                endPoint: .bottom
            )
            .ignoresSafeArea()

            VStack(alignment: .leading, spacing: 20) {
                Spacer(minLength: 36)

                TeaStreamMark(size: 48)

                Text(movie.title)
                    .font(.system(size: 62, weight: .black, design: .rounded))
                    .foregroundStyle(Color.teaCream)
                    .lineLimit(2)
                    .frame(maxWidth: 780, alignment: .leading)

                metadataLine

                if let overview = movie.overview, !overview.isEmpty {
                    Text(overview)
                        .font(.title3)
                        .foregroundStyle(Color.teaCream.opacity(0.9))
                        .lineSpacing(4)
                        .lineLimit(4)
                        .frame(maxWidth: 760, alignment: .leading)
                }

                if let errorMessage {
                    Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(Color.teaAmber)
                        .font(.headline)
                        .frame(maxWidth: 720, alignment: .leading)
                }

                actionButtons
            }
            .padding(.leading, 82)
            .padding(.trailing, 60)
            .padding(.vertical, 54)
        }
        .background(Color.teaBackground)
        .task(id: movie.id) {
            focusedAction = .play
            async let ratings: Void = model.loadRatings(for: movie)
            async let prewarm: Void = model.prewarmPlayback(
                for: movie,
                startSeconds: history?.positionSeconds ?? 0
            )
            _ = await (ratings, prewarm)
        }
        .fullScreenCover(
            item: $preparedPlayback,
            onDismiss: {
                Task { await model.loadContinueWatching() }
            }
        ) { prepared in
            PlayerView(movie: movie, prepared: prepared, api: model.api)
        }
    }

    @ViewBuilder
    private var metadataLine: some View {
        HStack(spacing: 15) {
            Text(movie.genreSummary ?? "Movie")
            if let year = movie.year {
                Text("•")
                Text(String(year))
            }
            if let contentRating = ratings?.contentRating {
                Text("•")
                Text(contentRating)
            }
            MovieRatingBadges(ratings: ratings)
            if let history, history.progress > 0 {
                Text("•")
                Text("\(Int(history.progress * 100))% watched")
                    .foregroundStyle(Color.teaAccentLight)
            }
        }
        .font(.headline)
        .foregroundStyle(Color.teaMuted)
    }

    private var actionButtons: some View {
        VStack(alignment: .leading, spacing: 12) {
            Button {
                Task { await preparePlayback(startSeconds: history?.positionSeconds ?? 0) }
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isPreparing,
                    progressTint: Color.teaBackground
                )
            }
            .buttonStyle(TeaDetailActionButtonStyle(kind: .prominent))
            .focusEffectDisabled()
            .focused($focusedAction, equals: .play)
            .disabled(isPreparing || isRemoving)

            if history != nil {
                Button {
                    Task { await preparePlayback(startSeconds: 0) }
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(TeaDetailActionButtonStyle(kind: .standard))
                .focusEffectDisabled()
                .focused($focusedAction, equals: .startOver)
                .disabled(isPreparing || isRemoving)

                Button {
                    Task { await removeFromContinueWatching() }
                } label: {
                    actionLabel(
                        title: isRemoving ? "Removing…" : "Remove from Continue Watching",
                        systemImage: "xmark",
                        showsProgress: isRemoving
                    )
                }
                .buttonStyle(TeaDetailActionButtonStyle(kind: .destructive))
                .focusEffectDisabled()
                .focused($focusedAction, equals: .remove)
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 4)
    }

    private var primaryButtonTitle: String {
        switch preparationStage {
        case .findingRelease:
            return "Finding a Release…"
        case .bufferingVideo:
            return "Buffering Video…"
        case nil:
            return history == nil ? "Play" : "Resume"
        }
    }

    private func actionLabel(
        title: String,
        systemImage: String,
        showsProgress: Bool = false,
        progressTint: Color = .teaCream
    ) -> some View {
        HStack(spacing: 14) {
            if showsProgress {
                ProgressView()
                    .tint(progressTint)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 26)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
        .font(.headline.weight(.bold))
        .frame(width: 740)
    }

    private func preparePlayback(startSeconds: Double) async {
        isPreparing = true
        defer {
            isPreparing = false
            preparationStage = nil
        }
        do {
            preparedPlayback = try await model.preparePlayback(
                for: movie,
                startSeconds: startSeconds,
                onStage: { preparationStage = $0 }
            )
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
            focusedAction = .play
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
