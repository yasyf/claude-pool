import SwiftUI

// The pool mascot: a pure-vector blob whose eyes, brows, mouth, sweat, and
// tint track the pool's alarm mood. All geometry lives in unit space scaled
// by `size`, so it renders crisply at any widget footprint with no assets.
// Widgets are static archived renders — no animation APIs work here — so
// every mood must read from a single frame, and the panic "shake" is a
// static tilt whose sign flips with each timeline refresh.

/// Geometry knobs for one mood, in unit space (fractions of `size`).
struct CritterSpec {
    let squash: CGFloat // body height fraction (lower = more cowering)
    let eyeRadius: CGFloat
    let pupilRadius: CGFloat
    let lid: CGFloat // fraction of the eye the relaxed eyelid covers
    let browAngle: Double // degrees; positive raises the inner ends (worry)
    let browLift: CGFloat // extra brow height above the eyes
    let mouth: Mouth
    let sweatDrops: Int
    let tilt: Double // degrees; panic wobble

    enum Mouth {
        case curve(CGFloat) // control-point dip: + smile, − frown, 0 flat
        case open // alarmed "o"
        case zigzag // panicked grit
    }
}

extension Mood {
    var spec: CritterSpec {
        switch self {
        case .chill:
            CritterSpec(squash: 0.92, eyeRadius: 0.085, pupilRadius: 0.045, lid: 0.35,
                        browAngle: -12, browLift: 0, mouth: .curve(0.10), sweatDrops: 0, tilt: 0)
        case .easy:
            CritterSpec(squash: 0.92, eyeRadius: 0.095, pupilRadius: 0.045, lid: 0.15,
                        browAngle: -6, browLift: 0.005, mouth: .curve(0.06), sweatDrops: 0, tilt: 0)
        case .uneasy:
            CritterSpec(squash: 0.90, eyeRadius: 0.105, pupilRadius: 0.042, lid: 0,
                        browAngle: 6, browLift: 0.010, mouth: .curve(0), sweatDrops: 0, tilt: 0)
        case .worried:
            CritterSpec(squash: 0.88, eyeRadius: 0.115, pupilRadius: 0.037, lid: 0,
                        browAngle: 14, browLift: 0.020, mouth: .curve(-0.07), sweatDrops: 0, tilt: 0)
        case .alarmed:
            CritterSpec(squash: 0.86, eyeRadius: 0.135, pupilRadius: 0.031, lid: 0,
                        browAngle: 22, browLift: 0.030, mouth: .open, sweatDrops: 1, tilt: 0)
        case .panic:
            CritterSpec(squash: 0.84, eyeRadius: 0.150, pupilRadius: 0.025, lid: 0,
                        browAngle: 30, browLift: 0.035, mouth: .zigzag, sweatDrops: 2, tilt: 3)
        }
    }
}

struct CritterView: View {
    let mood: Mood
    var size: CGFloat
    /// Timeline-entry date seeding the deterministic panic wobble: the tilt
    /// sign flips with minute parity, so panic sways slowly across refreshes.
    var seed: Date = .now

    private var spec: CritterSpec { mood.spec }
    private var rgb: RGB { mood.rgb }

    var body: some View {
        ZStack {
            bodyBlob
            tremor
            eye(at: 0.35)
            eye(at: 0.65)
            brow(at: 0.35, angle: -spec.browAngle)
            brow(at: 0.65, angle: spec.browAngle)
            mouth
            sweat
        }
        .frame(width: size, height: size)
        .rotationEffect(.degrees(tiltDegrees))
        .accessibilityLabel(Text("pool mood: \(mood.rawValue)"))
    }

    private var tiltDegrees: Double {
        guard spec.tilt != 0 else { return 0 }
        let flip = Calendar.current.component(.minute, from: seed) % 2 == 0
        return flip ? spec.tilt : -spec.tilt
    }

    // MARK: body

    private var bodyBlob: some View {
        Ellipse()
            .fill(RadialGradient(
                colors: [rgb.mixed(with: RGB(r: 1, g: 1, b: 1), 0.30).color, rgb.color],
                center: .init(x: 0.35, y: 0.25),
                startRadius: 0, endRadius: size * 0.85))
            .overlay(Ellipse().strokeBorder(
                rgb.mixed(with: RGB(r: 0, g: 0, b: 0), 0.18).color, lineWidth: size * 0.02))
            .frame(width: size, height: spec.squash * size)
            .position(x: 0.5 * size, y: (1 - spec.squash / 2) * size)
    }

    /// Faint flanking arcs that read as a static shake at panic.
    @ViewBuilder private var tremor: some View {
        if spec.tilt != 0 {
            ForEach([1.0, -1.0], id: \.self) { side in
                Path { p in
                    let x = (0.5 + side * 0.47) * size
                    p.move(to: CGPoint(x: x, y: 0.40 * size))
                    p.addQuadCurve(
                        to: CGPoint(x: x, y: 0.66 * size),
                        control: CGPoint(x: x + side * 0.10 * size, y: 0.53 * size))
                }
                .stroke(rgb.color.opacity(0.5),
                        style: StrokeStyle(lineWidth: size * 0.022, lineCap: .round))
            }
        }
    }

    // MARK: face

    private func eye(at x: CGFloat) -> some View {
        let r = spec.eyeRadius * size
        let lidColor = rgb.mixed(with: RGB(r: 1, g: 1, b: 1), 0.12).color
        return ZStack {
            Circle().fill(.white)
            Circle()
                .fill(.black)
                .frame(width: spec.pupilRadius * 2 * size)
                // Chill pupils sit low: half-asleep, not staring.
                .offset(y: spec.lid > 0.2 ? r * 0.15 : 0)
            if spec.lid > 0 {
                VStack(spacing: 0) {
                    Rectangle().fill(lidColor).frame(height: spec.lid * 2 * r)
                    Spacer(minLength: 0)
                }
            }
        }
        .frame(width: r * 2, height: r * 2)
        .clipShape(Circle())
        .position(x: x * size, y: 0.42 * size)
    }

    private func brow(at x: CGFloat, angle: Double) -> some View {
        Capsule()
            .fill(rgb.mixed(with: RGB(r: 0, g: 0, b: 0), 0.55).color)
            .frame(width: 0.17 * size, height: 0.032 * size)
            .rotationEffect(.degrees(angle))
            .position(x: x * size,
                      y: (0.42 - spec.eyeRadius - 0.06 - spec.browLift) * size)
    }

    @ViewBuilder private var mouth: some View {
        let dark = rgb.mixed(with: RGB(r: 0, g: 0, b: 0), 0.60).color
        switch spec.mouth {
        case .curve(let dip):
            Path { p in
                p.move(to: CGPoint(x: 0.35 * size, y: 0.66 * size))
                p.addQuadCurve(
                    to: CGPoint(x: 0.65 * size, y: 0.66 * size),
                    control: CGPoint(x: 0.5 * size, y: (0.66 + dip * 2) * size))
            }
            .stroke(dark, style: StrokeStyle(lineWidth: size * 0.028, lineCap: .round))
        case .open:
            Ellipse()
                .fill(dark)
                .frame(width: 0.12 * size, height: 0.16 * size)
                .position(x: 0.5 * size, y: 0.69 * size)
        case .zigzag:
            Path { p in
                let y = 0.67 * size
                let amp = 0.035 * size
                let left = 0.33 * size
                let step = (0.34 * size) / 4
                p.move(to: CGPoint(x: left, y: y))
                for i in 1 ... 4 {
                    let dy = i % 2 == 1 ? -amp : amp
                    p.addLine(to: CGPoint(x: left + CGFloat(i) * step, y: y + dy))
                }
            }
            .stroke(dark, style: StrokeStyle(lineWidth: size * 0.028,
                                             lineCap: .round, lineJoin: .round))
        }
    }

    @ViewBuilder private var sweat: some View {
        if spec.sweatDrops >= 1 {
            sweatDrop.position(x: 0.84 * size, y: 0.18 * size)
        }
        if spec.sweatDrops >= 2 {
            sweatDrop
                .scaleEffect(0.8)
                .position(x: 0.14 * size, y: 0.24 * size)
        }
    }

    private var sweatDrop: some View {
        Teardrop()
            .fill(LinearGradient(
                colors: [RGB(r: 0.55, g: 0.85, b: 0.98).color, Palette.blue.color],
                startPoint: .top, endPoint: .bottom))
            .frame(width: 0.10 * size, height: 0.13 * size)
    }
}

/// A droplet: pointed apex easing into a round belly.
struct Teardrop: Shape {
    func path(in rect: CGRect) -> Path {
        var p = Path()
        let w = rect.width
        let h = rect.height
        p.move(to: CGPoint(x: rect.midX, y: rect.minY))
        p.addCurve(to: CGPoint(x: rect.maxX, y: rect.minY + h * 0.65),
                   control1: CGPoint(x: rect.midX + w * 0.10, y: rect.minY + h * 0.25),
                   control2: CGPoint(x: rect.maxX, y: rect.minY + h * 0.40))
        p.addArc(center: CGPoint(x: rect.midX, y: rect.minY + h * 0.65),
                 radius: w / 2,
                 startAngle: .zero, endAngle: .degrees(180), clockwise: false)
        p.addCurve(to: CGPoint(x: rect.midX, y: rect.minY),
                   control1: CGPoint(x: rect.minX, y: rect.minY + h * 0.40),
                   control2: CGPoint(x: rect.midX - w * 0.10, y: rect.minY + h * 0.25))
        p.closeSubpath()
        return p
    }
}

// MARK: - Previews

#Preview("critter strip") {
    VStack(spacing: 16) {
        HStack(spacing: 16) {
            ForEach(Mood.allCases, id: \.self) { mood in
                VStack(spacing: 6) {
                    CritterView(mood: mood, size: 56)
                    Text(mood.rawValue).font(.caption2).foregroundStyle(.secondary)
                }
            }
        }
        HStack(spacing: 16) {
            CritterView(mood: .panic, size: 96)
            CritterView(mood: .chill, size: 96)
        }
    }
    .padding(24)
}
