
# Badge Generator

A high-performance Go application that generates custom badges by combining icons and colors into PNG images.

## Features

- **Parallel Processing**: Utilizes all available CPU cores for rendering and encoding
- **Memory Efficient**: Object pooling for RGBA buffers and palette-based PNG compression
- **Batch Generation**: Creates all valid combinations of icons and colors
- **Single Mode**: Generate individual badges on demand

## Usage

### Batch Mode
```bash
go run main.go
```
Generates all badge combinations and saves them to the `images/` directory.

### Single Mode
```bash
go run main.go 0_1_0_11
```
Generates a single badge with indices: symbol=0, border=1, color1=0, color2=11

Output: `badge.png`

## Assets Structure

```
assets/
├── csv/
│   ├── badge_colors.csv    # Color definitions (RGB + layer)
│   └── badge_icons.csv     # Icon definitions (file paths + layer)
└── badges/                 # PNG image files
```

## Performance

- Renders at 300px resolution
- Uses object pooling to reduce GC pressure
- Skips existing files to avoid redundant work
- Progress tracking every 100 renders
## Generated Badges

View pre-generated badges at:
```
https://raw.githubusercontent.com/Rider21/badge/refs/heads/data/images/{symbol}-{border}-{color1}-{color2}.png
```

Example: [0-1-0-11.png](https://raw.githubusercontent.com/Rider21/badge/refs/heads/data/images/0-1-0-11.png)

The URL is constructed from badge indices:
- `symbol`: Icon index
- `border`: Border style index
- `color1`: Primary color index
- `color2`: Secondary color index

This application generates all valid corporation icon combinations from Hades Star, producing complete badge assets for the game.
