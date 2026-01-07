package main

import (
	"embed"
	"encoding/csv"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	xdraw "golang.org/x/image/draw"
)

//go:embed assets/*
var assetsFS embed.FS

const (
	OutputPath = "images"
	OutputSize = 300
)

// Data structures
type BadgeColorData struct{ R, G, B int }
type BadgeIconData struct {
	Layer           int
	OriginalIcon    *image.RGBA
	OriginalOutline *image.RGBA
}

type RenderJob struct {
	Filename string
	Palette  color.Palette
	Symbol   *image.RGBA
	Border   *image.RGBA
	Outline  *image.RGBA
}

var (
	badgeColors        []BadgeColorData
	badgeIcons         []BadgeIconData
	layer0ColorIndices []int
	layer1ColorIndices []int

	// Single pool for all RGBA buffers (used for dst and scratch)
	rgbaPool    sync.Pool
	palettePool sync.Pool

	fileCaseMap   map[string]string
	existingFiles map[string]bool // Cache of existing files
)

func init() {
	rgbaPool = sync.Pool{
		New: func() interface{} {
			return image.NewRGBA(image.Rect(0, 0, OutputSize, OutputSize))
		},
	}
	palettePool = sync.Pool{
		New: func() interface{} {
			return image.NewPaletted(image.Rect(0, 0, OutputSize, OutputSize), nil)
		},
	}
}

func main() {
	fmt.Println("🚀 Starting build")
	buildFileCaseMap()

	if err := loadIcons(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}

	// Single-run mode
	if len(os.Args) > 1 {
		runSingleMode(os.Args[1])
		return
	}

	_ = os.MkdirAll(OutputPath, 0755)

	scanOutputDirectory()

	total := calculateTotal()
	fmt.Printf("✅ Resources loaded. Total combinations: %d\n", total)
	if total == 0 {
		return
	}

	numWorkers := runtime.NumCPU()
	jobs := make(chan RenderJob, numWorkers*2)
	var processedCount int64
	var wg sync.WaitGroup

	for range numWorkers {
		wg.Go(func() {
			for job := range jobs {
				processJob(job)
				curr := atomic.AddInt64(&processedCount, 1)
				if curr%100 == 0 || curr == int64(total) {
					printProgress(curr, int64(total))
				}
			}
		})
	}

	// Job generator
	go func() {
		for _, c1Idx := range layer0ColorIndices {
			c1 := getColor(c1Idx)

			for _, c2Idx := range layer1ColorIndices {
				c2 := getColor(c2Idx)

				// Create a shared palette once for this combination of colors
				sharedPalette := color.Palette{
					color.RGBA{0, 0, 0, 0}, // [0] Transparent
					c1,                     // [1] Color for Symbol/Outline
					c2,                     // [2] Color for Border Base
				}

				// Iterate over icons
				for sIdx, symbol := range badgeIcons {
					if symbol.Layer != 0 {
						continue
					}
					for bIdx, border := range badgeIcons {
						if border.Layer != 1 {
							continue
						}
						fName := fmt.Sprintf("%d-%d-%d-%d.png", sIdx, bIdx, c1Idx, c2Idx)

						// Check map (avoid os.Stat calls)
						if existingFiles[fName] {
							atomic.AddInt64(&processedCount, 1)
							continue
						}

						jobs <- RenderJob{
							Filename: fName,
							Palette:  sharedPalette,
							Symbol:   symbol.OriginalIcon,
							Border:   border.OriginalIcon,
							Outline:  border.OriginalOutline,
						}
					}
				}
			}
		}
		close(jobs)
	}()

	wg.Wait()
	fmt.Printf("\n✨ Generation complete! Saved to: %s\n", OutputPath)
}

func processJob(j RenderJob) {
	// Get two buffers from the pool: one for the final image, one for the mask (scratch)
	dst := rgbaPool.Get().(*image.RGBA)
	scratch := rgbaPool.Get().(*image.RGBA)

	defer rgbaPool.Put(dst)
	defer rgbaPool.Put(scratch)

	// Clear dst
	draw.Draw(dst, dst.Bounds(), image.Transparent, image.Point{}, draw.Src)

	// Colors are taken from the palette (Palette[1]=C1, Palette[2]=C2)
	c1 := j.Palette[1]
	c2 := j.Palette[2]

	// Layer 1: Border Base (Scale 1.0, Tint C2)
	if j.Border != nil {
		drawLayer(dst, scratch, j.Border, c2, 1.0)
	}
	// Layer 2: Symbol (Scale 0.7, Tint C1)
	if j.Symbol != nil {
		drawLayer(dst, scratch, j.Symbol, c1, 0.7)
	}
	// Layer 3: Outline (Scale 1.0, Tint C1)
	if j.Outline != nil {
		drawLayer(dst, scratch, j.Outline, c1, 1.0)
	}

	savePNG(j.Filename, dst, j.Palette)
}

func drawLayer(dst, scratch, src *image.RGBA, tint color.Color, scale float64) {
	sb := src.Bounds()
	w, h := int(float64(sb.Dx())*scale), int(float64(sb.Dy())*scale)
	if w <= 0 || h <= 0 {
		return
	}

	ox, oy := (OutputSize-w)/2, (OutputSize-h)/2
	rect := image.Rect(ox, oy, ox+w, oy+h)

	// Clear scratch (important because it comes from the pool)
	draw.Draw(scratch, rect, image.Transparent, image.Point{}, draw.Src)

	// Scale: source -> scratch
	xdraw.NearestNeighbor.Scale(scratch, rect, src, sb, xdraw.Over, nil)

	// Apply tint: draw tint onto dst using scratch as mask
	draw.DrawMask(dst, rect, image.NewUniform(tint), image.Point{}, scratch, rect.Min, draw.Over)
}

func savePNG(filename string, img *image.RGBA, pal color.Palette) {
	palImg := palettePool.Get().(*image.Paletted)
	defer palettePool.Put(palImg)

	palImg.Palette = pal
	draw.Draw(palImg, palImg.Bounds(), img, image.Point{}, draw.Src)

	// Determine output path. In single-run mode we may use "badge.png" as a simple filename.
	fullPath := filepath.Join(OutputPath, filename)
	if filename == "badge.png" {
		fullPath = "badge.png"
	}

	f, err := os.Create(fullPath)
	if err != nil {
		return
	}
	defer f.Close()

	enc := png.Encoder{CompressionLevel: png.BestCompression}
	_ = enc.Encode(f, palImg)
}

// --- Helpers ---

func scanOutputDirectory() {
	existingFiles = make(map[string]bool)
	entries, err := os.ReadDir(OutputPath)
	if err != nil {
		return // Directory may not exist; that's OK
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			existingFiles[e.Name()] = true
		}
	}
}

func runSingleMode(arg string) {
	parts := strings.Split(arg, "_")
	if len(parts) != 4 {
		fmt.Println("Usage: program.exe SIdx_BIdx_C1Idx_C2Idx")
		return
	}
	s, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	c1Idx, _ := strconv.Atoi(parts[2])
	c2Idx, _ := strconv.Atoi(parts[3])

	c1 := getColor(c1Idx)
	c2 := getColor(c2Idx)

	pal := color.Palette{color.RGBA{0, 0, 0, 0}, c1, c2}

	// Find requested icons
	var sym, border *BadgeIconData
	// Simple lookup (could be optimized; not critical for single run)
	if s < len(badgeIcons) {
		sym = &badgeIcons[s]
	}
	if b < len(badgeIcons) {
		border = &badgeIcons[b]
	}

	if sym == nil || border == nil {
		fmt.Println("Invalid icon indices")
		return
	}

	job := RenderJob{
		Filename: "badge.png",
		Palette:  pal,
		Symbol:   sym.OriginalIcon,
		Border:   border.OriginalIcon,
		Outline:  border.OriginalOutline,
	}

	// Run directly
	processJob(job)
	fmt.Println("✅ Generated badge.png")
}

func buildFileCaseMap() {
	fileCaseMap = make(map[string]string)
	_ = fs.WalkDir(assetsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fileCaseMap[strings.ToLower(filepath.Base(path))] = path
		}
		return nil
	})
}

func loadIcons() error {
	f1, err := assetsFS.Open("assets/csv/badge_colors.csv")
	if err != nil {
		return err
	}
	defer f1.Close()

	r1, _ := csv.NewReader(f1).ReadAll()
	for i := 1; i < len(r1); i++ {
		r, _ := strconv.Atoi(r1[i][1])
		g, _ := strconv.Atoi(r1[i][2])
		b, _ := strconv.Atoi(r1[i][3])
		l, _ := strconv.Atoi(r1[i][4])
		badgeColors = append(badgeColors, BadgeColorData{r, g, b})
		if l == 0 {
			layer0ColorIndices = append(layer0ColorIndices, i-1)
		} else {
			layer1ColorIndices = append(layer1ColorIndices, i-1)
		}
	}

	f2, err := assetsFS.Open("assets/csv/badge_icons.csv")
	if err != nil {
		return err
	}
	defer f2.Close()

	r2, _ := csv.NewReader(f2).ReadAll()
	for i := 1; i < len(r2); i++ {
		l, _ := strconv.Atoi(r2[i][3])
		badgeIcons = append(badgeIcons, BadgeIconData{
			Layer:           l,
			OriginalIcon:    loadRawImage(r2[i][1]),
			OriginalOutline: loadRawImage(r2[i][2]),
		})
	}
	return nil
}

func loadRawImage(name string) *image.RGBA {
	if name == "" {
		return nil
	}
	fn := name
	if !strings.HasSuffix(fn, ".png") {
		fn += ".png"
	}
	path, ok := fileCaseMap[strings.ToLower(fn)]
	if !ok {
		return nil
	}

	f, _ := assetsFS.Open(path)
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil
	}

	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, src, b.Min, draw.Src)
	return dst
}

func getColor(idx int) color.RGBA {
	if idx < 0 || idx >= len(badgeColors) {
		return color.RGBA{255, 255, 255, 255}
	}
	c := badgeColors[idx]
	return color.RGBA{uint8(c.R), uint8(c.G), uint8(c.B), 255}
}

func calculateTotal() int {
	syms, shapes := 0, 0
	for _, icon := range badgeIcons {
		if icon.Layer == 0 {
			syms++
		} else {
			shapes++
		}
	}
	return syms * shapes * len(layer0ColorIndices) * len(layer1ColorIndices)
}

func printProgress(curr, total int64) {
	percent := float64(curr) / float64(total) * 100
	fmt.Printf("\033[2K\r[Progress] %.2f%% (%d/%d)", percent, curr, total)
}
