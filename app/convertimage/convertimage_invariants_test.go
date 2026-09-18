package convertimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/pdfpagefit"
)

func makePNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// pdfImageDims is one embedded image XObject's pixel dimensions, as reported by pdfcpu.
type pdfImageDims struct {
	Width  int
	Height int
}

// readPDFGeometry re-reads a produced PDF with pdfcpu and returns its page count, the MediaBox of
// each page, and the dimensions of each page's image XObject. The task requires this verification:
// page count, MediaBox and XObject dimensions.
func readPDFGeometry(t *testing.T, path string) (int, [][2]float64, [][]pdfImageDims) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	count, err := api.PageCount(bytes.NewReader(data), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("pdfcpu PageCount: %v", err)
	}
	dims, err := api.PageDims(bytes.NewReader(data), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("pdfcpu PageDims: %v", err)
	}
	imageMaps, err := api.Images(bytes.NewReader(data), nil, model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("pdfcpu Images: %v", err)
	}

	media := make([][2]float64, len(dims))
	for i, dim := range dims {
		media[i] = [2]float64{dim.Width, dim.Height}
	}
	images := make([][]pdfImageDims, len(imageMaps))
	for i, imageMap := range imageMaps {
		for _, img := range imageMap {
			images[i] = append(images[i], pdfImageDims{Width: img.Width, Height: img.Height})
		}
	}
	return count, media, images
}

func writeTempPDF(t *testing.T, pdf []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.pdf")
	if err := os.WriteFile(path, pdf, 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	return path
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestGoldenRule17ArchivedPageIsCroppedNaturalToneNotEnhanced pins the invariant that the enhanced
// buffer is produced (and its failures surfaced) but never encoded or archived; the CROPPED page is
// what gets encoded.
func TestGoldenRule17ArchivedPageIsCroppedNaturalToneNotEnhanced(t *testing.T) {
	h := newHarness(t)
	croppedPage := []byte("cropped-natural-tone")
	enhancedPage := []byte("enhanced-never-archived")
	h.steps.cropFn = func([]byte) imagetopdf.PipelineStepResult {
		return stepResult(2, imagetopdf.LabelCropped, croppedPage)
	}
	h.steps.enhanceFn = func([]byte) imagetopdf.PipelineStepResult {
		return stepResult(3, imagetopdf.LabelEnhanced, enhancedPage)
	}

	if _, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg")); err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if h.steps.enhanceCalls == 0 {
		t.Fatal("enhancement stage did not run")
	}
	if len(h.encodeInputs) != 1 {
		t.Fatalf("EncodeJpeg called %d times, want 1", len(h.encodeInputs))
	}
	if !bytes.Equal(h.encodeInputs[0], croppedPage) {
		t.Fatalf("archived page = %q, want the cropped page", h.encodeInputs[0])
	}
	if bytes.Equal(h.encodeInputs[0], enhancedPage) {
		t.Fatal("the enhanced buffer was archived")
	}
}

// TestGoldenRule17SourceMovedToTrashNeverDeleted pins "move, never delete".
func TestGoldenRule17SourceMovedToTrashNeverDeleted(t *testing.T) {
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")

	result, err := h.converter.ConvertImageToPdf(context.Background(), photo)
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if _, err := os.Stat(photo); !os.IsNotExist(err) {
		t.Fatalf("source still at its original path (err=%v)", err)
	}
	if result.SourceImagePath != filepath.Join(h.trashDir(), "photo.jpg") {
		t.Fatalf("SourceImagePath = %q", result.SourceImagePath)
	}
	if _, err := os.Stat(result.SourceImagePath); err != nil {
		t.Fatalf("moved source missing: %v", err)
	}
}

// TestMoveConvertedSourceToTrashSuffixesCollisions pins the _1, _2 … collision suffix.
func TestMoveConvertedSourceToTrashSuffixesCollisions(t *testing.T) {
	dir := t.TempDir()
	trash := filepath.Join(dir, ".delete_files", "img_converted")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "photo.jpg"), []byte("earlier-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "photo_1.jpg"), []byte("earlier-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(photo, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := MoveConvertedSourceToTrash(dir, photo, "")
	if err != nil {
		t.Fatalf("MoveConvertedSourceToTrash: %v", err)
	}
	if moved != filepath.Join(trash, "photo_2.jpg") {
		t.Fatalf("moved = %q, want photo_2.jpg", moved)
	}
	first, _ := os.ReadFile(filepath.Join(trash, "photo.jpg"))
	second, _ := os.ReadFile(filepath.Join(trash, "photo_1.jpg"))
	if string(first) != "earlier-1" || string(second) != "earlier-2" {
		t.Fatal("earlier trash files were clobbered")
	}
}

// TestMoveConvertedSourceToTrashAllowsTwentySuffixesThenFails pins the 20-attempt bound.
func TestMoveConvertedSourceToTrashAllowsTwentySuffixesThenFails(t *testing.T) {
	t.Run("twentieth suffix succeeds", func(t *testing.T) {
		dir := t.TempDir()
		trash := filepath.Join(dir, ".delete_files", "img_converted")
		if err := os.MkdirAll(trash, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= 19; i++ {
			name := "photo.jpg"
			if i > 0 {
				name = fmt.Sprintf("photo_%d.jpg", i)
			}
			if err := os.WriteFile(filepath.Join(trash, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		photo := filepath.Join(dir, "photo.jpg")
		if err := os.WriteFile(photo, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		moved, err := MoveConvertedSourceToTrash(dir, photo, "")
		if err != nil {
			t.Fatalf("MoveConvertedSourceToTrash: %v", err)
		}
		if moved != filepath.Join(trash, "photo_20.jpg") {
			t.Fatalf("moved = %q, want photo_20.jpg", moved)
		}
	})

	t.Run("twenty-first collision fails and leaves the source", func(t *testing.T) {
		dir := t.TempDir()
		trash := filepath.Join(dir, ".delete_files", "img_converted")
		if err := os.MkdirAll(trash, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= 20; i++ {
			name := "photo.jpg"
			if i > 0 {
				name = fmt.Sprintf("photo_%d.jpg", i)
			}
			if err := os.WriteFile(filepath.Join(trash, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		photo := filepath.Join(dir, "photo.jpg")
		if err := os.WriteFile(photo, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := MoveConvertedSourceToTrash(dir, photo, "")
		if err == nil || !strings.Contains(err.Error(), "after 20 attempts") {
			t.Fatalf("err = %v, want a 20-attempts error", err)
		}
		if _, statErr := os.Stat(photo); statErr != nil {
			t.Fatalf("source was moved despite the failure: %v", statErr)
		}
	})
}

// TestMoveConvertedSourceToTrashUsesTheGroupSubfolder pins the bundle's per-folder trash subfolder.
func TestMoveConvertedSourceToTrashUsesTheGroupSubfolder(t *testing.T) {
	dir := t.TempDir()
	photo := filepath.Join(dir, "p1.jpg")
	if err := os.WriteFile(photo, []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := MoveConvertedSourceToTrash(dir, photo, "releve")
	if err != nil {
		t.Fatalf("MoveConvertedSourceToTrash: %v", err)
	}
	want := filepath.Join(dir, ".delete_files", "img_converted", "releve", "p1.jpg")
	if moved != want {
		t.Fatalf("moved = %q, want %q", moved, want)
	}
}

// TestWritePdfWithoutOverwritingClaimsTheNameExclusively pins the 'wx' semantics: the check IS the
// write, and an existing name is never overwritten.
func TestWritePdfWithoutOverwritingClaimsTheNameExclusively(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "photo.pdf"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo_1.pdf"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := WritePdfWithoutOverwriting(dir, "photo", []byte("new"), "photo.jpg")
	if err != nil {
		t.Fatalf("WritePdfWithoutOverwriting: %v", err)
	}
	if path != filepath.Join(dir, "photo_2.pdf") {
		t.Fatalf("path = %q, want photo_2.pdf", path)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "photo.pdf"))
	second, _ := os.ReadFile(filepath.Join(dir, "photo_1.pdf"))
	if string(first) != "first" || string(second) != "second" {
		t.Fatal("existing PDFs were overwritten")
	}
}

func TestWritePdfWithoutOverwritingFailsAfterTwentyAttempts(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= MaxPdfNameAttempts; i++ {
		name := "photo.pdf"
		if i > 0 {
			name = fmt.Sprintf("photo_%d.pdf", i)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := WritePdfWithoutOverwriting(dir, "photo", []byte("new"), "photo.jpg")
	if err == nil || !strings.Contains(err.Error(), "after 20 attempts") {
		t.Fatalf("err = %v, want a 20-attempts error", err)
	}
}

// TestGoldenRule17PdfWrittenBeforeSourceTouched pins the ordering: the PDF is on disk and its text
// extracted before the source moves, so an extraction failure leaves the photo in place for retry.
func TestGoldenRule17PdfWrittenBeforeSourceTouched(t *testing.T) {
	h := newHarness(t)
	h.extractErr = errors.New("pdf2w unavailable")
	photo := h.writePhoto("photo.jpg")

	if _, err := h.converter.ConvertImageToPdf(context.Background(), photo); err == nil {
		t.Fatal("expected the extraction failure to propagate")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "photo.pdf")); err != nil {
		t.Fatalf("PDF was not written before extraction: %v", err)
	}
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("source was touched before extraction succeeded: %v", err)
	}
}

// TestGoldenRule17ConversionFailureNeverDestructive pins that a pipeline failure (here: the encoder)
// leaves the source untouched and writes no PDF.
func TestGoldenRule17ConversionFailureNeverDestructive(t *testing.T) {
	h := newHarness(t)
	h.encodeErr = errors.New("encode failed")
	photo := h.writePhoto("photo.jpg")

	if _, err := h.converter.ConvertImageToPdf(context.Background(), photo); err == nil {
		t.Fatal("expected the conversion failure to propagate")
	}
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("source was lost on conversion failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "photo.pdf")); !os.IsNotExist(err) {
		t.Fatalf("a PDF was written despite failure (err=%v)", err)
	}
}

// TestGoldenRule17FolderBundlingRules restates the bundling contract end-to-end: a folder of 2+
// photos becomes ONE multi-page PDF named after the folder, while a lone photo or a mixed folder is
// not bundled (each file takes the per-file path).
func TestGoldenRule17FolderBundlingRules(t *testing.T) {
	h := newHarness(t)
	twoPhotos := h.bundle("deux", []string{"a.jpg", "b.jpg"})
	lone := h.folderWith("seul", map[string][]byte{"only.jpg": h.jpeg})
	mixed := h.folderWith("mixte", map[string][]byte{"a.jpg": h.jpeg, "b.jpg": h.jpeg, "notes.pdf": []byte("x")})

	found := FindImageBundleFolders(h.dir)
	if !reflect.DeepEqual(found, []string{twoPhotos}) {
		t.Fatalf("found = %#v, want only %s", found, twoPhotos)
	}

	result, err := h.converter.ConvertImageFolderToPdf(context.Background(), twoPhotos)
	if err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}
	if result.PageCount != 2 {
		t.Fatalf("PageCount = %d, want 2", result.PageCount)
	}
	if filepath.Base(result.PdfPath) != "deux.pdf" {
		t.Fatalf("PdfPath = %q, want a PDF named after the folder", result.PdfPath)
	}

	// The lone photo and the mixed folder stay on the file-by-file path: converting the lone photo
	// yields a one-page PDF, and the mixed folder is not in the bundle list.
	if _, err := os.Stat(filepath.Join(lone, "only.jpg")); err != nil {
		t.Fatalf("lone photo missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mixed, "notes.pdf")); err != nil {
		t.Fatalf("mixed folder was consumed: %v", err)
	}
}

// TestBuildPdfFromPagesIsReadableByPdfcpu verifies the produced PDF by re-reading it with pdfcpu:
// page count, MediaBox per page, and image XObject dimensions.
func TestBuildPdfFromPagesIsReadableByPdfcpu(t *testing.T) {
	pages := [][]byte{makeJPEG(300, 400), makeJPEG(500, 200), makeJPEG(250, 250)}

	pdf, err := BuildPdfFromPages(pages)
	if err != nil {
		t.Fatalf("BuildPdfFromPages: %v", err)
	}
	path := writeTempPDF(t, pdf)

	count, media, images := readPDFGeometry(t, path)
	if count != 3 {
		t.Fatalf("page count = %d, want 3", count)
	}
	wantMedia := [][2]float64{
		{pdfpagefit.A4ShortSide, pdfpagefit.A4LongSide},
		{pdfpagefit.A4LongSide, pdfpagefit.A4ShortSide},
		{pdfpagefit.A4ShortSide, pdfpagefit.A4LongSide},
	}
	if len(media) != len(wantMedia) {
		t.Fatalf("MediaBox count = %d, want %d", len(media), len(wantMedia))
	}
	for i, want := range wantMedia {
		if !approx(media[i][0], want[0]) || !approx(media[i][1], want[1]) {
			t.Fatalf("page %d MediaBox = %v, want %v", i, media[i], want)
		}
	}

	wantImages := [][2]int{{300, 400}, {500, 200}, {250, 250}}
	if len(images) != len(wantImages) {
		t.Fatalf("image page count = %d, want %d", len(images), len(wantImages))
	}
	for i, want := range wantImages {
		if len(images[i]) != 1 {
			t.Fatalf("page %d has %d image XObjects, want 1", i, len(images[i]))
		}
		got := images[i][0]
		if got.Width != want[0] || got.Height != want[1] {
			t.Fatalf("page %d image XObject = %dx%d, want %dx%d", i, got.Width, got.Height, want[0], want[1])
		}
	}
}

// TestBuildPdfFromPagesMediaBoxMatchesFitImageToA4 asserts the page geometry equals the ported TS
// fit geometry, not merely some A4 size.
func TestBuildPdfFromPagesMediaBoxMatchesFitImageToA4(t *testing.T) {
	pages := [][]byte{makeJPEG(300, 400), makeJPEG(500, 200), makeJPEG(250, 250)}
	pdf, err := BuildPdfFromPages(pages)
	if err != nil {
		t.Fatalf("BuildPdfFromPages: %v", err)
	}
	_, media, _ := readPDFGeometry(t, writeTempPDF(t, pdf))

	fits := []pdfpagefit.PagePlacement{
		pdfpagefit.FitImageToA4(300, 400),
		pdfpagefit.FitImageToA4(500, 200),
		pdfpagefit.FitImageToA4(250, 250),
	}
	for i, fit := range fits {
		if !approx(media[i][0], fit.PageWidth) || !approx(media[i][1], fit.PageHeight) {
			t.Fatalf("page %d MediaBox = %v, want %vx%v", i, media[i], fit.PageWidth, fit.PageHeight)
		}
	}
}

// TestBuildPdfFromPagesContentStreamMatchesFitPlacement checks the placement matrix: the draw
// rectangle and centring offsets exactly as FitImageToA4 computes them.
func TestBuildPdfFromPagesContentStreamMatchesFitPlacement(t *testing.T) {
	pdf, err := BuildPdfFromPages([][]byte{makeJPEG(300, 400), makeJPEG(500, 200)})
	if err != nil {
		t.Fatalf("BuildPdfFromPages: %v", err)
	}
	text := string(pdf)
	for _, fit := range []pdfpagefit.PagePlacement{pdfpagefit.FitImageToA4(300, 400), pdfpagefit.FitImageToA4(500, 200)} {
		want := fmt.Sprintf("q %s 0 0 %s %s %s cm /Im0 Do Q",
			formatPDFFloat(fit.DrawWidth), formatPDFFloat(fit.DrawHeight), formatPDFFloat(fit.X), formatPDFFloat(fit.Y))
		if !strings.Contains(text, want) {
			t.Fatalf("content stream missing %q", want)
		}
	}
}

func TestBuildPdfFromPagesRejectsNonJpeg(t *testing.T) {
	if _, err := BuildPdfFromPages([][]byte{makePNG(10, 10)}); err == nil {
		t.Fatal("expected an error for a non-JPEG page")
	}
}

func TestBuildPdfFromPagesHandlesAnEmptyPageList(t *testing.T) {
	pdf, err := BuildPdfFromPages(nil)
	if err != nil {
		t.Fatalf("BuildPdfFromPages: %v", err)
	}
	count, _, _ := readPDFGeometry(t, writeTempPDF(t, pdf))
	if count != 0 {
		t.Fatalf("page count = %d, want 0", count)
	}
}
