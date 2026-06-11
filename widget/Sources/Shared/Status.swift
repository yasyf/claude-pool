import Foundation

// Models for ~/.cc-pool/status.json, the daemon's atomic status mirror.
// The wire contract (exact keys, omitempty fields, zero-time sentinel) is
// pinned by TestStatusSnapshotJSONKeys in internal/daemon/snapshot_test.go.

struct PoolStatus: Decodable {
    let proto: Int
    let version: String
    let generatedAt: Date
    let accounts: [AccountStatus]

    enum CodingKeys: String, CodingKey {
        case proto, version, accounts
        case generatedAt = "generated_at"
    }
}

struct AccountStatus: Decodable, Identifiable {
    let id: Int
    let configDir: String
    let label: String
    let score: Double
    let remaining5h: Double
    let remaining7d: Double
    let activeSessions: Int
    let rateLimited: Bool
    let exhausted: Bool? // omitempty in Go
    let hasUsage: Bool
    let stale: Bool
    let resets5h: Date?
    let resets7d: Date?
    let extraEnabled: Bool? // omitempty
    let extraUsed: Double? // omitempty, currency cents
    let extraLimit: Double? // omitempty, currency cents

    // Explicit keys, not .convertFromSnakeCase: digit-leading components like
    // remaining_5h convert ambiguously. Keys the widget ignores (sample_age,
    // overlay_kind, the PascalCase components breakdown) are simply undeclared.
    enum CodingKeys: String, CodingKey {
        case id, label, score, stale, exhausted
        case configDir = "config_dir"
        case remaining5h = "remaining_5h"
        case remaining7d = "remaining_7d"
        case activeSessions = "active_sessions"
        case rateLimited = "rate_limited"
        case hasUsage = "has_usage"
        case resets5h = "resets_5h"
        case resets7d = "resets_7d"
        case extraEnabled = "extra_enabled"
        case extraUsed = "extra_used"
        case extraLimit = "extra_limit"
    }

    var isExhausted: Bool { exhausted ?? false }
    var hasOverage: Bool { (extraEnabled ?? false) && (extraUsed ?? 0) > 0 }

    /// Display label; empty labels fall back to the acct-NN dir basename.
    var displayName: String {
        label.isEmpty ? (configDir as NSString).lastPathComponent : label
    }

    /// Mirrors snapshotTier in internal/cli/status.go: status must never rank
    /// an unusable account above a usable one, however high its score.
    var tier: Int {
        if !rateLimited && !isExhausted { return 0 }
        if !rateLimited { return 1 }
        return 2
    }
}

extension [AccountStatus] {
    /// Display order: usability tier asc, then score desc — the same order
    /// `ccp status` uses, so "first" here matches the CLI's ▸ next pick.
    var ranked: [AccountStatus] {
        sorted { ($0.tier, -$0.score) < ($1.tier, -$1.score) }
    }
}

extension JSONDecoder {
    /// Decoder for status.json. Go's time.Time marshals RFC3339Nano, which
    /// omits fractional seconds when ns == 0 — while ISO8601DateFormatter
    /// requires fractions with .withFractionalSeconds and rejects them
    /// without. So try both. (generated_at is whole-second by construction;
    /// resets_* round-trip sqlite at whole-second precision; the dual parse
    /// keeps the widget safe if that ever changes.)
    static let poolStatus: JSONDecoder = {
        let frac = ISO8601DateFormatter()
        frac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            guard let t = plain.date(from: s) ?? frac.date(from: s) else {
                throw DecodingError.dataCorrupted(.init(
                    codingPath: decoder.codingPath,
                    debugDescription: "unparseable RFC3339 timestamp: \(s)"))
            }
            return t
        }
        return d
    }()
}

extension Date {
    /// Go's zero time (0001-01-01) means "no active window"; anything before
    /// 2000 reads as nil.
    var nonZero: Date? { timeIntervalSince1970 > 946_684_800 ? self : nil }
}

enum StatusFile {
    /// The real user home. Inside the sandboxed appex, NSHomeDirectory()
    /// points at the container (~/Library/Containers/…/Data); the sandbox
    /// exception is for the real ~/.cc-pool, so resolve home via passwd.
    static var realHome: String {
        if let pw = getpwuid(getuid()), let dir = pw.pointee.pw_dir {
            return String(cString: dir)
        }
        return NSHomeDirectory()
    }

    static var url: URL {
        URL(fileURLWithPath: realHome).appendingPathComponent(".cc-pool/status.json")
    }
}

// MARK: - Preview / gallery fixture

extension PoolStatus {
    /// Hardcoded fixture for the widget-gallery snapshot and Xcode previews.
    /// Six accounts so the medium preview overflows into "+2 more" and the
    /// large preview shows a full board: overage, stale, no-data, and
    /// unusable rows all at once.
    static let sample = PoolStatus(
        proto: 1,
        version: "dev",
        generatedAt: Date().addingTimeInterval(-90),
        accounts: [
            AccountStatus(
                id: 1, configDir: "/Users/you/.cc-pool/accounts/acct-01",
                label: "work@example.com", score: 88.4,
                remaining5h: 58, remaining7d: 91, activeSessions: 4,
                rateLimited: false, exhausted: nil, hasUsage: true, stale: false,
                resets5h: Date().addingTimeInterval(3 * 3600),
                resets7d: Date().addingTimeInterval(4 * 86400),
                extraEnabled: nil, extraUsed: nil, extraLimit: nil),
            AccountStatus(
                id: 2, configDir: "/Users/you/.cc-pool/accounts/acct-02",
                label: "rebecca.fallon.engineering@example-corp.com", score: 64.0,
                remaining5h: 60, remaining7d: 41, activeSessions: 2,
                rateLimited: false, exhausted: nil, hasUsage: true, stale: false,
                resets5h: Date().addingTimeInterval(2 * 3600),
                resets7d: Date().addingTimeInterval(3 * 86400),
                extraEnabled: nil, extraUsed: nil, extraLimit: nil),
            AccountStatus(
                id: 3, configDir: "/Users/you/.cc-pool/accounts/acct-03",
                label: "personal@example.com", score: 41.0,
                remaining5h: 22, remaining7d: 58, activeSessions: 0,
                rateLimited: false, exhausted: nil, hasUsage: true, stale: false,
                resets5h: Date().addingTimeInterval(90 * 60),
                resets7d: Date().addingTimeInterval(2 * 86400),
                extraEnabled: true, extraUsed: 5073, extraLimit: 10000),
            AccountStatus(
                id: 4, configDir: "/Users/you/.cc-pool/accounts/acct-04",
                label: "side@example.com", score: 18.0,
                remaining5h: 12, remaining7d: 35, activeSessions: 0,
                rateLimited: false, exhausted: nil, hasUsage: true, stale: true,
                resets5h: Date().addingTimeInterval(3600),
                resets7d: Date().addingTimeInterval(5 * 86400),
                extraEnabled: nil, extraUsed: nil, extraLimit: nil),
            AccountStatus(
                id: 5, configDir: "/Users/you/.cc-pool/accounts/acct-05",
                label: "fresh@example.com", score: 0.0,
                remaining5h: 0, remaining7d: 0, activeSessions: 0,
                rateLimited: false, exhausted: nil, hasUsage: false, stale: false,
                resets5h: nil, resets7d: nil,
                extraEnabled: nil, extraUsed: nil, extraLimit: nil),
            AccountStatus(
                id: 6, configDir: "/Users/you/.cc-pool/accounts/acct-06",
                label: "", score: -40.2,
                remaining5h: 1, remaining7d: 12, activeSessions: 0,
                rateLimited: true, exhausted: true, hasUsage: true, stale: false,
                resets5h: Date().addingTimeInterval(40 * 60),
                resets7d: Date().addingTimeInterval(86400),
                extraEnabled: true, extraUsed: 177, extraLimit: 5000),
        ])
}
