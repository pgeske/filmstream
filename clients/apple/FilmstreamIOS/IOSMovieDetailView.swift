import FilmstreamCore
import SwiftUI

struct IOSMovieDetailView: View {
    @Environment(IOSAppModel.self) private var model
    let movie: Movie

    @State private var preparedPlayback: PreparedPlayback?
    @State private var isPreparing = false
    @State private var preparationStage: PlaybackPreparationStage?
    @State private var isRemoving = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? {
        model.history(for: movie)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: movie)
    }

    private var metadataSummary: String {
        var values = [movie.genreSummary ?? "Movie"]
        if let year = movie.year {
            values.append(String(year))
        }
        if let contentRating = ratings?.contentRating {
            values.append(contentRating)
        }
        return values.joined(separator: " • ")
    }

    var body: some View {
        GeometryReader { geometry in
            let layout = IOSAdaptiveLayout(
                width: geometry.size.width,
                height: geometry.size.height
            )
            if layout.usesCinematicDetail {
                cinematicLayout(width: geometry.size.width)
            } else {
                compactLayout(width: geometry.size.width, layout: layout)
            }
        }
        .navigationTitle("")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        .toolbarBackground(.hidden, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
        .task(id: movie.id) {
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
            IOSPlayerView(movie: movie, prepared: prepared, api: model.api)
        }
    }

    private func compactLayout(width: CGFloat, layout: IOSAdaptiveLayout) -> some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    backdrop(width: width)
                    detailContent(showsTitle: false)
                        .frame(maxWidth: layout.isWide ? 760 : .infinity)
                        .frame(maxWidth: .infinity)
                }
                .padding(.bottom, 36)
            }
            .frame(width: width)
            .ignoresSafeArea(edges: .top)
        }
    }

    private func cinematicLayout(width: CGFloat) -> some View {
        ZStack(alignment: .leading) {
            IOSCinematicDetailBackground(movie: movie)

            ScrollView(.vertical, showsIndicators: false) {
                detailContent(showsTitle: true)
                    .frame(maxWidth: min(600, width * 0.62), alignment: .leading)
                    .padding(.leading, max(44, width * 0.06))
                    .padding(.trailing, 30)
                    .padding(.vertical, 54)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    private func backdrop(width: CGFloat) -> some View {
        MobileBackdropImage(movie: movie)
            .frame(width: width, height: 360)
            .clipped()
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.2),
                        .init(color: Color.mobileTeaBackground.opacity(0.52), location: 0.64),
                        .init(color: Color.mobileTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )
            }
            .overlay {
                LinearGradient(
                    colors: [Color.mobileTeaBackground.opacity(0.46), .clear],
                    startPoint: .leading,
                    endPoint: .trailing
                )
            }
            .overlay {
                LinearGradient(
                    colors: [.black.opacity(0.42), .clear],
                    startPoint: .top,
                    endPoint: .center
                )
            }
            .overlay(alignment: .bottomLeading) {
                Text(movie.title)
                    .font(.system(size: 32, weight: .black, design: .rounded))
                    .foregroundStyle(Color.mobileTeaCream)
                    .lineLimit(3)
                    .padding(.horizontal, 18)
                    .padding(.bottom, 22)
            }
    }

    private func detailContent(showsTitle: Bool) -> some View {
        VStack(alignment: .leading, spacing: showsTitle ? 20 : 18) {
            if showsTitle {
                Text(movie.title)
                    .font(.system(size: 48, weight: .black, design: .rounded))
                    .foregroundStyle(Color.mobileTeaCream)
                    .lineLimit(2)
            }

            VStack(alignment: .leading, spacing: 6) {
                Text(metadataSummary)
                    .foregroundStyle(Color.mobileTeaMuted)

                if let history, history.progress > 0 {
                    Text("\(Int(history.progress * 100))% watched")
                        .foregroundStyle(Color.mobileTeaAccentLight)
                }
            }
            .font(.subheadline.weight(.semibold))

            MobileRatingBadges(ratings: ratings)

            if let overview = movie.overview, !overview.isEmpty {
                Text(overview)
                    .font(.body)
                    .foregroundStyle(Color.mobileTeaCream.opacity(0.88))
                    .lineSpacing(3)
            }

            if let errorMessage {
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Color.mobileTeaAmber)
                    .padding(13)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(
                        Color.mobileTeaPanel.opacity(0.72),
                        in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                    )
            }

            actionButtons

            if let history, history.progress > 0 {
                ProgressView(value: history.progress)
                    .tint(Color.mobileTeaAccent)
                    .accessibilityLabel("Movie progress")
                    .accessibilityValue("\(Int(history.progress * 100)) percent")
            }
        }
        .padding(.horizontal, 18)
    }

    private var actionButtons: some View {
        VStack(spacing: 10) {
            Button {
                Task { await preparePlayback(startSeconds: history?.positionSeconds ?? 0) }
            } label: {
                actionLabel(
                    title: primaryButtonTitle,
                    systemImage: "play.fill",
                    showsProgress: isPreparing,
                    progressTint: Color.mobileTeaBackground
                )
            }
            .buttonStyle(MobileDetailButtonStyle(kind: .prominent))
            .disabled(isPreparing || isRemoving)

            if history != nil {
                Button {
                    Task { await preparePlayback(startSeconds: 0) }
                } label: {
                    actionLabel(title: "Play from Beginning", systemImage: "arrow.counterclockwise")
                }
                .buttonStyle(MobileDetailButtonStyle(kind: .standard))
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
                .buttonStyle(MobileDetailButtonStyle(kind: .destructive))
                .disabled(isPreparing || isRemoving)
            }
        }
        .padding(.top, 2)
    }

    private var primaryButtonTitle: String {
        switch preparationStage {
        case .findingRelease: "Finding a Release…"
        case .bufferingVideo: "Buffering Video…"
        case nil: history == nil ? "Play" : "Resume"
        }
    }

    private func actionLabel(
        title: String,
        systemImage: String,
        showsProgress: Bool = false,
        progressTint: Color = .mobileTeaCream
    ) -> some View {
        HStack(spacing: 11) {
            if showsProgress {
                ProgressView()
                    .controlSize(.small)
                    .tint(progressTint)
            } else {
                Image(systemName: systemImage)
                    .frame(width: 20)
            }
            Text(title)
                .lineLimit(1)
            Spacer()
        }
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
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
