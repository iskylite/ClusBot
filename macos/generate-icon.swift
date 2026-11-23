import AppKit
import Foundation

guard CommandLine.arguments.count == 3,
      let brand = NSImage(contentsOfFile: CommandLine.arguments[1]) else {
    fatalError("Usage: swift generate-icon.swift <brand.svg> <output.iconset>")
}
let output = URL(fileURLWithPath: CommandLine.arguments[2], isDirectory: true)
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)

// An 824-point mark within the 1024-point icon canvas leaves the standard
// macOS icon margin while keeping the original SVG as the sole artwork.
for points in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let pixels = points * scale
        guard let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
                                            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                                            isPlanar: false, colorSpaceName: .deviceRGB,
                                            bytesPerRow: 0, bitsPerPixel: 0),
              let context = NSGraphicsContext(bitmapImageRep: bitmap) else {
            fatalError("Unable to create icon bitmap")
        }
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = context
        context.imageInterpolation = .high
        let inset = CGFloat(pixels) * 100 / 1024
        let size = CGFloat(pixels) - 2 * inset
        // Fill the source's white rounded tile underneath the SVG; this also
        // avoids a small transparency seam in AppKit's SVG path rasterizer.
        let tileInset = size * 0.5 / 29
        NSColor.white.setFill()
        NSBezierPath(roundedRect: NSRect(x: inset + tileInset, y: inset + tileInset,
                                         width: size - 2 * tileInset, height: size - 2 * tileInset),
                     xRadius: size * 7 / 29, yRadius: size * 7 / 29).fill()
        brand.draw(in: NSRect(x: inset, y: inset, width: size, height: size),
                   from: .zero, operation: .sourceOver, fraction: 1)
        context.flushGraphics()
        NSGraphicsContext.restoreGraphicsState()
        guard let png = bitmap.representation(using: .png, properties: [:]) else {
            fatalError("Unable to encode icon bitmap")
        }
        let suffix = scale == 2 ? "@2x" : ""
        try png.write(to: output.appendingPathComponent("icon_\(points)x\(points)\(suffix).png"))
    }
}
