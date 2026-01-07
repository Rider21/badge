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
)

//go:embed assets/*
var assetsFS embed.FS

const (
	OutputPath = "images"
	OutputSize = 320
)

type BadgeColorData struct{ R, G, B int }
type BadgeIconData struct {
	IconFile        string
	OutlineFile     string
	Layer           int
	OriginalIcon    *image.RGBA
	OriginalOutline *image.RGBA
}

type RenderJob struct{ SIdx, BIdx, C1Idx, C2Idx int }

var (
	badgeColors        []BadgeColorData
	badgeIcons         []BadgeIconData
	layer0ColorIndices []int
	layer1ColorIndices []int
	rgbaPool           sync.Pool
	fileCaseMap        map[string]string
)

func init() {
	rgbaPool = sync.Pool{
		New: func() interface{} {
			return image.NewRGBA(image.Rect(0, 0, OutputSize, OutputSize))
		},
	}
}

func main() {
	fmt.Println("🚀 Starting build (Standard Libs, Parallel IO, 320px)...")
	buildFileCaseMap()

	if err := loadIcons(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}

	if len(os.Args) > 1 {
		runSingleMode(os.Args[1])
		return
	}

	// Ensure the output directory exists
	_ = os.MkdirAll(OutputPath, 0755)

	total := calculateTotal()
	fmt.Printf("✅ Resources loaded. Total combinations: %d\n", total)
	if total == 0 {
		return
	}

	// Use all available CPU cores for simultaneous rendering and encoding
	numWorkers := runtime.NumCPU()
	jobs := make(chan RenderJob, numWorkers*2)

	var processedCount int64
	var wg sync.WaitGroup

	for range numWorkers {
		wg.Go(func() {
			for j := range jobs {
				processJob(j)
				curr := atomic.AddInt64(&processedCount, 1)
				if curr%100 == 0 || curr == int64(total) {
					printProgress(curr, int64(total))
				}
			}
		})
	}

	// Generate all valid combinations of icons and colors
	go func() {
		for sIdx, symbol := range badgeIcons {
			if symbol.Layer != 0 {
				continue
			}
			for bIdx, border := range badgeIcons {
				if border.Layer != 1 {
					continue
				}
				for _, c1Idx := range layer0ColorIndices {
					for _, c2Idx := range layer1ColorIndices {
						jobs <- RenderJob{sIdx, bIdx, c1Idx, c2Idx}
					}
				}
			}
		}
		close(jobs)
	}()

	wg.Wait()
	fmt.Printf("\n✨ Generation complete! Files saved to: %s\n", OutputPath)
}

func processJob(j RenderJob) {
	fileName := fmt.Sprintf("%d-%d-%d-%d.png", j.SIdx, j.BIdx, j.C1Idx, j.C2Idx)
	fullPath := filepath.Join(OutputPath, fileName)

	// Check if file already exists to skip redundant work
	if _, err := os.Stat(fullPath); err == nil {
		return
	}

	img := renderFast(j)
	defer rgbaPool.Put(img)

	c1, c2 := getColor(j.C1Idx), getColor(j.C2Idx)
	savePNG(fullPath, img, c1, c2)
}

func renderFast(j RenderJob) *image.RGBA {
	dst := rgbaPool.Get().(*image.RGBA)
	// Clear the reused buffer from the pool
	draw.Draw(dst, dst.Bounds(), image.Transparent, image.Point{}, draw.Src)

	c1, c2 := getColor(j.C1Idx), getColor(j.C2Idx)
	symbol, border := badgeIcons[j.SIdx], badgeIcons[j.BIdx]

	// Layering order: Border Base -> Symbol Base -> Border Outline
	if border.OriginalIcon != nil {
		drawCentered(dst, border.OriginalIcon, c2)
	}
	if symbol.OriginalIcon != nil {
		drawCentered(dst, symbol.OriginalIcon, c1)
	}
	if border.OriginalOutline != nil {
		drawCentered(dst, border.OriginalOutline, c1)
	}
	return dst
}

func drawCentered(dst *image.RGBA, src *image.RGBA, tint color.RGBA) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	offX, offY := (OutputSize-sw)/2, (OutputSize-sh)/2

	// Fast per-pixel tinting (Multiply operation)
	for y := range sh {
		for x := range sw {
			si := y*src.Stride + x*4
			di := (y+offY)*dst.Stride + (x+offX)*4

			a := src.Pix[si+3]
			if a == 0 {
				continue
			}

			dst.Pix[di] = uint8(uint32(src.Pix[si]) * uint32(tint.R) / 255)
			dst.Pix[di+1] = uint8(uint32(src.Pix[si+1]) * uint32(tint.G) / 255)
			dst.Pix[di+2] = uint8(uint32(src.Pix[si+2]) * uint32(tint.B) / 255)
			dst.Pix[di+3] = a
		}
	}
}

func savePNG(path string, img *image.RGBA, c1, c2 color.RGBA) {
	// Create a 3-color palette (Transparent + 2 Tints) to minimize file size
	pal := color.Palette{color.RGBA{0, 0, 0, 0}, c1, c2}
	palImg := image.NewPaletted(img.Bounds(), pal)
	draw.Draw(palImg, palImg.Bounds(), img, image.Point{}, draw.Src)

	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	enc := png.Encoder{CompressionLevel: png.BestCompression}
	_ = enc.Encode(f, palImg)
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
	// Parse color definitions
	f1, err := assetsFS.Open("assets/csv/badge_colors.csv")
	if err != nil {
		return err
	}
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

	// Load and decode source PNG images into memory
	f2, err := assetsFS.Open("assets/csv/badge_icons.csv")
	if err != nil {
		return err
	}
	r2, _ := csv.NewReader(f2).ReadAll()
	for i := 1; i < len(r2); i++ {
		l, _ := strconv.Atoi(r2[i][3])
		badgeIcons = append(badgeIcons, BadgeIconData{
			IconFile: r2[i][1], OutlineFile: r2[i][2], Layer: l,
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
		switch icon.Layer {
		case 0:
			syms++
		case 1:
			shapes++
		}
	}
	return syms * shapes * len(layer0ColorIndices) * len(layer1ColorIndices)
}

func printProgress(curr, total int64) {
	percent := float64(curr) / float64(total) * 100
	fmt.Printf("\033[2K\r[Progress] %.2f%% (%d/%d)", percent, curr, total)
}

func runSingleMode(arg string) {
	p := strings.Split(arg, "_")
	if len(p) != 4 {
		return
	}
	s, _ := strconv.Atoi(p[0])
	b, _ := strconv.Atoi(p[1])
	c1, _ := strconv.Atoi(p[2])
	c2, _ := strconv.Atoi(p[3])
	img := renderFast(RenderJob{s, b, c1, c2})
	savePNG("badge.png", img, getColor(c1), getColor(c2))
}
