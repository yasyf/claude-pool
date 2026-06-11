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
            } else {
                switch family {
                case .systemSmall:
                    SmallView(status: status, stale: stale)
                case .systemLarge:
                    AccountListView(status: status, stale: stale, maxRows: 6, style: .detailed)
                default:
                    AccountListView(status: status, stale: stale, maxRows: 4, style: .compact)
                }
            }
        }
    }
}

// MARK: - Medium & large: ranked account list, closest to `ccp status`

/// Row density per family: medium rows are name + bars; large rows add the
/// resets/overage detail line.
enum RowStyle { case compact, detailed }

/// Ranked account list filling the widget. Every row gets an equal flexible
/// slot (maxHeight .infinity) so surplus height spreads across rows instead
/// of pooling above the footer; with fewer accounts each row simply breathes.
struct AccountListView: View {
    let status: PoolStatus
    let stale: Bool
    let maxRows: Int
    let style: RowStyle

    var body: some View {
        let ranked = status.accounts.ranked
        VStack(alignment: .leading, spacing: 0) {
            ForEach(ranked.prefix(maxRows)) { account in
                AccountRow(account: account, style: style)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
            }
            FooterView(generatedAt: status.generatedAt, stale: stale,
                       overflow: max(0, ranked.count - maxRows))
                .padding(.top, 2)
        }
        .opacity(stale ? 0.55 : 1)
    }
}

struct AccountRow: View {
    let account: AccountStatus
    let style: RowStyle

    /// Lines below the name align past the live dot: dot (7) + spacing (5).
    private static let indent: CGFloat = 12

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 5) {
                LiveDot(count: account.activeSessions)
                Text(account.displayName)
                    .font(.system(size: 12, weight: .medium))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 4)
                BadgeRow(account: account, style: style)
                    .layoutPriority(1) // badges hug; the name truncates first
            }
            Group {
                if account.hasUsage {
                    HStack(spacing: 10) {
                        UsageCell(window: "5h", remaining: account.remaining5h, alert: account.unusable)
                        UsageCell(window: "7d", remaining: account.remaining7d, alert: account.unusable)
                    }
                } else {
                    Text("no data")
                        .font(.system(size: 10))
                        .foregroundStyle(.tertiary)
                }
            }
            .padding(.leading, Self.indent)
            if style == .detailed, let detail {
                Text(detail)
                    .font(.system(size: 10))
                    .monospacedDigit()
                    .foregroundStyle(.tertiary)
                    .lineLimit(1)
                    .padding(.leading, Self.indent)
            }
        }
    }

    /// Detail line: the CLI's RESETS column for both windows plus overage
    /// spend — shown for every account, not only unusable ones.
    private var detail: String? {
        var parts: [String] = []
        if let reset = account.resets5h?.nonZero {
            parts.append("5h resets " + reset.formatted(date: .omitted, time: .shortened))
        }
        if let reset = account.resets7d?.nonZero {
            parts.append("7d resets " + reset.formatted(.dateTime.weekday(.abbreviated).hour().minute()))
        }
        if account.hasOverage {
            let used = (account.extraUsed ?? 0) / 100
            if let limit = account.extraLimit, limit > 0 {
                parts.append(String(format: "extra $%.2f of $%.2f", used, limit / 100))
            } else {
                parts.append(String(format: "extra $%.2f", used))
            }
        }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
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
                    .font(.system(size: 12, weight: .semibold))
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            if best.hasUsage {
                UsageCell(window: "5h", remaining: best.remaining5h, alert: best.unusable)
                UsageCell(window: "7d", remaining: best.remaining7d, alert: best.unusable)
            } else {
                Text("no data yet")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            HStack(spacing: 4) {
                BadgeRow(account: best, style: .compact)
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

/// Capsule gauge. The bar fills with % USED (matching the `ccp status`
/// columns); color reflects remaining headroom.
struct HeadroomBar: View {
    let remaining: Double // clamped to 0...100 by the caller
    let alert: Bool

    private static let height: CGFloat = 6

    var body: some View {
        let usedFraction = (100 - remaining) / 100
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(.quaternary)
                if usedFraction > 0 {
                    // Floor at the capsule's own diameter so a barely-used
                    // account renders a full dot, not a degenerate sliver.
                    Capsule()
                        .fill(headroomColor(remaining: remaining, alert: alert))
                        .frame(width: max(Self.height, geo.size.width * usedFraction))
                }
            }
        }
        .frame(height: Self.height)
    }
}

/// "5h ▮▮▮░ 58%" cell: fixed label and percent columns around a flexible
/// bar, so bars align across rows and split leftover width with siblings.
struct UsageCell: View {
    let window: String
    let remaining: Double
    let alert: Bool

    var body: some View {
        let clamped = min(max(remaining, 0), 100)
        HStack(spacing: 4) {
            Text(window)
                .font(.system(size: 10))
                .foregroundStyle(.tertiary)
                .frame(width: 16, alignment: .leading)
            HeadroomBar(remaining: clamped, alert: alert)
            Text("\(Int((100 - clamped).rounded()))%")
                .font(.system(size: 10, weight: .medium, design: .monospaced))
                .foregroundStyle(.secondary)
                .frame(width: 30, alignment: .trailing)
        }
        .frame(maxWidth: .infinity)
    }
}

struct LiveDot: View {
    let count: Int

    var body: some View {
        HStack(spacing: 2) {
            Circle()
                .fill(count > 0 ? Color.green : Color.secondary.opacity(0.35))
                .frame(width: 7, height: 7)
            if count > 0 {
                Text("\(count)")
                    .font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(.secondary)
            }
        }
    }
}

/// The CLI's stale / rate-limited / exhausted / overage flags as compact glyphs.
struct BadgeRow: View {
    let account: AccountStatus
    let style: RowStyle

    var body: some View {
        HStack(spacing: 3) {
            if account.stale {
                Image(systemName: "clock.badge.exclamationmark")
                    .font(.system(size: 10))
                    .foregroundStyle(.orange)
            }
            if account.rateLimited {
                Image(systemName: "exclamationmark.octagon.fill")
                    .font(.system(size: 10))
                    .foregroundStyle(.red)
            }
            if account.isExhausted {
                Image(systemName: "hourglass.bottomhalf.filled")
                    .font(.system(size: 10))
                    .foregroundStyle(.red)
            }
            // Detailed rows carry overage and reset times on their own
            // detail line; repeating them here would double-print.
            if style == .compact {
                if account.hasOverage {
                    Text(String(format: "$%.2f", (account.extraUsed ?? 0) / 100))
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundStyle(.orange)
                }
                // When the account can't serve, the time it recovers is the one
                // fact that matters — the CLI's RESETS column, shown on demand.
                if account.unusable, let reset = account.resets5h?.nonZero {
                    Text(reset, format: .dateTime.hour().minute())
                        .font(.system(size: 10))
                        .foregroundStyle(.secondary)
                }
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

#Preview("large", as: .systemLarge) {
    CCPoolStatusWidget()
} timeline: {
    StatusEntry(date: .now, state: .ok(.sample, stale: false))
    StatusEntry(date: .now, state: .ok(.sample, stale: true))
}
