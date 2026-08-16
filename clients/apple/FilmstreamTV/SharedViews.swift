import FilmstreamCore
import Foundation
import SwiftUI
import UIKit

extension Color {
    static let teaBackground = Color(red: 0.027, green: 0.067, blue: 0.047)
    static let teaBackgroundTop = Color(red: 0.063, green: 0.145, blue: 0.102)
    static let teaPanel = Color(red: 0.071, green: 0.153, blue: 0.110)
    static let teaPanelElevated = Color(red: 0.106, green: 0.212, blue: 0.153)
    static let teaAccent = Color(red: 0.624, green: 0.769, blue: 0.482)
    static let teaAccentLight = Color(red: 0.784, green: 0.867, blue: 0.663)
    static let teaCream = Color(red: 0.953, green: 0.922, blue: 0.867)
    static let teaMuted = Color(red: 0.682, green: 0.725, blue: 0.659)
    static let teaAmber = Color(red: 0.839, green: 0.631, blue: 0.373)
    static let teaHoney = Color(red: 0.855, green: 0.694, blue: 0.365)
    static let teaTomato = Color(red: 0.780, green: 0.302, blue: 0.251)
}

struct TeaBackground: View {
    var body: some View {
        ZStack {
            LinearGradient(
                colors: [Color.teaBackgroundTop, Color.teaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            RadialGradient(
                colors: [Color.teaAccent.opacity(0.14), .clear],
                center: .topTrailing,
                startRadius: 20,
                endRadius: 920
            )
            RadialGradient(
                colors: [Color.teaAmber.opacity(0.055), .clear],
                center: .bottomLeading,
                startRadius: 10,
                endRadius: 760
            )
        }
        .ignoresSafeArea()
    }
}

struct TeaStreamMark: View {
    var size: CGFloat = 54

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.3, style: .continuous)
                .fill(
                    LinearGradient(
                        colors: [Color.teaAccentLight, Color.teaAccent],
                        startPoint: .topLeading,
                        endPoint: .bottomTrailing
                    )
                )
            Image(systemName: "leaf.fill")
                .font(.system(size: size * 0.58, weight: .semibold))
                .foregroundStyle(Color.teaBackground)
                .rotationEffect(.degrees(-18))
            Image(systemName: "play.fill")
                .font(.system(size: size * 0.22, weight: .black))
                .foregroundStyle(Color.teaCream)
                .offset(x: -1)
        }
        .frame(width: size, height: size)
        .shadow(color: Color.teaAccent.opacity(0.22), radius: 18, y: 8)
        .accessibilityHidden(true)
    }
}

struct BrandHeader: View {
    var body: some View {
        HStack(spacing: 16) {
            TeaStreamMark()
            HStack(spacing: 0) {
                Text("TEA")
                    .foregroundStyle(Color.teaAccentLight)
                Text("STREAM")
                    .foregroundStyle(Color.teaCream)
            }
            .font(.system(size: 35, weight: .black, design: .rounded))
            .tracking(2.2)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("TeaStream")
    }
}

struct TeaActionButtonStyle: ButtonStyle {
    var prominent = false

    func makeBody(configuration: Configuration) -> some View {
        TeaActionButtonBody(
            configuration: configuration,
            prominent: prominent
        )
    }
}

private struct TeaActionButtonBody: View {
    let configuration: ButtonStyle.Configuration
    let prominent: Bool

    @Environment(\.isFocused) private var isFocused

    var body: some View {
        configuration.label
            .foregroundStyle(isFocused ? Color.teaBackground : Color.teaCream)
            .padding(.horizontal, 24)
            .padding(.vertical, 14)
            .background(
                isFocused
                    ? Color.teaAccentLight
                    : prominent ? Color.teaAccent : Color.teaPanelElevated,
                in: Capsule()
            )
            .overlay {
                Capsule()
                    .stroke(
                        isFocused ? Color.teaCream.opacity(0.62) : Color.teaAccent.opacity(0.32),
                        lineWidth: 1.5
                    )
            }
            .shadow(
                color: isFocused ? Color.teaAccent.opacity(0.36) : .black.opacity(0.18),
                radius: isFocused ? 24 : 10,
                y: isFocused ? 10 : 6
            )
            .scaleEffect(configuration.isPressed ? 0.97 : isFocused ? 1.055 : 1)
            .animation(.snappy(duration: 0.2), value: isFocused)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

enum TeaDetailButtonKind: Equatable {
    case prominent
    case standard
    case destructive
}

struct TeaDetailActionButtonStyle: ButtonStyle {
    let kind: TeaDetailButtonKind

    func makeBody(configuration: Configuration) -> some View {
        TeaDetailActionButtonBody(configuration: configuration, kind: kind)
    }
}

private struct TeaDetailActionButtonBody: View {
    let configuration: ButtonStyle.Configuration
    let kind: TeaDetailButtonKind

    @Environment(\.isFocused) private var isFocused

    private var foregroundColor: Color {
        if isFocused || kind == .prominent {
            return Color.teaBackground
        }
        return kind == .destructive ? Color.teaCream.opacity(0.9) : Color.teaCream
    }

    private var backgroundColor: Color {
        if isFocused {
            return kind == .destructive ? Color.teaAmber : Color.teaAccentLight
        }
        switch kind {
        case .prominent:
            return Color.teaAccent
        case .standard:
            return Color.teaPanelElevated.opacity(0.9)
        case .destructive:
            return Color.teaBackground.opacity(0.72)
        }
    }

    var body: some View {
        configuration.label
            .foregroundStyle(foregroundColor)
            .padding(.horizontal, 22)
            .padding(.vertical, 15)
            .background(backgroundColor, in: RoundedRectangle(cornerRadius: 13, style: .continuous))
            .overlay {
                RoundedRectangle(cornerRadius: 13, style: .continuous)
                    .stroke(
                        kind == .destructive ? Color.teaAmber.opacity(isFocused ? 0.8 : 0.42) : Color.teaCream.opacity(isFocused ? 0.52 : 0.14),
                        lineWidth: isFocused ? 2 : 1
                    )
            }
            .shadow(
                color: isFocused ? Color.teaAccent.opacity(0.32) : .black.opacity(0.25),
                radius: isFocused ? 22 : 10,
                y: isFocused ? 10 : 5
            )
            .scaleEffect(configuration.isPressed ? 0.985 : isFocused ? 1.025 : 1, anchor: .leading)
            .animation(.snappy(duration: 0.18), value: isFocused)
            .animation(.easeOut(duration: 0.1), value: configuration.isPressed)
    }
}

struct MovieRatingBadges: View {
    let ratings: MovieRatings?
    var tmdbRating: Double? = nil

    var body: some View {
        HStack(spacing: 10) {
            if let tmdbRating, tmdbRating > 0 {
                RatingBadge(source: .tmdb, value: String(format: "%.1f", tmdbRating))
            }
            if let imdb = ratings?.imdb {
                RatingBadge(source: .imdb, value: String(format: "%.1f", imdb))
            }
            if let rottenTomatoes = ratings?.rottenTomatoes {
                RatingBadge(source: .rottenTomatoes, value: "\(rottenTomatoes)%")
            }
        }
        .accessibilityElement(children: .combine)
    }
}

private enum RatingSource {
    case tmdb
    case imdb
    case rottenTomatoes

    var accessibilityName: String {
        switch self {
        case .tmdb: "TMDB"
        case .imdb: "IMDb"
        case .rottenTomatoes: "Rotten Tomatoes"
        }
    }
}

private struct RatingBadge: View {
    let source: RatingSource
    let value: String

    var body: some View {
        HStack(spacing: 8) {
            brandMark
            Text(value)
                .font(.system(size: 18, weight: .semibold, design: .rounded))
                .monospacedDigit()
                .foregroundStyle(Color.teaCream.opacity(0.94))
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 7)
        .background(
            Color.teaPanel.opacity(0.82),
            in: RoundedRectangle(cornerRadius: 11, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 11, style: .continuous)
                .stroke(Color.teaCream.opacity(0.12), lineWidth: 1)
        }
        .shadow(color: .black.opacity(0.16), radius: 7, y: 3)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(source.accessibilityName) \(value)")
    }

    @ViewBuilder
    private var brandMark: some View {
        switch source {
        case .tmdb:
            HStack(spacing: 3) {
                Image(systemName: "film.fill")
                Text("TMDB")
            }
            .font(.system(size: 13, weight: .bold, design: .rounded))
            .foregroundStyle(Color.teaAccentLight)
        case .imdb:
            Text("IMDb")
                .font(.system(size: 13, weight: .black, design: .rounded))
                .foregroundStyle(Color.teaBackground)
                .padding(.horizontal, 5)
                .padding(.vertical, 3)
                .background(
                    Color.teaHoney,
                    in: RoundedRectangle(cornerRadius: 4, style: .continuous)
                )
        case .rottenTomatoes:
            TomatoMark(size: 23)
        }
    }
}

private struct TomatoMark: View {
    let size: CGFloat

    var body: some View {
        ZStack {
            Circle()
                .fill(Color.teaTomato)
                .frame(width: size * 0.78, height: size * 0.78)
                .offset(y: size * 0.08)
            Circle()
                .fill(Color.teaCream.opacity(0.26))
                .frame(width: size * 0.16, height: size * 0.16)
                .offset(x: -size * 0.16, y: -size * 0.02)
            Image(systemName: "leaf.fill")
                .font(.system(size: size * 0.48, weight: .bold))
                .foregroundStyle(Color.teaAccent)
                .rotationEffect(.degrees(-22))
                .offset(y: -size * 0.27)
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

struct NetflixShelfItem: Identifiable {
    let movie: Movie
    var progress: Double? = nil
    var contentRating: String? = nil

    var id: String { movie.id }
}

private enum NetflixShelfLayout {
    static let collapsedWidth: CGFloat = 223
    static let expandedWidth: CGFloat = 632
    static let artworkHeight: CGFloat = 334
    static let firstCardInset: CGFloat = 88
    static let viewportOverflow: CGFloat = 72
    static let trailingScrollSpace: CGFloat = 1240
    static let focusedAnchor = UnitPoint(x: 0.07, y: 0.5)
}

struct NetflixMovieShelf: View {
    let title: String
    let items: [NetflixShelfItem]
    var requestsInitialFocus = false
    var onFocus: (Movie) -> Void = { _ in }

    @State private var expandedMovieID: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(title)
                .font(.system(size: 38, weight: .bold, design: .rounded))
                .foregroundStyle(Color.teaCream)
                .padding(.horizontal, 16)

            ScrollViewReader { proxy in
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(alignment: .top, spacing: 24) {
                        ForEach(items) { item in
                            NetflixShelfCard(
                                item: item,
                                isExpanded: expandedMovieID == item.id,
                                requestsInitialFocus: requestsInitialFocus && item.id == items.first?.id
                            ) { isFocused in
                                if isFocused {
                                    onFocus(item.movie)
                                    withAnimation(.easeInOut(duration: 0.28)) {
                                        expandedMovieID = item.id
                                    }
                                    Task { @MainActor in
                                        await Task.yield()
                                        guard expandedMovieID == item.id else { return }
                                        withAnimation(.easeInOut(duration: 0.32)) {
                                            proxy.scrollTo(item.id, anchor: NetflixShelfLayout.focusedAnchor)
                                        }
                                    }
                                } else if expandedMovieID == item.id {
                                    Task { @MainActor in
                                        try? await Task.sleep(for: .milliseconds(80))
                                        guard expandedMovieID == item.id else { return }
                                        withAnimation(.easeInOut(duration: 0.2)) {
                                            expandedMovieID = nil
                                        }
                                    }
                                }
                            }
                            .id(item.id)
                        }

                        Color.clear
                            .frame(width: NetflixShelfLayout.trailingScrollSpace, height: 1)
                            .accessibilityHidden(true)
                    }
                    .padding(.leading, NetflixShelfLayout.firstCardInset)
                    .padding(.top, 12)
                    .padding(.bottom, 18)
                }
                .padding(.horizontal, -NetflixShelfLayout.viewportOverflow)
            }
        }
        .focusSection()
    }
}

private struct NetflixShelfCard: View {
    let item: NetflixShelfItem
    let isExpanded: Bool
    let requestsInitialFocus: Bool
    let onFocusChange: (Bool) -> Void

    @FocusState private var isFocused: Bool

    private var cardWidth: CGFloat {
        isExpanded ? NetflixShelfLayout.expandedWidth : NetflixShelfLayout.collapsedWidth
    }

    var body: some View {
        NavigationLink(value: item.movie) {
            VStack(alignment: .leading, spacing: 12) {
                artwork
                    .frame(width: cardWidth, height: NetflixShelfLayout.artworkHeight)
                    .clipped()
                    .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
                    .overlay {
                        RoundedRectangle(cornerRadius: 18, style: .continuous)
                            .stroke(
                                isExpanded ? Color.teaAccentLight.opacity(0.9) : Color.teaCream.opacity(0.1),
                                lineWidth: isExpanded ? 3 : 1
                            )
                    }
                    .overlay(alignment: .bottom) {
                        if let progress = item.progress, progress > 0 {
                            ProgressView(value: progress)
                                .tint(Color.teaAccent)
                                .padding(.horizontal, 12)
                                .padding(.bottom, 10)
                        }
                    }
                    .shadow(
                        color: isExpanded ? Color.teaAccent.opacity(0.24) : .black.opacity(0.36),
                        radius: isExpanded ? 26 : 12,
                        y: isExpanded ? 13 : 7
                    )

                if isExpanded {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(item.movie.title)
                            .font(.system(size: 28, weight: .bold, design: .rounded))
                            .foregroundStyle(Color.teaCream)
                            .lineLimit(2)

                        HStack(spacing: 7) {
                            Text(item.movie.primaryGenre ?? "Movie")
                            if let year = item.movie.year {
                                Text("•")
                                Text(String(year))
                            }
                            if let contentRating = item.contentRating {
                                Text("•")
                                Text(contentRating)
                            }
                            if let progress = item.progress, progress > 0 {
                                Text("•")
                                Text("\(Int(progress * 100))% watched")
                                    .foregroundStyle(Color.teaAccentLight)
                            }
                        }
                        .font(.system(size: 23, weight: .semibold))
                        .foregroundStyle(Color.teaMuted)

                        if let overview = item.movie.overview, !overview.isEmpty {
                            Text(overview)
                                .font(.system(size: 22, weight: .regular))
                                .foregroundStyle(Color.teaCream.opacity(0.78))
                                .lineSpacing(2)
                                .lineLimit(2)
                                .frame(maxWidth: cardWidth, alignment: .leading)
                        }
                    }
                    .transition(.opacity)
                }
            }
            .frame(width: cardWidth, alignment: .leading)
            .contentShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
        }
        .buttonStyle(MovieCardButtonStyle())
        .focusEffectDisabled()
        .focused($isFocused)
        .zIndex(isExpanded ? 1 : 0)
        .animation(.easeInOut(duration: 0.28), value: isExpanded)
        .onChange(of: isFocused) { _, focused in
            onFocusChange(focused)
        }
        .task {
            if requestsInitialFocus {
                isFocused = true
            }
        }
    }

    private var artwork: some View {
        ZStack {
            if isExpanded {
                NetflixShelfArtwork(
                    movie: item.movie,
                    landscape: true,
                    width: cardWidth,
                    height: NetflixShelfLayout.artworkHeight
                )
                .transition(.opacity)
            } else {
                NetflixShelfArtwork(
                    movie: item.movie,
                    landscape: false,
                    width: cardWidth,
                    height: NetflixShelfLayout.artworkHeight
                )
                .transition(.opacity)
            }
        }
        .animation(.easeInOut(duration: 0.2), value: isExpanded)
    }
}

private struct NetflixShelfArtwork: View {
    let movie: Movie
    let landscape: Bool
    let width: CGFloat
    let height: CGFloat

    var body: some View {
        AsyncImage(url: landscape ? (movie.backdropURL ?? movie.posterURL) : (movie.posterURL ?? movie.backdropURL)) { phase in
            switch phase {
            case let .success(image):
                image
                    .resizable()
                    .scaledToFill()
            case .empty:
                ZStack {
                    Color.teaPanel
                    ProgressView()
                        .tint(Color.teaAccent)
                }
            default:
                LinearGradient(
                    colors: [Color.teaPanelElevated, Color.teaBackground],
                    startPoint: .topLeading,
                    endPoint: .bottomTrailing
                )
                .overlay {
                    Image(systemName: "film.stack")
                        .font(.system(size: 52))
                        .foregroundStyle(Color.teaAccentLight.opacity(0.38))
                }
            }
        }
        .frame(width: width, height: height)
        .clipped()
        .overlay {
            LinearGradient(
                colors: [.clear, Color.teaBackground.opacity(0.2)],
                startPoint: .top,
                endPoint: .bottom
            )
        }
        .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
    }
}

private struct MovieCardButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(configuration.isPressed ? 0.82 : 1)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

struct MovieNavigationCard: View {
    let movie: Movie
    var progress: Double? = nil
    var requestsInitialFocus = false
    var action: (() -> Void)? = nil
    @FocusState private var isFocused: Bool

    private let cardWidth: CGFloat = 250

    var body: some View {
        Group {
            if let action {
                Button(action: action) {
                    cardContent
                }
            } else {
                NavigationLink(value: movie) {
                    cardContent
                }
            }
        }
        .buttonStyle(MovieCardButtonStyle())
        .focusEffectDisabled()
        .focused($isFocused)
        .zIndex(isFocused ? 1 : 0)
        .task {
            if requestsInitialFocus {
                isFocused = true
            }
        }
    }

    private var cardContent: some View {
        VStack(alignment: .leading, spacing: 13) {
            PosterImage(movie: movie)
                .frame(width: cardWidth, height: 375)
                .overlay {
                    RoundedRectangle(cornerRadius: 20, style: .continuous)
                        .stroke(
                            isFocused ? Color.teaAccentLight.opacity(0.82) : Color.teaCream.opacity(0.12),
                            lineWidth: isFocused ? 2.5 : 1
                        )
                }
                .overlay(alignment: .bottom) {
                    if let progress, progress > 0 {
                        ProgressView(value: progress)
                            .tint(Color.teaAccent)
                            .padding(.horizontal, 12)
                            .padding(.bottom, 11)
                    }
                }
                .overlay(alignment: .topTrailing) {
                    if isFocused {
                        Image(systemName: "play.fill")
                            .font(.system(size: 17, weight: .bold))
                            .foregroundStyle(Color.teaCream)
                            .frame(width: 42, height: 42)
                            .background(Color.teaPanelElevated.opacity(0.94), in: Circle())
                            .overlay {
                                Circle()
                                    .stroke(Color.teaCream.opacity(0.18), lineWidth: 1)
                            }
                            .shadow(color: .black.opacity(0.20), radius: 8, y: 4)
                            .padding(14)
                            .transition(.scale.combined(with: .opacity))
                            .accessibilityHidden(true)
                    }
                }
                .shadow(
                    color: isFocused ? Color.teaAccent.opacity(0.18) : .black.opacity(0.35),
                    radius: isFocused ? 24 : 12,
                    y: isFocused ? 12 : 8
                )

            VStack(alignment: .leading, spacing: 2) {
                Text(movie.title)
                    .font(.headline.weight(.semibold))
                    .foregroundStyle(isFocused ? Color.teaAccentLight : Color.teaCream)
                    .lineLimit(1)
                HStack(spacing: 6) {
                    if let genre = movie.primaryGenre {
                        Text(genre)
                    }
                    if movie.primaryGenre != nil, movie.year != nil {
                        Text("•")
                    }
                    if let year = movie.year {
                        Text(String(year))
                    }
                }
                .font(.subheadline)
                .foregroundStyle(Color.teaMuted)
            }
            .frame(width: cardWidth, alignment: .leading)
        }
        .frame(width: cardWidth, alignment: .leading)
        .contentShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .scaleEffect(isFocused ? 1.028 : 1, anchor: .topLeading)
        .animation(.snappy(duration: 0.22), value: isFocused)
    }
}

struct PosterImage: View {
    let movie: Movie

    @State private var imageData: Data?
    @State private var isLoading = false

    var body: some View {
        Group {
            if let imageData, let image = UIImage(data: imageData) {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFill()
            } else if isLoading {
                ZStack {
                    Color.teaPanel
                    ProgressView()
                }
            } else {
                placeholder
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .task(id: movie) {
            await loadArtwork()
        }
    }

    private var placeholder: some View {
        ZStack {
            LinearGradient(
                colors: [Color.teaPanelElevated, Color.teaBackground],
                startPoint: .topLeading,
                endPoint: .bottomTrailing
            )
            Image(systemName: "film.stack")
                .font(.system(size: 54))
                .foregroundStyle(Color.teaAccentLight.opacity(0.38))
        }
    }

    private func loadArtwork() async {
        imageData = nil
        let urls = [movie.posterURL, movie.backdropURL].compactMap { $0 }
        guard !urls.isEmpty else { return }

        isLoading = true
        defer { isLoading = false }
        for url in urls {
            for attempt in 0..<2 {
                do {
                    var request = URLRequest(
                        url: url,
                        cachePolicy: attempt == 0 ? .returnCacheDataElseLoad : .reloadIgnoringLocalCacheData,
                        timeoutInterval: 20
                    )
                    request.setValue("image/avif,image/webp,image/*,*/*;q=0.8", forHTTPHeaderField: "Accept")
                    let (data, response) = try await URLSession.shared.data(for: request)
                    guard let response = response as? HTTPURLResponse,
                          (200..<300).contains(response.statusCode),
                          UIImage(data: data) != nil else {
                        continue
                    }
                    imageData = data
                    return
                } catch is CancellationError {
                    return
                } catch {
                    if attempt == 0 {
                        try? await Task.sleep(for: .milliseconds(350))
                    }
                }
            }
        }
    }
}

struct BackdropImage: View {
    let movie: Movie

    var body: some View {
        AsyncImage(url: movie.backdropURL) { phase in
            if case let .success(image) = phase {
                image
                    .resizable()
                    .scaledToFill()
            } else {
                LinearGradient(
                    colors: [Color.teaPanelElevated, Color.teaBackground],
                    startPoint: .topTrailing,
                    endPoint: .bottomLeading
                )
            }
        }
    }
}
