import Foundation

@MainActor
public final class RecommendationPoller {
    private var task: Task<Void, Never>?

    public init() {}

    deinit {
        task?.cancel()
    }

    public func cancel() {
        task?.cancel()
        task = nil
    }

    public func start(
        interval: Duration = .seconds(5),
        timeout: Duration = .seconds(120),
        fetch: @escaping @MainActor @Sendable () async throws -> Recommendations,
        receive: @escaping @MainActor @Sendable (Recommendations) -> Bool,
        onTimeout: @escaping @MainActor @Sendable () -> Void
    ) {
        cancel()

        task = Task {
            let clock = ContinuousClock()
            let deadline = clock.now.advanced(by: timeout)

            while !Task.isCancelled {
                do {
                    try await Task.sleep(for: interval)
                } catch {
                    return
                }

                guard !Task.isCancelled else { return }
                guard clock.now < deadline else {
                    onTimeout()
                    return
                }

                let attempt = await withTaskGroup(of: PollAttempt.self) { group in
                    group.addTask {
                        do {
                            return .response(try await fetch())
                        } catch {
                            return .failed
                        }
                    }
                    group.addTask {
                        do {
                            try await clock.sleep(until: deadline)
                            return .timedOut
                        } catch {
                            return .failed
                        }
                    }
                    let first = await group.next() ?? .failed
                    group.cancelAll()
                    return first
                }

                guard !Task.isCancelled else { return }
                switch attempt {
                case let .response(response):
                    guard receive(response) else { return }
                case .failed:
                    continue
                case .timedOut:
                    onTimeout()
                    return
                }
            }
        }
    }

    private enum PollAttempt: Sendable {
        case response(Recommendations)
        case failed
        case timedOut
    }
}
