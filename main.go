package main

import (
	"embed"
	"encoding/csv"
	"fmt"
	"image"
	"image/color"
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
	OutputSize = 512
)

type BadgeColorData struct{ R, G, B int }
type BadgeIconData struct {
	IconFile      string
	OutlineFile   string
	Layer         int
	ScaledIcon    *image.RGBA
	ScaledOutline *image.RGBA
}

type RenderJob struct{ SIdx, BIdx, C1Idx, C2Idx int }
type SaveJob struct {
	FileName string
	Img      *image.RGBA
	C1, C2   color.RGBA
}

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
	fmt.Println("🚀 Indexing resources and scaling...")
	buildFileCaseMap()

	if err := loadAndPreScale(); err != nil {
		log.Fatalf("Error: %v", err)
	}

	if len(os.Args) > 1 {
		runSingleMode(os.Args[1])
		return
	}

	_ = os.MkdirAll(OutputPath, 0755)
	total := calculateTotal()
	fmt.Printf("✅ Combinations to generate: %d\n", total)

	if total == 0 {
		return
	}

	renderWorkers := runtime.NumCPU() - 1
	if renderWorkers < 1 {
		renderWorkers = 1
	}

	jobs := make(chan RenderJob, 100)
	saveChan := make(chan SaveJob, 100)
	var processedCount int64

	var renderWG sync.WaitGroup
	var saveWG sync.WaitGroup

	saveWG.Add(1)
	go func() {
		defer saveWG.Done()
		for job := range saveChan {
			saveWithMinimalPalette(job, false)
			curr := atomic.AddInt64(&processedCount, 1)
			if curr%50 == 0 || curr == int64(total) {
				printProgress(curr, int64(total))
			}
			rgbaPool.Put(job.Img)
		}
	}()

	for i := 0; i < renderWorkers; i++ {
		renderWG.Add(1)
		go func() {
			defer renderWG.Done()
			for j := range jobs {
				fname := fmt.Sprintf("%d-%d-%d-%d.png", j.SIdx, j.BIdx, j.C1Idx, j.C2Idx)
				if _, err := os.Stat(filepath.Join(OutputPath, fname)); err == nil {
					atomic.AddInt64(&processedCount, 1)
					continue
				}
				img := renderFast(j)
				saveChan <- SaveJob{fname, img, getColor(j.C1Idx), getColor(j.C2Idx)}
			}
		}()
	}

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

	renderWG.Wait()
	close(saveChan)
	saveWG.Wait()

	fmt.Printf("\n✨ Done! Used Nearest Neighbor algorithm for clarity.\n")
}

func saveWithMinimalPalette(job SaveJob, root bool) {
	minimalPalette := color.Palette{
		color.RGBA{0, 0, 0, 0},
		job.C1,
		job.C2,
	}

	palImg := image.NewPaletted(job.Img.Bounds(), minimalPalette)
	xdraw.Draw(palImg, palImg.Bounds(), job.Img, image.Point{}, xdraw.Src)

	path := filepath.Join(OutputPath, job.FileName)
	if root {
		path = job.FileName
	}

	f, _ := os.Create(path)
	defer f.Close()

	enc := png.Encoder{CompressionLevel: png.BestCompression}
	enc.Encode(f, palImg)
}

func preScaleSafe(name string, scale float64) *image.RGBA {
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
	src, _, _ := image.Decode(f)

	sz := int(float64(OutputSize) * scale)
	dst := image.NewRGBA(image.Rect(0, 0, sz, sz))
	xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

func buildFileCaseMap() {
	fileCaseMap = make(map[string]string)
	fs.WalkDir(assetsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			fileCaseMap[strings.ToLower(filepath.Base(path))] = path
		}
		return nil
	})
}

func loadAndPreScale() error {
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
	f2, err := assetsFS.Open("assets/csv/badge_icons.csv")
	if err != nil {
		return err
	}
	r2, _ := csv.NewReader(f2).ReadAll()
	for i := 1; i < len(r2); i++ {
		l, _ := strconv.Atoi(r2[i][3])
		scale := 1.0
		if l == 0 {
			scale = 0.65
		}
		badgeIcons = append(badgeIcons, BadgeIconData{
			IconFile: r2[i][1], OutlineFile: r2[i][2], Layer: l,
			ScaledIcon: preScaleSafe(r2[i][1], scale), ScaledOutline: preScaleSafe(r2[i][2], 1.0),
		})
	}
	return nil
}

func renderFast(j RenderJob) *image.RGBA {
	dst := rgbaPool.Get().(*image.RGBA)
	for i := range dst.Pix {
		dst.Pix[i] = 0
	}
	c1, c2 := getColor(j.C1Idx), getColor(j.C2Idx)
	symbol, border := badgeIcons[j.SIdx], badgeIcons[j.BIdx]
	if border.ScaledIcon != nil {
		drawFast(dst, border.ScaledIcon, c2, 0)
	}
	if symbol.ScaledIcon != nil {
		offset := (OutputSize - symbol.ScaledIcon.Bounds().Dx()) / 2
		drawFast(dst, symbol.ScaledIcon, c1, offset)
	}
	if border.ScaledOutline != nil {
		drawFast(dst, border.ScaledOutline, c1, 0)
	}
	return dst
}

func drawFast(dst *image.RGBA, src *image.RGBA, tint color.RGBA, offset int) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			si, di := y*src.Stride+x*4, (y+offset)*dst.Stride+(x+offset)*4
			if di+3 >= len(dst.Pix) {
				continue
			}
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

func getColor(idx int) color.RGBA {
	if idx < 0 || idx >= len(badgeColors) {
		return color.RGBA{255, 255, 255, 255}
	}
	c := badgeColors[idx]
	return color.RGBA{uint8(c.R), uint8(c.G), uint8(c.B), 255}
}

func printProgress(curr, total int64) {
	percent := float64(curr) / float64(total) * 100
	barLen := 30
	completed := int(float64(barLen) * (float64(curr) / float64(total)))
	if completed > barLen {
		completed = barLen
	}
	bar := strings.Repeat("█", completed) + strings.Repeat("░", barLen-completed)
	fmt.Printf("\033[2K\r[%s] %.1f%% (%d/%d)", bar, percent, curr, total)
}

func calculateTotal() int {
	syms, shapes := 0, 0
	for _, icon := range badgeIcons {
		if icon.Layer == 0 {
			syms++
		}
		if icon.Layer == 1 {
			shapes++
		}
	}
	return syms * shapes * len(layer0ColorIndices) * len(layer1ColorIndices)
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
	saveWithMinimalPalette(SaveJob{"badge.png", img, getColor(c1), getColor(c2)}, true)
}
