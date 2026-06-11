import Foundation
import WidgetKit

struct StatusEntry: TimelineEntry {
    let date: Date
    let state: State

    enum State {
        case ok(PoolStatus, stale: Bool) // stale: generated_at is past staleAfter
        case noFile // daemon not running / never ran
        case denied // read refused — sandbox/entitlement problem, not the daemon
        case unreadable // decode failure or proto skew — surface it, never guess
    }
}

struct StatusProvider: TimelineProvider {
    /// The daemon polls every 180s + up to 30s jitter and stamps generated_at
    /// per completed poll; two missed cycles (~7 min) means it's down or wedged.
    static let staleAfter: TimeInterval = 7 * 60
    static let supportedProto = 1

    func placeholder(in _: Context) -> StatusEntry {
        StatusEntry(date: .now, state: .ok(.sample, stale: false))
    }

    func getSnapshot(in context: Context, completion: @escaping (StatusEntry) -> Void) {
        completion(context.isPreview ? placeholder(in: context) : load(at: .now))
    }

    func getTimeline(in _: Context, completion: @escaping (Timeline<StatusEntry>) -> Void) {
        let now = Date()
        // One entry, staleness judged at build time. Deliberately NO
        // future-dated pre-dimmed entry: reloads are budget-throttled
        // (reloadAllTimelines included), so a synthetic stale flip at
        // generated_at+staleAfter false-positives between throttled reloads —
        // the widget spent most of its life dimmed over a perfectly fresh
        // file. Stale dimming is therefore best-effort (next rebuild); the
        // self-updating relative "updated … ago" footer carries true age.
        let entry = load(at: now)
        completion(Timeline(entries: [entry], policy: .after(now.addingTimeInterval(5 * 60))))
    }

    private func load(at now: Date) -> StatusEntry {
        let data: Data
        do {
            data = try Data(contentsOf: StatusFile.url)
        } catch let err as CocoaError where err.code == .fileReadNoSuchFile {
            return StatusEntry(date: now, state: .noFile)
        } catch {
            // A sandbox denial (EPERM after an entitlement-stripping re-sign)
            // must not masquerade as "daemon not running" — that misdiagnosis
            // sends the user off to reinstall a perfectly healthy daemon.
            return StatusEntry(date: now, state: .denied)
        }
        // No last-good cache on failure: the daemon's atomic rename makes
        // partial reads impossible, so a persistent decode failure means
        // schema/proto skew the user should see, not paper over.
        guard let status = try? JSONDecoder.poolStatus.decode(PoolStatus.self, from: data),
              status.proto == Self.supportedProto
        else {
            return StatusEntry(date: now, state: .unreadable)
        }
        let stale = now.timeIntervalSince(status.generatedAt) > Self.staleAfter
        return StatusEntry(date: now, state: .ok(status, stale: stale))
    }
}
