// Package convertimage is a Go port of pdf-triage's src/application/convert-image-document.ts (379
// lines): the photo -> archivable A4 PDF pipeline and the folder-bundling rules around it.
//
// # Provenance
//
//	ConvertImageToPdf            -> src/application/convert-image-document.ts:264
//	ConvertImageFolderToPdf      -> src/application/convert-image-document.ts:323
//	FindImageBundleFolders       -> src/application/convert-image-document.ts:234
//	IsImageFile                  -> src/application/convert-image-document.ts:33
//	SortImagePagesNaturally      -> src/application/convert-image-document.ts:201
//	ListBundleImages             -> src/application/convert-image-document.ts:209
//	BuildPdfFromPages            -> src/application/convert-image-document.ts:160
//	WritePdfWithoutOverwriting   -> src/application/convert-image-document.ts:181
//	MoveConvertedSourceToTrash   -> src/application/convert-image-document.ts:84
//	ConvertedImageDocument       -> src/application/convert-image-document.ts:37-49
//
// The WHY-comments from the TS source are preserved verbatim at the declarations they explain.
//
// # PDF assembly: a minimal DCT-encoded writer, not pdfcpu's importer
//
// Each JPEG page is placed with the exact geometry pdfpagefit.FitImageToA4 computes: the page is A4
// portrait (595.28 x 841.89) for a portrait photo, A4 landscape (841.89 x 595.28) for a landscape
// one, and the image is scaled to fit, centred, never cropped or stretched. pdfcpu's
// api.ImportImages cannot express that: its Import carries ONE PageDim for every page of a call, so
// it cannot pick the page orientation per photo; and when Pos != types.Full its content stream
// scales the image to a Scale x page rectangle derived from the PAGE aspect ratio, not the image's
// (see pdfcpu's importImagePDFBytes), so it also does not preserve the photo's aspect ratio. When
// Pos == types.Full it makes the MediaBox the image's pixel dimensions instead of A4. This package
// therefore writes the smallest correct PDF by hand: one /DCTDecode image XObject per page (the JPEG
// bytes are embedded untouched, exactly as pdf-lib embedJpg did) and a content stream that draws it
// at the FitImageToA4 rectangle. github.com/pdfcpu/pdfcpu is still the verification tool: the tests
// re-read every produced PDF and assert page count, MediaBox and image XObject dimensions.
//
// # Golden Rule 17 invariants (all covered by tests)
//
//   - The archived page is the CROPPED, natural-tone image; the enhanced buffer is produced (and its
//     failures reported) but never encoded or archived.
//   - The source photo is MOVED to __raws/.delete_files/img_converted/ (never deleted), with a
//     _1, _2 … collision suffix, up to 20 attempts, and a per-folder subfolder for bundles.
//   - The PDF is claimed with 'wx' (exclusive create) BEFORE the source is touched, and never
//     overwrites an unrelated file.
//   - Conversion failure is never destructive: a failed write or a failed extraction leaves the
//     source exactly where it was, for the next scan to retry.
//   - Pages sort numerically (IMG_2 before IMG_10).
//   - A folder holding only photos (2+) becomes ONE multi-page PDF named after the folder; a lone
//     photo or a folder mixing photos with anything else is triaged file-by-file.
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. Injection. TS imported runOrientStep/runCropStep/runEnhanceStep and extractPDFContent
//     directly; its test mocked those modules. Go takes them as Deps fields (Steps, Extract,
//     EncodeJpeg, ForDocument) and the test passes fakes. Deps.InputDir is TS CONFIG.INPUT_DIR.
//  2. Logger. TS `logger.forDocument(name)` returns a child logger with Info/Warn; Go's
//     Deps.ForDocument returns the DocumentLogger interface. *logger.DocumentLogger satisfies it,
//     so production closes over *logger.Logger.ForDocument.
//  3. Sort. JS `localeCompare(..., {numeric: true, sensitivity: 'base'})` has no stdlib Go
//     equivalent; SortImagePagesNaturally implements the same observable rule for page ordering:
//     digit runs compare numerically, everything else case-insensitively. Full locale collation
//     (accents, language-specific tie-breaks) is out of scope — page stems are ASCII camera names.
//  4. Errors. TS throws; Go returns errors with the same text.
package convertimage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
)

// Extensions the triage scanner accepts that are photographs rather than documents. Kept in sync
// with the image branch of pdf-extractor.ts, which remains the fallback when conversion fails.
// (convert-image-document.ts:12-13, preserved verbatim.)
var imageExtensions = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".bmp":  true,
	".tiff": true,
}

// A photograph, not a diagram: JPEG at this quality is visually indistinguishable from the source at
// a fraction of a full-resolution PNG. Note @napi-rs/canvas takes 0-100 here, not 0-1.
// (convert-image-document.ts:16-18, preserved verbatim.)
const ArchiveJpegQuality = 85

// Bound on the _1, _2, … search for a free name, both for the generated PDF beside the source (the
// 'wx' write) and for the source's resting place in the trash folder. A stem that collides 20 times
// over is a pathological directory, not a case worth silently churning through.
// (convert-image-document.ts:20-23, preserved verbatim.)
const MaxPdfNameAttempts = 20

// ConvertedSourceTrash is where a source photograph goes once its PDF is safely on disk. It is kept,
// not deleted: the archived PDF holds an oriented, CROPPED, JPEG-re-encoded rendition, so if the
// crop detector clipped part of the page the original is the only way back — and orientation/OCR
// failures degrade gracefully while a bad crop does not. `.delete_files` is this project's existing
// trash convention (settings.ts creates it) and pdf-scanner.ts skips every dot-directory, so nothing
// parked here is ever re-triaged. It grows without bound; prune it periodically.
// (convert-image-document.ts:25-31, preserved verbatim.)
var ConvertedSourceTrash = []string{".delete_files", "img_converted"}

// ConvertedImageDocument mirrors the TS `ConvertedImageDocument` interface
// (convert-image-document.ts:37-49).
type ConvertedImageDocument struct {
	PdfPath   string
	Checksum  string
	RawText   string
	PageCount int
	// SourceImagePath is where the source photo(s) were parked under .delete_files/img_converted,
	// so the document can be traced back to the image it was made from and re-edited later. Empty
	// when the move failed — the PDF is still valid, there is just nothing to point at.
	SourceImagePath string
}

// StepRunner is the three-step seam convertimage calls. *imagetopdf.Stepper satisfies it, and tests
// pass a fake (TS mocked the whole image-to-pdf module).
type StepRunner interface {
	RunOrientStep(ctx context.Context, imageBuffer []byte) imagetopdf.PipelineStepResult
	RunCropStep(ctx context.Context, orientedBuffer []byte) imagetopdf.PipelineStepResult
	RunEnhanceStep(ctx context.Context, croppedBuffer []byte) imagetopdf.PipelineStepResult
}

// DocumentLogger is the subset of the per-document logger convertimage uses. *logger.DocumentLogger
// satisfies it; nil skips logging.
type DocumentLogger interface {
	Info(moduleName, message string, meta any)
	Warn(moduleName, message string, meta any)
}

// Deps is the injected environment. InputDir (TS CONFIG.INPUT_DIR) is where .delete_files lives.
// Steps, Extract and EncodeJpeg are required; ForDocument and Now may be nil.
type Deps struct {
	InputDir    string
	Steps       StepRunner
	Extract     func(pdfPath string) (pdfextractor.ExtractedPDF, error)
	EncodeJpeg  func(imageBuffer []byte, quality int) ([]byte, error)
	ForDocument func(filename string) DocumentLogger
	Now         func() time.Time
}

// Converter runs the photo -> PDF pipeline with one injected Deps set.
type Converter struct {
	deps Deps
}

// NewConverter returns a Converter. A nil Now defaults to time.Now.
func NewConverter(deps Deps) *Converter {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Converter{deps: deps}
}

// IsImageFile ports isImageFile, case-insensitively.
func IsImageFile(filePath string) bool {
	return imageExtensions[strings.ToLower(filepath.Ext(filePath))]
}

// ConvertImageToPdf ports convertImageToPdf.
//
// The WHY-comments below are preserved verbatim from the TS source
// (convert-image-document.ts:55-75, :76-83, :171-180).
//
// Turns one photographed document into the PDF that gets archived in its place, and returns the
// extracted text alongside it.
//
// TEXT COMES FROM pdf2w AFTER ASSEMBLY. The pipeline is orient -> crop -> enhance -> assemble the
// image-only A4 PDF, then that fresh PDF is handed to extractPDFContent(), the same required pdf2w
// path every other PDF takes. There is no local OCR any more: pdf2w finds no text layer in the
// assembled image-only PDF and runs its own vision-rescue. This costs one extra HTTP round-trip per
// photo (accepted by docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md) and means
// photos and scanned PDFs get their text from exactly one place.
//
// WHICH IMAGE GOES ON THE PAGE. The CROPPED one, not the enhanced one. Enhancement exists to make
// glyphs separable for a reader (it pushes contrast hard and sharpens), and that treatment can crush
// faint stamps and signatures — fine for a machine that is about to throw the pixels away, wrong for
// the copy of a document being kept for years. Only the archived page is the natural-toned one.
//
// FAILURE IS NEVER DESTRUCTIVE. Each stage degrades to the best buffer produced so far, so a failed
// crop still yields an upright PDF and a failed orientation still yields the original photo as a
// PDF. If the PDF cannot be written at all this throws and the original file is left exactly where
// it was, for the caller to fall back on. The source image is moved to the trash only after the PDF
// is confirmed on disk AND its text has been extracted.
//
//	// Claims a .pdf name with the 'wx' flag (exclusive create), never a plain write.
//	//
//	// An unrelated PDF can already own the name. Dropping both `contrat.jpg` (a photo of page 2) and
//	// `contrat.pdf` (the signed contract) into __raws puts them in the same scan batch, and the image
//	// is processed first — a plain writeFileSync would replace the contract's bytes with the photo,
//	// with no copy in .duplicates_files, .delete_files or __archive to recover from. 'wx' fails with
//	// EEXIST instead and we fall back to a suffixed name, mirroring renameAtomicNoOverwrite() in
//	// relocalize-document.ts. The check IS the write, so there is no TOCTOU window.
func (c *Converter) ConvertImageToPdf(ctx context.Context, imagePath string) (ConvertedImageDocument, error) {
	filename := filepath.Base(imagePath)
	docLog := c.documentLogger(filename)

	pageJpeg, err := c.renderPageFromImage(ctx, imagePath, docLog)
	if err != nil {
		return ConvertedImageDocument{}, err
	}
	pdfBytes, err := BuildPdfFromPages([][]byte{pageJpeg})
	if err != nil {
		return ConvertedImageDocument{}, err
	}

	// The PDF is written BEFORE the source is touched — the reverse order would lose the document if
	// the write failed. (convert-image-document.ts:271-272, preserved verbatim.)
	pdfPath, err := WritePdfWithoutOverwriting(filepath.Dir(imagePath), baseNameWithoutExt(imagePath), pdfBytes, imagePath)
	if err != nil {
		return ConvertedImageDocument{}, err
	}

	// The text comes from the required pdf2w service, on the assembled image-only PDF — the same path
	// every other PDF takes. A failure here is a hard error (no local OCR fallback) and is raised
	// before the source is moved, so the photo is still in place for the next scan to retry.
	// (convert-image-document.ts:280-283, preserved verbatim.)
	extracted, err := c.deps.Extract(pdfPath)
	if err != nil {
		return ConvertedImageDocument{}, err
	}
	rawText := extracted.RawText

	sourceImagePath := ""
	moved, moveErr := MoveConvertedSourceToTrash(c.deps.InputDir, imagePath, "")
	if moveErr != nil {
		// The PDF exists, so the document is safe and triage can proceed. A source image left in
		// place would otherwise be picked up and converted again on the next scan tick, so this is
		// worth shouting about even though it is not fatal to this document.
		// (convert-image-document.ts:289-294, preserved verbatim.)
		c.warn(docLog, "IMG2PDF", fmt.Sprintf("Converted to PDF but could not move the source image out of __raws: %s", moveErr.Error()), map[string]any{"imagePath": imagePath})
	} else {
		sourceImagePath = moved
		c.info(docLog, "IMG2PDF", fmt.Sprintf("Source photo kept in %s", strings.Join(ConvertedSourceTrash, "/")), map[string]any{"imagePath": imagePath, "trashedPath": sourceImagePath})
	}

	// Checksum the PDF, not the photo: the PDF is the artifact that gets archived and de-duplicated,
	// so a checksum taken from the discarded source would never match a re-scan of the archive.
	// (convert-image-document.ts:296-297, preserved verbatim.)
	checksum := checksumHex(pdfBytes)

	c.info(docLog, "IMG2PDF", "Converted photo to archivable PDF", map[string]any{
		"filename":  filename,
		"pdfPath":   pdfPath,
		"pdfBytes":  len(pdfBytes),
		"textChars": len(rawText),
	})

	return ConvertedImageDocument{
		PdfPath:         pdfPath,
		Checksum:        checksum,
		RawText:         rawText,
		PageCount:       1,
		SourceImagePath: sourceImagePath,
	}, nil
}

// ConvertImageFolderToPdf ports convertImageFolderToPdf.
//
// The WHY-comment below is preserved verbatim from the TS source
// (convert-image-document.ts:310-322).
//
// Bundles a folder of photographs in __raws into ONE multi-page PDF.
//
// A folder is how a phone photo batch of a multi-page document actually arrives, so
// `__raws/contrat-bail/` holding three photos becomes a single three-page document rather than three
// unrelated one-page ones. Pages follow sortImagePagesNaturally() (numeric-aware), the folder name
// becomes the PDF name, and the assembled bundle PDF is handed to extractPDFContent() (pdf2w) so the
// classifier reads the whole document at once.
//
// Ordering matches the single-photo path: the PDF is written first, and only then are the sources
// moved to .delete_files/img_converted/<folder>/ — keeping the pages grouped there too, so a bad
// crop on page 2 is recoverable without guessing which loose photo it was.
func (c *Converter) ConvertImageFolderToPdf(ctx context.Context, folderPath string) (ConvertedImageDocument, error) {
	folderName := filepath.Base(folderPath)
	docLog := c.documentLogger(folderName)

	imagePaths := ListBundleImages(folderPath)
	if len(imagePaths) == 0 {
		return ConvertedImageDocument{}, fmt.Errorf("No convertible images found in %s", folderPath)
	}

	pages := make([][]byte, 0, len(imagePaths))
	for _, imagePath := range imagePaths {
		page, err := c.renderPageFromImage(ctx, imagePath, docLog)
		if err != nil {
			return ConvertedImageDocument{}, err
		}
		pages = append(pages, page)
	}

	pdfBytes, err := BuildPdfFromPages(pages)
	if err != nil {
		return ConvertedImageDocument{}, err
	}
	pdfPath, err := WritePdfWithoutOverwriting(filepath.Dir(folderPath), folderName, pdfBytes, folderPath)
	if err != nil {
		return ConvertedImageDocument{}, err
	}

	// Same as the single-photo path: the assembled bundle PDF is handed to pdf2w for its text.
	// (convert-image-document.ts:340-341, preserved verbatim.)
	extracted, err := c.deps.Extract(pdfPath)
	if err != nil {
		return ConvertedImageDocument{}, err
	}
	rawText := extracted.RawText

	// For a bundle the whole folder is the source, so record the directory rather than one page.
	// (convert-image-document.ts:343-344, preserved verbatim.)
	sourceImagePath := ""
	for _, imagePath := range imagePaths {
		moved, moveErr := MoveConvertedSourceToTrash(c.deps.InputDir, imagePath, folderName)
		if moveErr != nil {
			c.warn(docLog, "IMG2PDF", fmt.Sprintf("Bundled to PDF but could not move a source page out of __raws: %s", moveErr.Error()), map[string]any{"imagePath": imagePath})
			continue
		}
		if sourceImagePath == "" {
			sourceImagePath = filepath.Dir(moved)
		}
	}

	// Remove the folder only once it is genuinely empty — anything left behind (a stray .txt, a page
	// whose move failed) means the user still has something there, and deleting it would be exactly
	// the data loss the rest of this module exists to prevent.
	// (convert-image-document.ts:354-356, preserved verbatim.)
	if entries, readErr := os.ReadDir(folderPath); readErr != nil {
		c.warn(docLog, "IMG2PDF", fmt.Sprintf("Could not remove the bundled folder: %s", readErr.Error()), map[string]any{"folderPath": folderPath})
	} else if len(entries) == 0 {
		if removeErr := os.Remove(folderPath); removeErr != nil {
			c.warn(docLog, "IMG2PDF", fmt.Sprintf("Could not remove the bundled folder: %s", removeErr.Error()), map[string]any{"folderPath": folderPath})
		}
	} else {
		c.warn(docLog, "IMG2PDF", "Bundled folder still has files left in it; leaving it in place", map[string]any{"folderPath": folderPath})
	}

	checksum := checksumHex(pdfBytes)

	c.info(docLog, "IMG2PDF", fmt.Sprintf("Bundled %d photos into one archivable PDF", len(pages)), map[string]any{
		"folderPath": folderPath,
		"pdfPath":    pdfPath,
		"pageCount":  len(pages),
		"pdfBytes":   len(pdfBytes),
		"textChars":  len(rawText),
	})

	return ConvertedImageDocument{
		PdfPath:         pdfPath,
		Checksum:        checksum,
		RawText:         rawText,
		PageCount:       len(pages),
		SourceImagePath: sourceImagePath,
	}, nil
}

// renderPageFromImage ports renderPageFromImage. The WHY-comment below is preserved verbatim from
// the TS source (convert-image-document.ts:115-124).
//
// Runs one photograph through the orientation, crop and enhancement stages and returns the page
// image. Text is no longer produced here — the caller hands the assembled PDF to
// extractPDFContent() once it is on disk.
//
// Every stage degrades gracefully: orientation, crop and enhancement each either improve the buffer
// or leave it untouched, so there is always something publishable no matter how far the pipeline
// gets. The returned JPEG is what gets archived (the CROPPED page, natural tones); the enhanced
// buffer is never archived.
func (c *Converter) renderPageFromImage(ctx context.Context, imagePath string, docLog DocumentLogger) ([]byte, error) {
	filename := filepath.Base(imagePath)
	imageBuffer, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, err
	}

	pageBuffer := imageBuffer

	oriented := c.deps.Steps.RunOrientStep(ctx, imageBuffer)
	if oriented.Error != "" || oriented.ImageBase64 == "" {
		c.warn(docLog, "IMG2PDF", fmt.Sprintf("Orientation failed, using the photo as-is: %s", oriented.Error), map[string]any{"filename": filename})
	} else {
		// Buffer.from(base64) is lenient: a malformed string yields the bytes it could decode rather
		// than throwing. Mirror that and keep whatever partial decode came back.
		pageBuffer, _ = base64.StdEncoding.DecodeString(oriented.ImageBase64)
	}

	cropped := c.deps.Steps.RunCropStep(ctx, pageBuffer)
	if cropped.Error != "" || cropped.ImageBase64 == "" {
		c.warn(docLog, "IMG2PDF", fmt.Sprintf("Crop failed, keeping the uncropped page: %s", cropped.Error), map[string]any{"filename": filename})
	} else {
		pageBuffer, _ = base64.StdEncoding.DecodeString(cropped.ImageBase64)
	}

	// Enhancement remains a pipeline stage (Golden Rule 17 governs the geometry stages), but its
	// output is no longer read by anything. Keep reporting its failures so a broken canvas path stays
	// visible. (convert-image-document.ts:148-154, preserved verbatim.)
	enhanced := c.deps.Steps.RunEnhanceStep(ctx, pageBuffer)
	if enhanced.Error != "" || enhanced.ImageBase64 == "" {
		c.warn(docLog, "IMG2PDF", fmt.Sprintf("Enhancement failed: %s", enhanced.Error), map[string]any{"filename": filename})
	}

	return c.deps.EncodeJpeg(pageBuffer, ArchiveJpegQuality)
}

// FindImageBundleFolders ports findImageBundleFolders. The WHY-comment below is preserved verbatim
// from the TS source (convert-image-document.ts:221-233).
//
// Finds folders under __raws whose contents are a single multi-page document.
//
// A folder qualifies only when it holds TWO OR MORE images and no other kind of file. Both halves
// matter:
//   - One image is not a bundle; it takes the ordinary single-photo path and keeps its own name.
//   - A folder mixing photos with a PDF (or anything else) is an ordinary folder the user is using
//     for storage, not a document. Silently fusing its photos into one PDF would be a destructive
//     guess, so it is left alone and each file is triaged individually as before.
//
// Dot-directories are skipped — that is where .blocked_files, .duplicates_files and the converted
// sources in .delete_files live, and none of them are incoming work.
func FindImageBundleFolders(rootDir string) []string {
	found := []string{}

	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}

		files := make([]os.DirEntry, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || name == "Thumbs.db" || name == ".DS_Store" || name == "desktop.ini" {
				continue
			}
			files = append(files, entry)
		}
		images := make([]os.DirEntry, 0, len(files))
		for _, file := range files {
			if IsImageFile(file.Name()) {
				images = append(images, file)
			}
		}

		if dir != rootDir && len(images) >= 2 && len(images) == len(files) {
			found = append(found, dir)
			return // the whole folder is this one document; do not descend into it
		}

		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				walk(filepath.Join(dir, entry.Name()))
			}
		}
	}

	walk(rootDir)
	return found
}

// ListBundleImages ports listBundleImages. The comment below is preserved verbatim from the TS
// source (convert-image-document.ts:205-208).
//
// The images inside a bundle folder, in page order. Non-image files are ignored here; whether the
// folder qualifies as a bundle at all is decided by findImageBundleFolders().
func ListBundleImages(folderPath string) []string {
	entries, err := os.ReadDir(folderPath)
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && IsImageFile(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sorted := SortImagePagesNaturally(names)
	paths := make([]string, 0, len(sorted))
	for _, name := range sorted {
		paths = append(paths, filepath.Join(folderPath, name))
	}
	return paths
}

// SortImagePagesNaturally ports sortImagePagesNaturally. The WHY-comment below is preserved
// verbatim from the TS source (convert-image-document.ts:197-200).
//
// Page order for a bundled folder: numeric-aware, so IMG_2 sorts before IMG_10 rather than after.
// Plain lexicographic ordering silently shuffles the pages of any document with 10+ photos.
func SortImagePagesNaturally(filenames []string) []string {
	out := make([]string, len(filenames))
	copy(out, filenames)
	sortStable(out, func(a, b string) int { return naturalCompare(a, b) })
	return out
}

// MoveConvertedSourceToTrash ports moveConvertedSourceToTrash. The WHY-comment below is preserved
// verbatim from the TS source (convert-image-document.ts:76-83 and :102-107).
//
// Moves a converted source photograph into __raws/.delete_files/img_converted/.
//
// Called only after the PDF is on disk, so the document is never at risk. Returns the resting path.
// Collisions get a _1, _2 … suffix rather than overwriting, the same way
// moveBlockedFileToBlockedFolder does — two photos named IMG_0001.jpg from different cameras are
// ordinary, and silently clobbering one would defeat the point of keeping them.
//
// groupName is the TS optional parameter: "" means no subfolder (a single photo); a bundle passes
// the folder name so its pages stay grouped.
//
//	// EXDEV: __raws can sit on a different volume from the temp/source location. Copy-then-unlink is
//	// the standard fallback, and the copy is verified by unlink only running on success.
func MoveConvertedSourceToTrash(inputDir, imagePath, groupName string) (string, error) {
	trashParts := append([]string{inputDir}, ConvertedSourceTrash...)
	trashDir := filepath.Join(trashParts...)
	if groupName != "" {
		trashDir = filepath.Join(trashDir, groupName)
	}
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", err
	}

	file := filepath.Base(imagePath)
	ext := filepath.Ext(file)
	base := strings.TrimSuffix(file, ext)

	target := filepath.Join(trashDir, file)
	for attempt := 1; fileExists(target); attempt++ {
		if attempt > MaxPdfNameAttempts {
			return "", fmt.Errorf("Could not find a free name for %s in %s after %d attempts", file, trashDir, MaxPdfNameAttempts)
		}
		target = filepath.Join(trashDir, fmt.Sprintf("%s_%d%s", base, attempt, ext))
	}

	if err := os.Rename(imagePath, target); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return "", err
		}
		if err := copyFile(imagePath, target); err != nil {
			return "", err
		}
		if err := os.Remove(imagePath); err != nil {
			return "", err
		}
	}

	return target, nil
}

// WritePdfWithoutOverwriting ports writePdfWithoutOverwriting. The WHY-comment below is preserved
// verbatim from the TS source (convert-image-document.ts:171-179) and is repeated on
// ConvertImageToPdf, which is where the TS comment sits.
//
// Unlike the TS loop, which probes by attempting the write, this returns the error from a failed
// non-EEXIST create (a disk-full or permission error is never converted into a suffix search).
func WritePdfWithoutOverwriting(dir, base string, bytes []byte, context string) (string, error) {
	pdfPath := filepath.Join(dir, base+".pdf")
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(pdfPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, writeErr := f.Write(bytes)
			closeErr := f.Close()
			if writeErr != nil {
				return "", writeErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			return pdfPath, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if attempt >= MaxPdfNameAttempts {
			return "", fmt.Errorf("Could not find a free .pdf name for %s after %d attempts", context, MaxPdfNameAttempts)
		}
		pdfPath = filepath.Join(dir, fmt.Sprintf("%s_%d.pdf", base, attempt+1))
	}
}

func (c *Converter) documentLogger(filename string) DocumentLogger {
	if c.deps.ForDocument == nil {
		return nil
	}
	return c.deps.ForDocument(filename)
}

func (c *Converter) info(log DocumentLogger, moduleName, message string, meta any) {
	if log != nil {
		log.Info(moduleName, message, meta)
	}
}

func (c *Converter) warn(log DocumentLogger, moduleName, message string, meta any) {
	if log != nil {
		log.Warn(moduleName, message, meta)
	}
}

func baseNameWithoutExt(p string) string {
	file := filepath.Base(p)
	return strings.TrimSuffix(file, filepath.Ext(file))
}

func checksumHex(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// naturalCompare is the numeric-aware, case-insensitive comparator behind
// SortImagePagesNaturally. It tokenizes on digit/non-digit boundaries and compares digit runs by
// numeric value (with leading zeros breaking ties deterministically) and everything else by
// lower-cased rune order.
func naturalCompare(a, b string) int {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		aDigit := isASCIIDigit(a[ai])
		bDigit := isASCIIDigit(b[bi])
		switch {
		case aDigit && bDigit:
			aStart, bStart := ai, bi
			for ai < len(a) && isASCIIDigit(a[ai]) {
				ai++
			}
			for bi < len(b) && isASCIIDigit(b[bi]) {
				bi++
			}
			aRun, bRun := a[aStart:ai], b[bStart:bi]
			if cmp := compareNumericRuns(aRun, bRun); cmp != 0 {
				return cmp
			}
		case aDigit != bDigit:
			// A missing digit run sorts before a present one, matching localeCompare's treatment of
			// "a" vs "a1".
			if aDigit {
				return -1
			}
			return 1
		default:
			ar, aSize := nextRune(a[ai:])
			br, bSize := nextRune(b[bi:])
			if cmp := compareFoldedRunes(ar, br); cmp != 0 {
				return cmp
			}
			ai += aSize
			bi += bSize
		}
	}
	switch {
	case ai < len(a):
		return 1
	case bi < len(b):
		return -1
	default:
		return 0
	}
}

func compareNumericRuns(a, b string) int {
	aTrim := strings.TrimLeft(a, "0")
	bTrim := strings.TrimLeft(b, "0")
	switch {
	case len(aTrim) != len(bTrim):
		if len(aTrim) < len(bTrim) {
			return -1
		}
		return 1
	case aTrim != bTrim:
		if aTrim < bTrim {
			return -1
		}
		return 1
	}
	// Equal value: fewer leading zeros first, so "1" sorts before "01" deterministically.
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	default:
		return 0
	}
}

func compareFoldedRunes(a, b rune) int {
	af := unicode.ToLower(a)
	bf := unicode.ToLower(b)
	switch {
	case af < bf:
		return -1
	case af > bf:
		return 1
	default:
		return 0
	}
}

func nextRune(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	return utf8.DecodeRuneInString(s)
}

func sortStable(items []string, cmp func(a, b string) int) {
	sort.SliceStable(items, func(i, j int) bool { return cmp(items[i], items[j]) < 0 })
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }
