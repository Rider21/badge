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

type BadgeColorData struct{ R, G, B int }

type BadgeIconData struct {
	Layer         int
	ScaledIcon    *image.Alpha
	ScaledOutline *image.Alpha
}

type RenderJob struct {
	Filename string
	Symbol   *image.Alpha
	Border   *image.Alpha
	Outline  *image.Alpha
	FgColor  color.RGBA
	BgColor  color.RGBA
}

var (
	badgeColors        []BadgeColorData
	badgeIcons         []BadgeIconData
	layer0ColorIndices []int
	layer1ColorIndices []int

	rgbaPool sync.Pool

	fileCaseMap   map[string]string
	existingFiles map[string]bool
)

func init() {
	rgbaPool = sync.Pool{
		New: func() interface{} {
			return image.NewRGBA(image.Rect(0, 0, OutputSize, OutputSize))
		},
	}
}

func main() {
	fmt.Println("Starting build...")
	buildFileCaseMap()

	// Load all badge icons and colors
	if err := loadIcons(); err != nil {
		log.Fatalf("Critical error: %v", err)
	}

	// Single badge generation mode
	if len(os.Args) > 1 {
		runSingleMode(os.Args[1])
		return
	}

	// Batch generation mode
	os.MkdirAll(OutputPath, 0755)
	scanOutputDirectory()

	total := calculateTotal()
	fmt.Printf("Total combinations: %d\n", total)
	if total == 0 {
		return
	}

	// Initialize worker pool
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

	go func() {
		for _, c1Idx := range layer0ColorIndices {
			c1 := getColor(c1Idx)
			for _, c2Idx := range layer1ColorIndices {
				c2 := getColor(c2Idx)

				for sIdx, symbol := range badgeIcons {
					if symbol.Layer != 0 {
						continue
					}
					for bIdx, border := range badgeIcons {
						if border.Layer != 1 {
							continue
						}
						fName := fmt.Sprintf("%d-%d-%d-%d.png", sIdx, bIdx, c1Idx, c2Idx)
						if existingFiles[fName] {
							atomic.AddInt64(&processedCount, 1)
							continue
						}

						jobs <- RenderJob{
							Filename: fName,
							Symbol:   symbol.ScaledIcon,
							Border:   border.ScaledIcon,
							Outline:  border.ScaledOutline,
							FgColor:  c1,
							BgColor:  c2,
						}
					}
				}
			}
		}
		close(jobs)
	}()

	wg.Wait()
	fmt.Printf("\nGeneration complete!\n")
}

func processJob(j RenderJob) {
	// Get fresh RGBA buffer from pool
	src := rgbaPool.Get().(*image.RGBA)
	defer rgbaPool.Put(src)

	// Clear buffer
	draw.Draw(src, src.Bounds(), image.Transparent, image.Point{}, draw.Src)

	// Layer components: border (background), symbol, outline
	if j.Border != nil {
		drawTinted(src, j.Border, j.BgColor)
	}
	if j.Symbol != nil {
		drawTinted(src, j.Symbol, j.FgColor)
	}
	if j.Outline != nil {
		drawTinted(src, j.Outline, j.FgColor)
	}

	savePNG(j.Filename, src)
}

func drawTinted(dst *image.RGBA, mask *image.Alpha, tint color.Color) {
	// Composite tinted mask onto destination using alpha masking
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(tint), image.Point{}, mask, image.Point{}, draw.Over)
}

func savePNG(filename string, img *image.RGBA) {
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
	enc.Encode(f, img)
}

func preProcessImage(src *image.RGBA, scale float64, scratch *image.RGBA) *image.Alpha {
	if src == nil {
		return nil
	}

	// Calculate scaled dimensions
	sb := src.Bounds()
	w := int(float64(sb.Dx()) * scale)
	h := int(float64(sb.Dy()) * scale)

	// Ensure even dimensions for proper centering
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	// Center scaled image in output canvas
	ox := (OutputSize - w) / 2
	oy := (OutputSize - h) / 2
	rect := image.Rect(ox, oy, ox+w, oy+h)

	draw.Draw(scratch, scratch.Bounds(), image.Transparent, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(scratch, rect, src, sb, xdraw.Over, nil)

	// Extract alpha channel from scaled scratch buffer
	alpha := image.NewAlpha(image.Rect(0, 0, OutputSize, OutputSize))
	for y := range OutputSize {
		dstOffset := y * alpha.Stride
		srcOffset := y * scratch.Stride
		for x := range OutputSize {
			alpha.Pix[dstOffset+x] = scratch.Pix[srcOffset+x*4+3]
		}
	}
	return alpha
}

func loadIcons() error {
	fColor, err := assetsFS.Open("assets/csv/badge_colors.csv")
	if err != nil {
		return err
	}
	defer fColor.Close()

	r1, _ := csv.NewReader(fColor).ReadAll()
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

	fIcon, err := assetsFS.Open("assets/csv/badge_icons.csv")
	if err != nil {
		return err
	}
	defer fIcon.Close()

	r2, _ := csv.NewReader(fIcon).ReadAll()
	loadScratch := image.NewRGBA(image.Rect(0, 0, OutputSize, OutputSize))

	for i := 1; i < len(r2); i++ {
		l, _ := strconv.Atoi(r2[i][3])
		scale := 1.0
		if l == 0 {
			scale = 0.7
		}

		rawIcon := loadRawImage(r2[i][1])
		rawOutline := loadRawImage(r2[i][2])

		badgeIcons = append(badgeIcons, BadgeIconData{
			Layer:         l,
			ScaledIcon:    preProcessImage(rawIcon, scale, loadScratch),
			ScaledOutline: preProcessImage(rawOutline, scale, loadScratch),
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

func scanOutputDirectory() {
	existingFiles = make(map[string]bool)
	entries, err := os.ReadDir(OutputPath)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
				existingFiles[e.Name()] = true
			}
		}
	}
}

func buildFileCaseMap() {
	fileCaseMap = make(map[string]string)
	fs.WalkDir(assetsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fileCaseMap[strings.ToLower(filepath.Base(path))] = path
		}
		return nil
	})
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

func runSingleMode(arg string) {
	parts := strings.Split(arg, "_")
	if len(parts) != 4 {
		return
	}
	s, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	c1Idx, _ := strconv.Atoi(parts[2])
	c2Idx, _ := strconv.Atoi(parts[3])

	c1, c2 := getColor(c1Idx), getColor(c2Idx)

	job := RenderJob{
		Filename: "badge.png",
		Symbol:   badgeIcons[s].ScaledIcon,
		Border:   badgeIcons[b].ScaledIcon,
		Outline:  badgeIcons[b].ScaledOutline,
		FgColor:  c1,
		BgColor:  c2,
	}
	processJob(job)
	fmt.Println("Generated badge.png")
}
