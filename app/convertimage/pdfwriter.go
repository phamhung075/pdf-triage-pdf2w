package convertimage

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"strconv"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/pdfpagefit"
)

// BuildPdfFromPages ports buildPdfFromPages: it assembles one DCT-encoded JPEG page per input, each
// fitted to A4 by pdfpagefit.FitImageToA4.
//
// This is NOT pdfcpu's api.ImportImages. See the package comment ("PDF assembly") for why: pdfcpu's
// importer carries one PageDim for the whole batch (no per-photo portrait/landscape page), does not
// preserve the image's aspect ratio when Pos != types.Full, and makes the MediaBox the pixel
// dimensions when Pos == types.Full. Writing the PDF directly is the only way to reproduce the TS
// pdf-lib geometry exactly. The JPEG bytes are embedded untouched under /Filter /DCTDecode, exactly
// as pdf-lib embedJpg embedded them, so the image XObject dimensions equal the JPEG's pixel
// dimensions and the MediaBox equals the FitImageToA4 page size.
//
// The output is a conventional PDF 1.4 file with a cross-reference table, readable by pdfcpu; the
// tests re-read every produced PDF with pdfcpu and assert the page count, MediaBox and XObject
// dimensions.
func BuildPdfFromPages(pages [][]byte) ([]byte, error) {
	type pageInfo struct {
		jpeg       []byte
		width      int
		height     int
		colorSpace string
		fit        pdfpagefit.PagePlacement
	}

	infos := make([]pageInfo, 0, len(pages))
	for index, jpegBytes := range pages {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpegBytes))
		if err != nil {
			return nil, fmt.Errorf("BuildPdfFromPages: page %d is not a decodable JPEG: %w", index, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			return nil, fmt.Errorf("BuildPdfFromPages: page %d has degenerate dimensions %dx%d", index, cfg.Width, cfg.Height)
		}
		components, err := jpegComponentCount(jpegBytes)
		if err != nil {
			return nil, fmt.Errorf("BuildPdfFromPages: page %d: %w", index, err)
		}
		infos = append(infos, pageInfo{
			jpeg:       jpegBytes,
			width:      cfg.Width,
			height:     cfg.Height,
			colorSpace: colorSpaceForComponents(components),
			fit:        pdfpagefit.FitImageToA4(float64(cfg.Width), float64(cfg.Height)),
		})
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	// A binary comment marks the file as containing binary data, as every real PDF writer does.
	buf.WriteString("%\xE2\xE3\xCF\xD3\n")

	totalObjects := 2 + 3*len(infos)
	offsets := make([]int, totalObjects+1)

	writeObject := func(num int, body []byte) {
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", num)
		buf.Write(body)
		buf.WriteString("\nendobj\n")
	}

	kids := make([]string, 0, len(infos))
	for i := range infos {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+3*i))
	}
	writeObject(1, []byte("<< /Type /Catalog /Pages 2 0 R >>"))
	writeObject(2, []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(infos))))

	for i, info := range infos {
		pageObj := 3 + 3*i
		imageObj := pageObj + 1
		contentObj := pageObj + 2

		pageBody := fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /ProcSet [/PDF /ImageC] /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>",
			formatPDFFloat(info.fit.PageWidth), formatPDFFloat(info.fit.PageHeight), imageObj, contentObj,
		)
		writeObject(pageObj, []byte(pageBody))

		var imageBuf bytes.Buffer
		imageBuf.WriteString("<< /Type /XObject /Subtype /Image ")
		fmt.Fprintf(&imageBuf, "/Width %d /Height %d ", info.width, info.height)
		fmt.Fprintf(&imageBuf, "/ColorSpace /%s /BitsPerComponent 8 ", info.colorSpace)
		if info.colorSpace == "DeviceCMYK" {
			// Adobe CMYK JPEGs store inverted values; this is the conventional Decode array.
			imageBuf.WriteString("/Decode [1 0 1 0 1 0 1 0] ")
		}
		imageBuf.WriteString("/Filter /DCTDecode ")
		fmt.Fprintf(&imageBuf, "/Length %d >>\nstream\n", len(info.jpeg))
		imageBuf.Write(info.jpeg)
		imageBuf.WriteString("\nendstream")
		writeObject(imageObj, imageBuf.Bytes())

		content := fmt.Sprintf("q %s 0 0 %s %s %s cm /Im0 Do Q",
			formatPDFFloat(info.fit.DrawWidth), formatPDFFloat(info.fit.DrawHeight),
			formatPDFFloat(info.fit.X), formatPDFFloat(info.fit.Y))
		var contentBuf bytes.Buffer
		fmt.Fprintf(&contentBuf, "<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
		writeObject(contentObj, contentBuf.Bytes())
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", totalObjects+1)
	buf.WriteString("0000000000 65535 f \n")
	for num := 1; num <= totalObjects; num++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[num])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", totalObjects+1, xrefOffset)

	return buf.Bytes(), nil
}

// formatPDFFloat renders a number with the shortest exact decimal form, so FitImageToA4's 595.28 and
// 841.89 survive verbatim.
func formatPDFFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func colorSpaceForComponents(components int) string {
	switch components {
	case 1:
		return "DeviceGray"
	case 4:
		return "DeviceCMYK"
	default:
		return "DeviceRGB"
	}
}

// jpegComponentCount reads Nf from the JPEG's SOF marker. It is used instead of comparing
// image/jpeg's ColorModel against color.GrayModel/color.CMYKModel because those are ModelFunc
// values, and comparing two func-typed interface values panics.
func jpegComponentCount(data []byte) (int, error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, fmt.Errorf("not a JPEG (missing SOI marker)")
	}
	i := 2
	for i+1 < len(data) {
		if data[i] != 0xFF {
			i++
			continue
		}
		marker := data[i+1]
		i += 2
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			continue // standalone, no length
		case marker == 0xD9 || marker == 0xDA:
			// EOI, or SOS before any SOF: no frame header to read.
			return 0, fmt.Errorf("no SOF marker found")
		}
		if i+1 >= len(data) {
			break
		}
		segmentLength := int(data[i])<<8 | int(data[i+1])
		if segmentLength < 2 || i+segmentLength > len(data) {
			break
		}
		if isSOFMarker(marker) {
			// [length(2)][precision(1)][height(2)][width(2)][Nf(1)]
			if segmentLength >= 8 {
				return int(data[i+7]), nil
			}
			break
		}
		i += segmentLength
	}
	return 0, fmt.Errorf("no SOF marker found")
}

func isSOFMarker(marker byte) bool {
	switch marker {
	case 0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
		return true
	}
	return false
}
