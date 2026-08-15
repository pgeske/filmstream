import FilmstreamCore
import SwiftUI

struct IOSMovieDetailView: View {
    @Environment(IOSAppModel.self) private var model
    let movie: Movie

    @State private var preparedPlayback: PreparedPlayback?
    @State private var isPreparing = false
    @State private var errorMessage: String?

    private var history: WatchHistoryEntry? {
        model.history(for: movie)
    }

    private var ratings: MovieRatings? {
        model.ratings(for: movie)
    }

    var body: some View {
        ZStack {
            MobileTeaBackground()

            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    backdrop
                    content
                }
                .padding(.bottom, 36)
            }
            .ignoresSafeArea(edges: .top)
        }
        .navigationTitle(movie.title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackground(Color.mobileTeaBackground.opacity(0.9), for: .navigationBar)
        .task(id: movie.id) {
            await model.loadRatings(for: movie)
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

    private var backdrop: some View {
        MobileBackdropImage(movie: movie)
            .frame(maxWidth: .infinity)
            .frame(height: 285)
            .clipped()
            .overlay {
                LinearGradient(
                    stops: [
                        .init(color: .clear, location: 0.18),
                        .init(color: Color.mobileTeaBackground.opacity(0.68), location: 0.68),
                        .init(color: Color.mobileTeaBackground, location: 1),
                    ],
                    startPoint: .top,
                    endPoint: .bottom
                )
            }
            .overlay {
                LinearGradient(
                    colors: [Color.mobileTeaBackground.opacity(0.48), .clear],
                    startPoint: .leading,
                    endPoint: .trailing
                )
            }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 20) {
            HStack(alignment: .bottom, spacing: 16) {
                MobilePosterImage(movie: movie)
                    .frame(width: 116, height: 174)
                    .overlay {
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .stroke(Color.mobileTeaAccentLight.opacity(0.22), lineWidth: 1)
                    }
                    .shadow(color: .black.opacity(0.42), radius: 16, y: 8)

                VStack(alignment: .leading, spacing: 10) {
                    Text(movie.title)
                        .font(.system(size: 27, weight: .bold, design: .rounded))
                        .foregroundStyle(Color.mobileTeaCream)
                        .lineLimit(3)

                    HStack(spacing: 10) {
                        if let year = movie.year {
                            Text(String(year))
                        }
                        if let history, history.progress > 0 {
                            Text("\(Int(history.progress * 100))% watched")
                                .foregroundStyle(Color.mobileTeaAccentLight)
                        }
                    }
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Color.mobileTeaMuted)
                }
                .padding(.bottom, 8)
            }
            .offset(y: -58)
            .padding(.bottom, -50)

            MobileRatingBadges(ratings: ratings, tmdbRating: movie.voteAverage)

            if let overview = movie.overview, !overview.isEmpty {
                Text(overview)
                    .font(.body)
                    .foregroundStyle(Color.mobileTeaCream.opacity(0.86))
                    .lineSpacing(3)
            }

            if let errorMessage {
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote.weight(.semibold))
                    .foregroundStyle(Color.mobileTeaAmber)
            }

            Button {
                Task { await preparePlayback() }
            } label: {
                HStack(spacing: 9) {
                    if isPreparing {
                        ProgressView()
                            .tint(Color.mobileTeaBackground)
                    } else {
                        Image(systemName: "play.fill")
                    }
                    Text(playButtonTitle)
                }
            }
            .buttonStyle(MobilePrimaryButtonStyle())
            .disabled(isPreparing)

            if let history, history.progress > 0 {
                ProgressView(value: history.progress)
                    .tint(Color.mobileTeaAccent)
                    .accessibilityLabel("Movie progress")
                    .accessibilityValue("\(Int(history.progress * 100)) percent")
            }
        }
        .padding(.horizontal, 18)
    }

    private var playButtonTitle: String {
        if isPreparing {
            return "Preparing Stream…"
        }
        return history == nil ? "Play" : "Resume"
    }

    private func preparePlayback() async {
        isPreparing = true
        defer { isPreparing = false }
        do {
            let playback = try await model.api.createPlayback(for: movie)
            preparedPlayback = try await model.api.prepareNativePlayback(
                playback,
                startSeconds: history?.positionSeconds ?? 0
            )
            errorMessage = nil
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
