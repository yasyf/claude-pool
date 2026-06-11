import SwiftUI
import WidgetKit

struct StatusWidgetView: View {
    @Environment(\.widgetFamily) private var family
    let entry: StatusEntry

    var body: some View {
        switch entry.state {
        case .noFile:
            MessageView(symbol: "moon.zzz", title: "daemon not running",
                        detail: "ccp service install")
        case .denied:
            MessageView(symbol: "lock.slash", title: "can't read ~/.cc-pool",
                        detail: "check widget entitlements")
        case .unreadable:
            MessageView(symbol: "exclamationmark.triangle", title: "status unreadable",
                        detail: "version skew? run ccp doctor")
        case .ok(let status, let stale):
            if status.accounts.isEmpty {
                MessageView(symbol: "person.crop.circle.badge.plus", title: "no accounts",
                            detail: "run ccp add")
            } else if family == .systemSmall {
                SmallView(status: status, stale: stale)
            } else {
                MediumView(status: status, stale: stale)
            }
        }
    }
}

// MARK: - Medium: all accounts, closest to `ccp status`

struct MediumView: View {
    let status: PoolStatus
    let stale: Bool

    private static let maxRows = 4

    var body: some View {
        let ranked = status.accounts.ranked
        VStack(alignment: .leading, spacing: 5) {
            ForEach(ranked.prefix(Self.maxRows)) { account in
                AccountRow(account: account)
            }
            Spacer(minLength: 0)
            FooterView(generatedAt: status.generatedAt, stale: stale,
                       overflow: max(0, ranked.count - Self.maxRows))
        }
        .opacity(stale ? 0.55 : 1)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

struct AccountRow: View {
    let account: AccountStatus

    var body: some View {
        HStack(spacing: 6) {
            LiveDot(count: account.activeSessions)
            Text(account.displayName)
                .font(.caption)
                .lineLimit(1)
                .truncationMode(.middle)
                .frame(maxWidth: .infinity, alignment: .leading)
            BadgeRow(account: account)
            if account.hasUsage {
                UsageBar(window: "5h", remaining: account.remaining5h, alert: account.unusable)
                UsageBar(window: "7d", remaining: account.remaining7d, alert: account.unusable)
            } else {
                Text("no data")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
    }
}

// MARK: - Small: the pool at a glance (CLI's ▸ next pick)

struct SmallView: View {
    let status: PoolStatus
    let stale: Bool

    var body: some View {
        let ranked = status.accounts.ranked
        let best = ranked[0]
        let liveTotal = status.accounts.reduce(0) { $0 + $1.activeSessions }
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 5) {
                LiveDot(count: liveTotal)
                Text(best.displayName)
                    .font(.caption.weight(.semibold))
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            if best.hasUsage {
                WideUsageBar(window: "5h", remaining: best.remaining5h, alert: best.unusable)
                WideUsageBar(window: "7d", remaining: best.remaining7d, alert: best.unusable)
            } else {
                Text("no data yet")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            HStack(spacing: 4) {
                BadgeRow(account: best)
                if liveTotal > 0 {
                    Text("\(liveTotal) live")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 0)
            FooterView(generatedAt: status.generatedAt, stale: stale, overflow: 0)
        }
        .opacity(stale ? 0.55 : 1)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

// MARK: - Pieces

/// Color by remaining headroom, matching the CLI's health tinting; an account
/// that cannot serve (rate-limited/exhausted) is always red.
func headroomColor(remaining: Double, alert: Bool) -> Color {
    if alert { return .red }
    if remaining > 40 { return .green }
    if remaining > 15 { return .yellow }
    return .red
}

extension AccountStatus {
    var unusable: Bool { rateLimited || isExhausted }
}

/// Compact "5h ▮▮▮░ 58%" cell for medium rows. The bar fills with % USED
/// (matching the `ccp status` columns); color reflects remaining headroom.
struct UsageBar: View {
    let window: String
    let remaining: Double
    let alert: Bool

    var body: some View {
        let clamped = min(max(remaining, 0), 100)
        HStack(spacing: 3) {
            Text(window)
                .font(.system(size: 8))
                .foregroundStyle(.tertiary)
            ProgressView(value: 100 - clamped, total: 100)
                .progressViewStyle(.linear)
                .tint(headroomColor(remaining: clamped, alert: alert))
                .frame(width: 34)
            Text("\(Int((100 - clamped).rounded()))%")
                .font(.system(size: 9, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 28, alignment: .trailing)
        }
    }
}

/// Full-width bar for the small widget.
struct WideUsageBar: View {
    let window: String
    let remaining: Double
    let alert: Bool

    var body: some View {
        let clamped = min(max(remaining, 0), 100)
        HStack(spacing: 4) {
            Text(window)
                .font(.system(size: 9))
                .foregroundStyle(.tertiary)
                .frame(width: 14, alignment: .leading)
            ProgressView(value: 100 - clamped, total: 100)
                .progressViewStyle(.linear)
                .tint(headroomColor(remaining: clamped, alert: alert))
            Text("\(Int((100 - clamped).rounded()))%")
                .font(.system(size: 10, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 32, alignment: .trailing)
        }
    }
}

struct LiveDot: View {
    let count: Int

    var body: some View {
        HStack(spacing: 2) {
            Circle()
                .fill(count > 0 ? Color.green : Color.secondary.opacity(0.35))
                .frame(width: 6, height: 6)
            if count > 0 {
                Text("\(count)")
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// The CLI's stale / rate-limited / exhausted / overage flags as compact glyphs.
struct BadgeRow: View {
    let account: AccountStatus

    var body: some View {
        HStack(spacing: 3) {
            if account.stale {
                Image(systemName: "clock.badge.exclamationmark")
                    .font(.system(size: 8))
                    .foregroundStyle(.orange)
            }
            if account.rateLimited {
                Image(systemName: "exclamationmark.octagon.fill")
                    .font(.system(size: 8))
                    .foregroundStyle(.red)
            }
            if account.isExhausted {
                Image(systemName: "hourglass.bottomhalf.filled")
                    .font(.system(size: 8))
                    .foregroundStyle(.red)
            }
            if account.hasOverage {
                Text(String(format: "$%.2f", (account.extraUsed ?? 0) / 100))
                    .font(.system(size: 8, design: .monospaced))
                    .foregroundStyle(.orange)
            }
            // When the account can't serve, the time it recovers is the one
            // fact that matters — the CLI's RESETS column, shown on demand.
            if account.unusable, let reset = account.resets5h?.nonZero {
                Text(reset, format: .dateTime.hour().minute())
                    .font(.system(size: 8))
                    .foregroundStyle(.secondary)
            }
        }
    }
}

struct FooterView: View {
    let generatedAt: Date
    let stale: Bool
    let overflow: Int

    var body: some View {
        HStack(spacing: 4) {
            if overflow > 0 {
                Text("+\(overflow) more")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            Spacer(minLength: 0)
            if stale {
                Text("stale · ")
                    .font(.caption2)
                    .foregroundStyle(.orange)
                + Text(generatedAt, style: .relative)
                    .font(.caption2)
                    .foregroundStyle(.orange)
                + Text(" ago")
                    .font(.caption2)
                    .foregroundStyle(.orange)
            } else {
                Text("updated ")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                + Text(generatedAt, style: .relative)
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                + Text(" ago")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
    }
}

struct MessageView: View {
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 4) {
            Image(systemName: symbol)
                .font(.title3)
                .foregroundStyle(.secondary)
            Text(title)
                .font(.caption.weight(.semibold))
            Text(detail)
                .font(.caption2.monospaced())
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// MARK: - Previews

#Preview("medium", as: .systemMedium) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .ok(.sample, stale: true))
    StatusEntry(date: .now, state: .noFile)
}

#Preview("small", as: .systemSmall) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .unreadable)
}
