# Blakeswap app icon

`AppIcon.png` was generated and edited with the built-in imagegen tool on September 5, 2026. The final artwork overlays an orange Bitcoin glyph on the swap arrows, on a full-bleed opaque charcoal background. It has no inset tile, transparent padding, or gray rim. macOS applies its icon mask; the app header uses the same packaged `AppIcon.icns` with a small rounded clip. The macOS build generates standard 1× and 2× resolutions using Apple's tools.

The full-bleed background follows [Apple's app icon guidance](https://developer.apple.com/design/human-interface-guidelines/app-icons/). The original prompt below is retained for provenance; the two subsequent edits replace its inset background and add the Bitcoin overlay.

## Original generation prompt

```text
Use case: logo-brand
Asset type: production macOS app icon for Blakeswap, a Bitcoin ↔ Bitcoin Blake2b atomic-swap app.
Primary request: Create one distinctive, beautifully restrained app icon, not a presentation or mockup.
Subject: Two bold opposing arrows interlocking into a compact rounded S-shaped swap symbol, suggesting an exchange between two independent chains. The upper arrow is warm Bitcoin orange and the lower arrow is the app's mint green. Use clear arrowheads and generous separation so the mark reads at 16–32 pixels.
Scene/backdrop: A dark charcoal rounded-square macOS icon tile, centered and filling about 88% of the square canvas. The area outside the tile must be genuinely transparent, including the corners.
Style/medium: Crisp precision graphic rendered as a polished raster asset. Subtle satin depth and restrained edge highlights; almost flat, with strong clean silhouette and balanced negative space.
Color palette: Existing Blakeswap UI colors: charcoal #161B22, mint #63E0B8, warm Bitcoin orange #F7931A.
Composition/framing: Straight-on, symmetrical visual balance, one centered icon on a square 1024×1024 canvas; icon silhouette fully visible. Mark occupies roughly two-thirds of the tile. No tilted perspective.
Constraints: No text, letters, numbers, currency glyphs, coins, chains of links, charts, extra symbols, borders, watermark, scenery, mockup devices, or decorative flourishes. Preserve real alpha transparency outside the rounded-square tile. Output exactly one icon.
```

## Full-bleed edit prompt

```text
Use case: precise-object-edit
Asset type: Blakeswap macOS app icon and native app header artwork.
Input image: edit target, the current orange and mint interlocking S-shaped two-arrow icon.
Primary request: Correct ONLY the icon framing and background. Preserve the existing arrow symbol exactly: same orange upper arrow pointing right, mint lower arrow pointing left, same shape, proportions, satin depth, lighting and colors. Keep the symbol large and centered.
Background: Replace the inset rounded dark tile and its gray beveled rim with a seamless flat charcoal #161B22 background covering the entire square canvas from edge to edge. FULL-BLEED OPAQUE SQUARE. Every corner and every edge must be solid charcoal with no transparent pixels. macOS itself will apply the outer rounded mask.
Constraints: No border, no gray rim, no outer shadow, no rounded square drawn inside the image, no transparent padding, no inset plate or frame. Keep the original symbol design unchanged. No text, extra symbols, presentation or mockup. Output one square production artwork image.
```

## Bitcoin overlay edit prompt

```text
Use case: precise-object-edit
Asset type: final Blakeswap app icon for macOS and app header.
Input image: edit target, the current full-bleed charcoal square with orange upper arrow and mint lower arrow interlocking into an S.
Primary request: Add the orange Bitcoin B symbol, exactly the recognizable Bitcoin currency glyph ₿ with its two vertical strokes, overlaid prominently on top of the center of the swap-arrow symbol. Make this Bitcoin glyph warm vivid Bitcoin orange (#F7931A), clearly legible over both arrows with a slim dark-charcoal outline/shadow for separation. The Bitcoin glyph is upright and about 34% of the canvas height. It sits in the foreground over the intersection of the two arrows. No circular coin or badge behind the glyph.
Preserve the existing swap arrows, directions, arrangement, colors, satin depth, large scale and centered framing. Keep the full-bleed opaque charcoal background extending completely to every edge and corner. No rounded inset tile, gray border, transparent padding, rim, extra symbols, other text or mockup. Only add the Bitcoin glyph overlay. Output one square production icon image.
```
