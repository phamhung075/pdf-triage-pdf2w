package triagescan

// Pipeline coverage beyond the two ported TS cases: the Golden Rule 3 no-text block, the Golden
// Rule 4 forbidden-subcategory block, and the success path (classify -> embed -> insert ->
// relocalize -> update -> FILE_COMPLETED). These pin the app/guards and app/relocalize wiring the
// objective lists, using the same fake + real-SQLite harness as the ported collision case.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
)

// longEnoughText is the same raw text the ported collision case uses; it passes the pre-registration
// quality gate (fewer than 50 tokens, not corrupted, no markdown to lose content from).
const longEnoughText = "Some genuine long enough raw text content here for the test to accept."

func TestRunTriageScanBlocksNoTextFile(t *testing.T) {
	h := newScanHarness(t)

	filePath := filepath.Join(h.inputDir, "empty.pdf")
	if err := os.WriteFile(filePath, []byte("some pdf bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "chk-empty", RawText: "   short   "}

	var events []Event
	result, err := h.scanner.RunTriageScan(context.Background(), func(event Event) {
		events = append(events, event)
	}, nil)
	if err != nil {
		t.Fatalf("RunTriageScan: %v", err)
	}

	if fileExists(filePath) {
		t.Fatalf("no-text file %s was left in __raws", filePath)
	}
	movedPath := filepath.Join(h.inputDir, ".blocked_files", "empty.pdf")
	if !fileExists(movedPath) {
		t.Fatalf("no-text file was not moved to %s", movedPath)
	}
	blocked, err := h.db.GetBlockedFile(movedPath)
	if err != nil {
		t.Fatalf("GetBlockedFile: %v", err)
	}
	if blocked == nil {
		t.Fatalf("no blocked_files row for %s", movedPath)
	}
	if blocked.Reason != "NO_TEXT_EXTRACTED" {
		t.Errorf("blocked reason = %q, want NO_TEXT_EXTRACTED", blocked.Reason)
	}
	if result.ProcessedCount != 0 || result.SkippedCount != 0 {
		t.Errorf("processed=%d skipped=%d, want 0/0", result.ProcessedCount, result.SkippedCount)
	}
	if h.classifier.calls != 0 {
		t.Errorf("classifier called %d times for a no-text file, want 0", h.classifier.calls)
	}
	if !hasEvent(events, EventFileFailed, StageFailed) {
		t.Errorf("no FILE_FAILED/FAILED event; events = %v", events)
	}
}

func TestRunTriageScanBlocksForbiddenSubcategory(t *testing.T) {
	h := newScanHarness(t)

	filePath := filepath.Join(h.inputDir, "junk.pdf")
	if err := os.WriteFile(filePath, []byte("some pdf bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "chk-junk", RawText: longEnoughText}
	h.classifier.fn = func(rawText, filename string) (documentschema.DocumentMetadata, error) {
		return documentschema.DocumentMetadata{
			Titre:        "Junk",
			Categorie:    "invoices",
			Subcategorie: "general",
		}, nil
	}

	var events []Event
	result, err := h.scanner.RunTriageScan(context.Background(), func(event Event) {
		events = append(events, event)
	}, nil)
	if err != nil {
		t.Fatalf("RunTriageScan: %v", err)
	}

	if fileExists(filePath) {
		t.Fatalf("forbidden-subcategory file %s was left in __raws", filePath)
	}
	movedPath := filepath.Join(h.inputDir, ".blocked_files", "junk.pdf")
	blocked, err := h.db.GetBlockedFile(movedPath)
	if err != nil {
		t.Fatalf("GetBlockedFile: %v", err)
	}
	if blocked == nil {
		t.Fatalf("no blocked_files row for %s", movedPath)
	}
	if blocked.Reason != "NO_SUBCATEGORY" {
		t.Errorf("blocked reason = %q, want NO_SUBCATEGORY", blocked.Reason)
	}
	if result.ProcessedCount != 0 || result.SkippedCount != 0 {
		t.Errorf("processed=%d skipped=%d, want 0/0", result.ProcessedCount, result.SkippedCount)
	}
	if h.ollama.embedCalls != 0 {
		t.Errorf("embedding called %d times for a blocked file, want 0", h.ollama.embedCalls)
	}
	if !hasEvent(events, EventFileFailed, StageFailed) {
		t.Errorf("no FILE_FAILED/FAILED event; events = %v", events)
	}
}

func TestRunTriageScanHappyPathMovesFile(t *testing.T) {
	h := newScanHarness(t)

	filePath := filepath.Join(h.inputDir, "facture.pdf")
	if err := os.WriteFile(filePath, []byte("some pdf bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "chk-happy", RawText: longEnoughText}
	h.classifier.fn = func(rawText, filename string) (documentschema.DocumentMetadata, error) {
		return documentschema.DocumentMetadata{
			Titre:        "Facture SFR janvier",
			Categorie:    "invoices",
			Subcategorie: "sfr",
			Date:         "2026-01-15",
		}, nil
	}
	finalPath := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "Facture SFR janvier.pdf")
	h.relocalizer.result = relocalize.RelocalizeResult{NewPath: finalPath, Moved: true}

	var events []Event
	result, err := h.scanner.RunTriageScan(context.Background(), func(event Event) {
		events = append(events, event)
	}, nil)
	if err != nil {
		t.Fatalf("RunTriageScan: %v", err)
	}

	if result.ScannedCount != 1 || result.ProcessedCount != 1 || result.SkippedCount != 0 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/0", result.ScannedCount, result.ProcessedCount, result.SkippedCount)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "MOVED" {
		t.Fatalf("items = %+v, want one MOVED item", result.Items)
	}
	if result.Items[0].NewPath != finalPath {
		t.Errorf("item newPath = %q, want %q", result.Items[0].NewPath, finalPath)
	}
	if h.relocalizer.calls != 1 {
		t.Fatalf("relocalizer calls = %d, want 1", h.relocalizer.calls)
	}
	if h.relocalizer.lastCategory != "invoices" || h.relocalizer.lastSubcategory != "sfr" {
		t.Errorf("relocalized with %q/%q, want invoices/sfr", h.relocalizer.lastCategory, h.relocalizer.lastSubcategory)
	}

	docs, err := h.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1", len(docs))
	}
	if docs[0].Status != "MOVED" || docs[0].NewPath != finalPath {
		t.Errorf("stored document status=%q new_path=%q, want MOVED/%q", docs[0].Status, docs[0].NewPath, finalPath)
	}
	if !hasEvent(events, EventFileCompleted, StageCompleted) {
		t.Errorf("no FILE_COMPLETED/COMPLETED event; events = %v", events)
	}
}

func hasEvent(events []Event, eventType EventType, stage Stage) bool {
	for _, event := range events {
		if event.Type == eventType && event.Stage == stage {
			return true
		}
	}
	return false
}
