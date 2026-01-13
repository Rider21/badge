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
	Palette  color.Palette
	Symbol   *image.Alpha
	Border   *image.Alpha
	Outline  *image.Alpha
}

var (
	badgeColors        []BadgeColorData
	badgeIcons         []BadgeIconData
	layer0ColorIndices []int
	layer1ColorIndices []int

	rgbaPool    sync.Pool
	palettePool sync.Pool

	fileCaseMap   map[string]string
	existingFiles map[string]bool
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

	if len(os.Args) > 1 {
		runSingleMode(os.Args[1])
		return
	}

	os.MkdirAll(OutputPath, 0755)
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

	go func() {
		for _, c1Idx := range layer0ColorIndices {
			c1 := getColor(c1Idx)
			for _, c2Idx := range layer1ColorIndices {
				c2 := getColor(c2Idx)

				smartPalette := createSmartPalette(c1, c2)

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
							Palette:  smartPalette,
							Symbol:   symbol.ScaledIcon,
							Border:   border.ScaledIcon,
							Outline:  border.ScaledOutline,
						}
					}
				}
			}
		}
		close(jobs)
	}()

	wg.Wait()
	fmt.Printf("\n✨ Generation complete!\n")
}

func processJob(j RenderJob) {
	dst := rgbaPool.Get().(*image.RGBA)
	defer rgbaPool.Put(dst)

	draw.Draw(dst, dst.Bounds(), image.Transparent, image.Point{}, draw.Src)

	// Layer order: Border -> Symbol -> Outline
	if j.Border != nil {
		drawTinted(dst, j.Border, j.Palette[5]) // Use 100% C2
	}
	if j.Symbol != nil {
		drawTinted(dst, j.Symbol, j.Palette[1]) // Use 100% C1
	}
	if j.Outline != nil {
		drawTinted(dst, j.Outline, j.Palette[1])
	}

	savePNG(j.Filename, dst, j.Palette)
}

func drawTinted(dst *image.RGBA, mask *image.Alpha, tint color.Color) {
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(tint), image.Point{}, mask, image.Point{}, draw.Over)
}

func createSmartPalette(c1, c2 color.RGBA) color.Palette {
	pal := make(color.Palette, 0, 9)
	pal = append(pal, color.RGBA{0, 0, 0, 0}) // Index 0: Transparent

	// 4 steps of alpha for each color to handle anti-aliasing
	alphas := []uint8{255, 170, 85, 20}

	// C1 indices: 1, 2, 3, 4
	for _, a := range alphas {
		pal = append(pal, color.RGBA{c1.R, c1.G, c1.B, a})
	}
	// C2 indices: 5, 6, 7, 8
	for _, a := range alphas {
		pal = append(pal, color.RGBA{c2.R, c2.G, c2.B, a})
	}
	return pal
}

func savePNG(filename string, img *image.RGBA, pal color.Palette) {
	palImg := palettePool.Get().(*image.Paletted)
	defer palettePool.Put(palImg)

	palImg.Palette = pal
	draw.Draw(palImg, palImg.Bounds(), img, image.Point{}, draw.Src)

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
	enc.Encode(f, palImg)
}

func preProcessImage(src *image.RGBA, scale float64, scratch *image.RGBA) *image.Alpha {
	if src == nil {
		return nil
	}

	sb := src.Bounds()
	w, h := int(float64(sb.Dx())*scale), int(float64(sb.Dy())*scale)
	ox, oy := (OutputSize-w)/2, (OutputSize-h)/2
	rect := image.Rect(ox, oy, ox+w, oy+h)

	draw.Draw(scratch, scratch.Bounds(), image.Transparent, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(scratch, rect, src, sb, xdraw.Over, nil)

	alpha := image.NewAlpha(image.Rect(0, 0, OutputSize, OutputSize))

	for y := range OutputSize {
		dstOffset := y * alpha.Stride
		srcOffset := y * scratch.Stride

		for x := range OutputSize {
			a := scratch.Pix[srcOffset+x*4+3]
			alpha.Pix[dstOffset+x] = a
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
	pal := createSmartPalette(c1, c2)

	job := RenderJob{
		Filename: "badge.png",
		Palette:  pal,
		Symbol:   badgeIcons[s].ScaledIcon,
		Border:   badgeIcons[b].ScaledIcon,
		Outline:  badgeIcons[b].ScaledOutline,
	}
	processJob(job)
	fmt.Println("✅ Generated badge.png")
}
