package convertimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
)

// These cases are ported from pdf-triage's src/application/convert-image-document.test.ts (28
// cases: 2 isImageFile, 13 convertImageToPdf, 6 findImageBundleFolders, 1
// sortImagePagesNaturally, 6 convertImageFolderToPdf). The upstream TypeScript suite is GREEN at
// port time (`npx vitest run src/application/convert-image-document.test.ts` -> 28 passed), so no
// upstream case is pinned red.
//
// The TS suite mocked the whole image-to-pdf module (including the retired runExtractStep), the
// extractor and encodeJpeg, and redirected CONFIG.INPUT_DIR. The Go port injects the same seams
// through Deps and uses t.TempDir() for all filesystem work; runExtractStep has no Go equivalent
// because step 4 is retired.

func makeJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(1, 1, 4, 4), image.NewUniform(color.RGBA{R: 0, G: 0, B: 0, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func stepResult(step int, label string, buffer []byte) imagetopdf.PipelineStepResult {
	return imagetopdf.PipelineStepResult{
		Step:        step,
		Label:       label,
		ImageBase64: base64.StdEncoding.EncodeToString(buffer),
		DurationMS:  1,
	}
}

type fakeSteps struct {
	orientFn      func([]byte) imagetopdf.PipelineStepResult
	cropFn        func([]byte) imagetopdf.PipelineStepResult
	enhanceFn     func([]byte) imagetopdf.PipelineStepResult
	orientInputs  [][]byte
	cropInputs    [][]byte
	enhanceInputs [][]byte
	enhanceCalls  int
}

func (f *fakeSteps) RunOrientStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	f.orientInputs = append(f.orientInputs, append([]byte(nil), buffer...))
	if f.orientFn != nil {
		return f.orientFn(buffer)
	}
	return stepResult(1, imagetopdf.LabelOriented, buffer)
}

func (f *fakeSteps) RunCropStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	f.cropInputs = append(f.cropInputs, append([]byte(nil), buffer...))
	if f.cropFn != nil {
		return f.cropFn(buffer)
	}
	return stepResult(2, imagetopdf.LabelCropped, buffer)
}

func (f *fakeSteps) RunEnhanceStep(_ context.Context, buffer []byte) imagetopdf.PipelineStepResult {
	f.enhanceCalls++
	f.enhanceInputs = append(f.enhanceInputs, append([]byte(nil), buffer...))
	if f.enhanceFn != nil {
		return f.enhanceFn(buffer)
	}
	return stepResult(3, imagetopdf.LabelEnhanced, buffer)
}

type harness struct {
	t             *testing.T
	dir           string
	jpeg          []byte
	steps         *fakeSteps
	extractCalls  []string
	extractResult pdfextractor.ExtractedPDF
	extractErr    error
	encodeInputs  [][]byte
	encodeResult  []byte
	encodeErr     error
	converter     *Converter
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		dir:   t.TempDir(),
		jpeg:  makeJPEG(8, 8),
		steps: &fakeSteps{},
		extractResult: pdfextractor.ExtractedPDF{
			Checksum: "pdf-checksum",
			RawText:  "# Invoice\n\ntotal 42",
			Numpages: 1,
		},
	}
	h.encodeResult = makeJPEG(8, 8)
	h.converter = NewConverter(Deps{
		InputDir: h.dir,
		Steps:    h.steps,
		Extract: func(pdfPath string) (pdfextractor.ExtractedPDF, error) {
			h.extractCalls = append(h.extractCalls, pdfPath)
			if h.extractErr != nil {
				return pdfextractor.ExtractedPDF{}, h.extractErr
			}
			return h.extractResult, nil
		},
		EncodeJpeg: func(buffer []byte, _ int) ([]byte, error) {
			h.encodeInputs = append(h.encodeInputs, append([]byte(nil), buffer...))
			if h.encodeErr != nil {
				return nil, h.encodeErr
			}
			return h.encodeResult, nil
		},
	})
	return h
}

func (h *harness) writePhoto(name string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, h.jpeg, 0o644); err != nil {
		h.t.Fatalf("write photo: %v", err)
	}
	return path
}

func (h *harness) trashDir() string {
	return filepath.Join(h.dir, ".delete_files", "img_converted")
}

func (h *harness) folderWith(name string, files map[string][]byte) string {
	h.t.Helper()
	dir := filepath.Join(h.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	for filename, content := range files {
		if err := os.WriteFile(filepath.Join(dir, filename), content, 0o644); err != nil {
			h.t.Fatalf("write %s: %v", filename, err)
		}
	}
	return dir
}

func (h *harness) bundle(name string, files []string) string {
	h.t.Helper()
	dir := filepath.Join(h.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	for _, filename := range files {
		if err := os.WriteFile(filepath.Join(dir, filename), h.jpeg, 0o644); err != nil {
			h.t.Fatalf("write %s: %v", filename, err)
		}
	}
	return dir
}

func hasPDFHeader(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		t.Fatalf("%s does not start with %%PDF-", path)
	}
}

func TestIsImageFileRecognisesPhotoExtensionsCaseInsensitively(t *testing.T) {
	for _, name := range []string{"a.jpg", "a.JPEG", "a.png", "a.webp", "a.bmp", "a.tiff"} {
		if !IsImageFile(name) {
			t.Fatalf("IsImageFile(%q) = false, want true", name)
		}
	}
}

func TestIsImageFileDoesNotClaimPDFsOrOtherDocuments(t *testing.T) {
	for _, name := range []string{"a.pdf", "a.docx", "a.txt", "a.xlsx", "noext"} {
		if IsImageFile(name) {
			t.Fatalf("IsImageFile(%q) = true, want false", name)
		}
	}
}

func TestConvertImageToPdfWritesPdfBesidePhotoAndRemovesPhoto(t *testing.T) {
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")

	result, err := h.converter.ConvertImageToPdf(context.Background(), photo)
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if result.PdfPath != filepath.Join(h.dir, "photo.pdf") {
		t.Fatalf("PdfPath = %q", result.PdfPath)
	}
	if _, err := os.Stat(result.PdfPath); err != nil {
		t.Fatalf("PDF missing: %v", err)
	}
	if _, err := os.Stat(photo); !os.IsNotExist(err) {
		t.Fatalf("source photo still present (err=%v)", err)
	}
	hasPDFHeader(t, result.PdfPath)
}

func TestConvertImageToPdfKeepsTheSourcePhotoInTrashInsteadOfDeletingIt(t *testing.T) {
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")
	originalBytes, err := os.ReadFile(photo)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.converter.ConvertImageToPdf(context.Background(), photo); err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	kept := filepath.Join(h.trashDir(), "photo.jpg")
	keptBytes, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("kept source missing: %v", err)
	}
	if !bytes.Equal(keptBytes, originalBytes) {
		t.Fatal("kept source bytes differ from the original")
	}
}

func TestConvertImageToPdfSuffixesRatherThanOverwritingAnEarlierConvertedSource(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.trashDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.trashDir(), "photo.jpg"), []byte("an earlier photo from another camera"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg")); err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	earlier, err := os.ReadFile(filepath.Join(h.trashDir(), "photo.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(earlier) != "an earlier photo from another camera" {
		t.Fatal("earlier trash file was clobbered")
	}
	if _, err := os.Stat(filepath.Join(h.trashDir(), "photo_1.jpg")); err != nil {
		t.Fatalf("photo_1.jpg missing: %v", err)
	}
}

func TestConvertImageToPdfParksTheSourceUnderADotDirectory(t *testing.T) {
	h := newHarness(t)
	result, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg"))
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}
	if !strings.Contains(result.SourceImagePath, filepath.Join(".delete_files", "img_converted")) {
		t.Fatalf("SourceImagePath = %q, want .delete_files/img_converted", result.SourceImagePath)
	}
}

func TestConvertImageToPdfNeverOverwritesAnUnrelatedPDFThatOwnsTheName(t *testing.T) {
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")
	occupied := filepath.Join(h.dir, "photo.pdf")
	originalBytes := []byte("%PDF-1.7 the pre-existing signed contract")
	if err := os.WriteFile(occupied, originalBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := h.converter.ConvertImageToPdf(context.Background(), photo)
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	// The pre-existing document is byte-for-byte intact...
	got, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, originalBytes) {
		t.Fatal("pre-existing PDF was overwritten")
	}
	// ...and the conversion took a different, free name that the caller can follow.
	if result.PdfPath == occupied {
		t.Fatal("PdfPath points at the pre-existing PDF")
	}
	if result.PdfPath != filepath.Join(h.dir, "photo_1.pdf") {
		t.Fatalf("PdfPath = %q, want photo_1.pdf", result.PdfPath)
	}
	hasPDFHeader(t, result.PdfPath)
}

func TestConvertImageToPdfKeepsSuffixingPastTheFirstCollision(t *testing.T) {
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")
	if err := os.WriteFile(filepath.Join(h.dir, "photo.pdf"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "photo_1.pdf"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := h.converter.ConvertImageToPdf(context.Background(), photo)
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if result.PdfPath != filepath.Join(h.dir, "photo_2.pdf") {
		t.Fatalf("PdfPath = %q, want photo_2.pdf", result.PdfPath)
	}
	first, _ := os.ReadFile(filepath.Join(h.dir, "photo.pdf"))
	second, _ := os.ReadFile(filepath.Join(h.dir, "photo_1.pdf"))
	if string(first) != "first" || string(second) != "second" {
		t.Fatal("earlier colliding PDFs were clobbered")
	}
}

func TestConvertImageToPdfGetsTheTextFromTheAssembledPdfNotLocalOCR(t *testing.T) {
	h := newHarness(t)
	h.extractResult.RawText = "pdf2w extracted text"

	result, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg"))
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if result.RawText != "pdf2w extracted text" {
		t.Fatalf("RawText = %q", result.RawText)
	}
	if len(h.extractCalls) != 1 || h.extractCalls[0] != result.PdfPath {
		t.Fatalf("extract calls = %#v, want [%s]", h.extractCalls, result.PdfPath)
	}
}

func TestConvertImageToPdfChecksumsThePdfNotTheDiscardedPhoto(t *testing.T) {
	h := newHarness(t)
	result, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg"))
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}
	pdfBytes, err := os.ReadFile(result.PdfPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pdfBytes)
	if result.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("Checksum = %q, want sha256(pdf)", result.Checksum)
	}
}

func TestConvertImageToPdfArchivesTheCroppedPageAndExtractsFromTheAssembledPdf(t *testing.T) {
	h := newHarness(t)
	croppedPage := []byte("cropped-page")
	h.steps.cropFn = func([]byte) imagetopdf.PipelineStepResult {
		return stepResult(2, imagetopdf.LabelCropped, croppedPage)
	}

	result, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg"))
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}

	if len(h.encodeInputs) != 1 || !bytes.Equal(h.encodeInputs[0], croppedPage) {
		t.Fatalf("EncodeJpeg inputs = %#v, want the cropped page", h.encodeInputs)
	}
	if len(h.extractCalls) != 1 || h.extractCalls[0] != result.PdfPath {
		t.Fatalf("extract calls = %#v", h.extractCalls)
	}
}

func TestConvertImageToPdfStillProducesAPdfWhenTheCropStepFails(t *testing.T) {
	h := newHarness(t)
	orientedPage := []byte("oriented-only")
	h.steps.orientFn = func([]byte) imagetopdf.PipelineStepResult {
		return stepResult(1, imagetopdf.LabelOriented, orientedPage)
	}
	h.steps.cropFn = func([]byte) imagetopdf.PipelineStepResult {
		return imagetopdf.PipelineStepResult{Step: 2, Label: imagetopdf.LabelCropped, DurationMS: 1, Error: "crop blew up"}
	}

	result, err := h.converter.ConvertImageToPdf(context.Background(), h.writePhoto("photo.jpg"))
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}
	if _, err := os.Stat(result.PdfPath); err != nil {
		t.Fatalf("PDF missing: %v", err)
	}
	if len(h.encodeInputs) != 1 || !bytes.Equal(h.encodeInputs[0], orientedPage) {
		t.Fatalf("EncodeJpeg inputs = %#v, want the oriented page", h.encodeInputs)
	}
}

func TestConvertImageToPdfStillProducesAPdfWhenOrientationFails(t *testing.T) {
	h := newHarness(t)
	h.steps.orientFn = func([]byte) imagetopdf.PipelineStepResult {
		return imagetopdf.PipelineStepResult{Step: 1, Label: imagetopdf.LabelOriented, DurationMS: 1, Error: "no orientation"}
	}
	h.steps.cropFn = func([]byte) imagetopdf.PipelineStepResult {
		return imagetopdf.PipelineStepResult{Step: 2, Label: imagetopdf.LabelCropped, DurationMS: 1, Error: "no crop"}
	}

	photo := h.writePhoto("photo.jpg")
	result, err := h.converter.ConvertImageToPdf(context.Background(), photo)
	if err != nil {
		t.Fatalf("ConvertImageToPdf: %v", err)
	}
	if _, err := os.Stat(result.PdfPath); err != nil {
		t.Fatalf("PDF missing: %v", err)
	}
	if len(h.encodeInputs) != 1 || !bytes.Equal(h.encodeInputs[0], h.jpeg) {
		t.Fatalf("EncodeJpeg inputs = %#v, want the original photo", h.encodeInputs)
	}
}

func TestConvertImageToPdfPropagatesExtractionFailureBeforeMovingTheSource(t *testing.T) {
	h := newHarness(t)
	h.extractErr = errors.New("PDF2W_SERVICE_URL is not configured")
	photo := h.writePhoto("photo.jpg")

	if _, err := h.converter.ConvertImageToPdf(context.Background(), photo); err == nil || err.Error() != "PDF2W_SERVICE_URL is not configured" {
		t.Fatalf("err = %v, want extraction failure", err)
	}

	// The PDF was already written, but the source is still in __raws for the next scan to retry.
	if _, err := os.Stat(filepath.Join(h.dir, "photo.pdf")); err != nil {
		t.Fatalf("PDF missing: %v", err)
	}
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("source photo was moved despite extraction failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.trashDir(), "photo.jpg")); !os.IsNotExist(err) {
		t.Fatalf("source unexpectedly in trash (err=%v)", err)
	}
}

func TestConvertImageToPdfNeverDeletesThePhotoWhenThePdfCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cannot make a directory unwritable as root")
	}
	h := newHarness(t)
	photo := h.writePhoto("photo.jpg")
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o755) })

	if _, err := h.converter.ConvertImageToPdf(context.Background(), photo); err == nil {
		t.Fatal("expected a write failure")
	}
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("source photo was deleted on write failure: %v", err)
	}
}

func TestFindImageBundleFoldersTreatsAFolderOfTwoOrMorePhotosAsOneDocument(t *testing.T) {
	h := newHarness(t)
	dir := h.folderWith("contrat-bail", map[string][]byte{"IMG_1.jpg": h.jpeg, "IMG_2.jpg": h.jpeg})

	found := FindImageBundleFolders(h.dir)
	if !reflect.DeepEqual(found, []string{dir}) {
		t.Fatalf("found = %#v, want [%s]", found, dir)
	}
}

func TestFindImageBundleFoldersDoesNotBundleAFolderHoldingASinglePhoto(t *testing.T) {
	h := newHarness(t)
	h.folderWith("solo", map[string][]byte{"IMG_1.jpg": h.jpeg})

	if found := FindImageBundleFolders(h.dir); len(found) != 0 {
		t.Fatalf("found = %#v, want []", found)
	}
}

func TestFindImageBundleFoldersDoesNotBundleAFolderMixingPhotosWithOtherFiles(t *testing.T) {
	h := newHarness(t)
	h.folderWith("mixed", map[string][]byte{"IMG_1.jpg": h.jpeg, "IMG_2.jpg": h.jpeg, "notes.pdf": []byte("x")})

	if found := FindImageBundleFolders(h.dir); len(found) != 0 {
		t.Fatalf("found = %#v, want []", found)
	}
}

func TestFindImageBundleFoldersIgnoresDotDirectories(t *testing.T) {
	h := newHarness(t)
	h.folderWith(".delete_files", map[string][]byte{"IMG_1.jpg": h.jpeg, "IMG_2.jpg": h.jpeg})

	if found := FindImageBundleFolders(h.dir); len(found) != 0 {
		t.Fatalf("found = %#v, want []", found)
	}
}

func TestFindImageBundleFoldersIgnoresOSJunkFilesWhenDeciding(t *testing.T) {
	h := newHarness(t)
	dir := h.folderWith("scan", map[string][]byte{"a.jpg": h.jpeg, "b.jpg": h.jpeg, "Thumbs.db": []byte("junk")})

	found := FindImageBundleFolders(h.dir)
	if !reflect.DeepEqual(found, []string{dir}) {
		t.Fatalf("found = %#v, want [%s]", found, dir)
	}
}

func TestFindImageBundleFoldersNeverTreatsTheRootItselfAsABundle(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.dir, "a.jpg"), h.jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "b.jpg"), h.jpeg, 0o644); err != nil {
		t.Fatal(err)
	}

	if found := FindImageBundleFolders(h.dir); len(found) != 0 {
		t.Fatalf("found = %#v, want []", found)
	}
}

func TestSortImagePagesNaturallyOrdersPage2BeforePage10(t *testing.T) {
	got := SortImagePagesNaturally([]string{"IMG_10.jpg", "IMG_2.jpg", "IMG_1.jpg"})
	want := []string{"IMG_1.jpg", "IMG_2.jpg", "IMG_10.jpg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %#v, want %#v", got, want)
	}
}

func TestListBundleImagesReturnsOnlyImagesInPageOrder(t *testing.T) {
	h := newHarness(t)
	dir := h.folderWith("bundle", map[string][]byte{
		"IMG_10.jpg": h.jpeg,
		"IMG_2.jpg":  h.jpeg,
		"notes.txt":  []byte("nope"),
	})

	got := ListBundleImages(dir)
	want := []string{filepath.Join(dir, "IMG_2.jpg"), filepath.Join(dir, "IMG_10.jpg")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got = %#v, want %#v", got, want)
	}
}

func TestConvertImageFolderToPdfProducesOnePdfNamedAfterTheFolder(t *testing.T) {
	h := newHarness(t)
	dir := h.bundle("contrat-bail", []string{"IMG_1.jpg", "IMG_2.jpg", "IMG_3.jpg"})

	result, err := h.converter.ConvertImageFolderToPdf(context.Background(), dir)
	if err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}

	if result.PdfPath != filepath.Join(h.dir, "contrat-bail.pdf") {
		t.Fatalf("PdfPath = %q", result.PdfPath)
	}
	if result.PageCount != 3 {
		t.Fatalf("PageCount = %d, want 3", result.PageCount)
	}
	hasPDFHeader(t, result.PdfPath)
}

func TestConvertImageFolderToPdfGetsTheWholeBundleTextFromTheAssembledPdf(t *testing.T) {
	h := newHarness(t)
	h.extractResult.RawText = "page 1 text\n\n---\n\npage 2 text"

	result, err := h.converter.ConvertImageFolderToPdf(context.Background(), h.bundle("facture", []string{"a.jpg", "b.jpg"}))
	if err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}

	if result.RawText != "page 1 text\n\n---\n\npage 2 text" {
		t.Fatalf("RawText = %q", result.RawText)
	}
	if len(h.extractCalls) != 1 || h.extractCalls[0] != result.PdfPath {
		t.Fatalf("extract calls = %#v", h.extractCalls)
	}
}

func TestConvertImageFolderToPdfKeepsTheSourcePagesTogetherUnderTheFolderSubfolder(t *testing.T) {
	h := newHarness(t)
	dir := h.bundle("releve", []string{"p1.jpg", "p2.jpg"})

	if _, err := h.converter.ConvertImageFolderToPdf(context.Background(), dir); err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}

	kept := filepath.Join(h.trashDir(), "releve")
	for _, name := range []string{"p1.jpg", "p2.jpg"} {
		if _, err := os.Stat(filepath.Join(kept, name)); err != nil {
			t.Fatalf("%s missing under %s: %v", name, kept, err)
		}
	}
}

func TestConvertImageFolderToPdfRemovesTheFolderOnceItsPagesAreMovedOut(t *testing.T) {
	h := newHarness(t)
	dir := h.bundle("vide", []string{"a.jpg", "b.jpg"})

	if _, err := h.converter.ConvertImageFolderToPdf(context.Background(), dir); err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("folder still present (err=%v)", err)
	}
}

func TestConvertImageFolderToPdfLeavesTheFolderInPlaceIfAnythingElseIsStillInside(t *testing.T) {
	h := newHarness(t)
	dir := h.bundle("reste", []string{"a.jpg", "b.jpg"})
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.converter.ConvertImageFolderToPdf(context.Background(), dir); err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("folder was removed: %v", err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(notes) != "keep me" {
		t.Fatalf("notes.txt = %q, err=%v", notes, err)
	}
}

func TestConvertImageFolderToPdfDoesNotOverwriteAPdfThatOwnsTheFolderName(t *testing.T) {
	h := newHarness(t)
	dir := h.bundle("rapport", []string{"a.jpg", "b.jpg"})
	occupied := filepath.Join(h.dir, "rapport.pdf")
	if err := os.WriteFile(occupied, []byte("%PDF-1.7 pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := h.converter.ConvertImageFolderToPdf(context.Background(), dir)
	if err != nil {
		t.Fatalf("ConvertImageFolderToPdf: %v", err)
	}

	got, _ := os.ReadFile(occupied)
	if string(got) != "%PDF-1.7 pre-existing" {
		t.Fatal("pre-existing PDF was overwritten")
	}
	if result.PdfPath != filepath.Join(h.dir, "rapport_1.pdf") {
		t.Fatalf("PdfPath = %q, want rapport_1.pdf", result.PdfPath)
	}
}

func TestConvertImageFolderToPdfErrorsWhenNoImagesAreFound(t *testing.T) {
	h := newHarness(t)
	empty := filepath.Join(h.dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := h.converter.ConvertImageFolderToPdf(context.Background(), empty)
	if err == nil || !strings.Contains(err.Error(), "No convertible images found") {
		t.Fatalf("err = %v", err)
	}
}
