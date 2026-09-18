// System/config routes #7, #8, #10-#13, ported from web-server.ts:239-440.
package httpapi

import (
	"io/fs"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
)

func (s *server) registerSystem(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/server/restart", s.serverRestartHandler)
	mux.HandleFunc("GET /api/triage/status", s.triageStatusHandler)
	mux.HandleFunc("GET /api/config/setup-state", s.setupStateHandler)
	mux.HandleFunc("GET /api/config", s.getConfigHandler)
	mux.HandleFunc("GET /api/system/stats", s.systemStatsHandler)
	mux.HandleFunc("PUT /api/config", s.putConfigHandler)
}

// serverRestartHandler ports web-server.ts:240-247. The response is flushed before the injected exit
// runs (after 500 ms, as the TS setTimeout did).
func (s *server) serverRestartHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"message": "Server restarting..."})
	if s.deps.Exit != nil {
		exit := s.deps.Exit
		delay := s.deps.RestartDelay
		if delay <= 0 {
			delay = 500 * time.Millisecond
		}
		go func() {
			time.Sleep(delay)
			exit(0)
		}()
	}
}

// triageStatusHandler is `res.json(getTaskState())`.
func (s *server) triageStatusHandler(w http.ResponseWriter, r *http.Request) {
	state := s.deps.Tasks.State()
	writeJSON(w, 200, toTaskStateJSON(state))
}

// setupStateHandler ports web-server.ts:288-312.
func (s *server) setupStateHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()
	firstRun := s.deps.Settings.IsFirstRun()

	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Suggested folders for a brand-new install. Deliberately NOT CONFIG.INPUT_DIR /
	// OUTPUT_ROOT_DIR: unconfigured, those resolve under DATA_DIR, which for a packaged app is
	// %APPDATA% — a hidden system folder nobody wants their documents living in.
	documents := filepath.Join(home, "Documents", "Smart PDF Triage")

	inputDir := cfg.InputDir
	outputRoot := cfg.OutputRootDir
	if firstRun {
		inputDir = filepath.Join(documents, "Incoming")
		outputRoot = filepath.Join(documents, "Archive")
	}

	writeJSON(w, 200, map[string]any{
		"configured": !firstRun,
		"dataDir":    s.deps.Settings.DataDir(),
		"defaults": map[string]any{
			"input_dir":       inputDir,
			"output_root_dir": outputRoot,
			"language":        cfg.Language,
			"ollama_host":     cfg.OllamaHost,
			"ollama_model":    cfg.OllamaModel,
		},
	})
}

// getConfigHandler ports web-server.ts:314-327.
func (s *server) getConfigHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()
	writeJSON(w, 200, map[string]any{
		"language":               cfg.Language,
		"input_dir":              cfg.InputDir,
		"output_root_dir":        cfg.OutputRootDir,
		"ollama_model":           cfg.OllamaModel,
		"ollama_host":            cfg.OllamaHost,
		"personal_name_denylist": cfg.PersonalNameDenylist,
	})
}

// putConfigHandler ports web-server.ts:422-440: SystemSettingsSchema.parse then updateConfig.
func (s *server) putConfigHandler(w http.ResponseWriter, r *http.Request) {
	parsed, err := documentschema.ParseSystemSettings(bodyBytes(r))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	patch := settings.UpdateSettings{
		Language:      parsed.Language,
		InputDir:      &parsed.InputDir,
		OutputRootDir: &parsed.OutputRootDir,
		OllamaModel:   &parsed.OllamaModel,
		OllamaHost:    &parsed.OllamaHost,
	}
	// nil means the field was omitted; an explicit [] is a provided empty denylist (TS `if (arr)`).
	if parsed.PersonalNameDenylist != nil {
		patch.PersonalNameDenylist = *parsed.PersonalNameDenylist
	}

	if err := s.deps.Settings.UpdateConfig(patch); err != nil {
		writeError(w, 400, err.Error())
		return
	}

	cfg := s.deps.Settings.Config()
	writeJSON(w, 200, map[string]any{
		"message": "System settings updated successfully",
		"config": map[string]any{
			"language":               cfg.Language,
			"input_dir":              cfg.InputDir,
			"output_root_dir":        cfg.OutputRootDir,
			"ollama_model":           cfg.OllamaModel,
			"ollama_host":            cfg.OllamaHost,
			"personal_name_denylist": cfg.PersonalNameDenylist,
		},
	})
}

// systemStatsHandler ports web-server.ts:330-419 verbatim, including the format buckets and the
// parseFloat(toFixed(2)) size formatting.
func (s *server) systemStatsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()

	rawStats := getDirStats(cfg.InputDir)
	archiveStats := getDirStats(cfg.OutputRootDir)

	var dbBytes int64
	if info, err := os.Stat(cfg.DBPath); err == nil {
		dbBytes = info.Size()
	}

	totalFiles := rawStats.Count + archiveStats.Count
	totalBytes := rawStats.Bytes + archiveStats.Bytes

	breakdown := map[string]map[string]any{}
	for _, key := range []string{"pdf", "image", "text", "word", "excel"} {
		rc := rawStats.FormatBreakdown[key]
		ac := archiveStats.FormatBreakdown[key]
		count := rc.Count + ac.Count
		bytes := rc.Bytes + ac.Bytes
		breakdown[key] = map[string]any{
			"count":         count,
			"bytes":         bytes,
			"sizeFormatted": formatBytes(bytes),
		}
	}

	writeJSON(w, 200, map[string]any{
		"raws": map[string]any{
			"count":         rawStats.Count,
			"bytes":         rawStats.Bytes,
			"sizeFormatted": formatBytes(rawStats.Bytes),
		},
		"archive": map[string]any{
			"count":         archiveStats.Count,
			"bytes":         archiveStats.Bytes,
			"sizeFormatted": formatBytes(archiveStats.Bytes),
		},
		"database": map[string]any{
			"bytes":         dbBytes,
			"sizeFormatted": formatBytes(dbBytes),
		},
		"total": map[string]any{
			"count":         totalFiles,
			"bytes":         totalBytes,
			"sizeFormatted": formatBytes(totalBytes),
		},
		"formatBreakdown": breakdown,
	})
}

type formatStat struct {
	Count int
	Bytes int64
}

type dirStats struct {
	Count           int
	Bytes           int64
	FormatBreakdown map[string]*formatStat
}

func newDirStats() dirStats {
	return dirStats{FormatBreakdown: map[string]*formatStat{
		"pdf":   {},
		"image": {},
		"text":  {},
		"word":  {},
		"excel": {},
	}}
}

// getDirStats is getDirStatsSync: a recursive size/count walk with five extension buckets.
func getDirStats(dirPath string) dirStats {
	stats := newDirStats()
	info, err := os.Stat(dirPath)
	if err != nil || !info.IsDir() {
		return stats
	}

	_ = filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fileInfo, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		stats.Count++
		stats.Bytes += fileInfo.Size()

		ext := strings.ToLower(filepath.Ext(d.Name()))
		key := "pdf"
		switch {
		case containsString([]string{".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tiff"}, ext):
			key = "image"
		case containsString([]string{".txt", ".md", ".csv", ".log", ".json"}, ext):
			key = "text"
		case containsString([]string{".docx", ".doc"}, ext):
			key = "word"
		case containsString([]string{".xlsx", ".xls"}, ext):
			key = "excel"
		}
		if bucket := stats.FormatBreakdown[key]; bucket != nil {
			bucket.Count++
			bucket.Bytes += fileInfo.Size()
		}
		return nil
	})
	return stats
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// formatBytes ports the TS formatBytes exactly: toFixed(2) then parseFloat (trailing zeros dropped).
func formatBytes(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	k := 1024.0
	sizes := []string{"B", "KB", "MB", "GB", "TB"}
	i := int(math.Floor(math.Log(float64(bytes)) / math.Log(k)))
	if i >= len(sizes) {
		i = len(sizes) - 1
	}
	value := float64(bytes) / math.Pow(k, float64(i))
	text := strings.TrimRight(jsToFixed2(value), "0")
	text = strings.TrimRight(text, ".")
	return text + " " + sizes[i]
}

// jsToFixed2 is Number.prototype.toFixed(2) for the finite non-negative doubles formatBytes
// produces. ECMAScript rounds the double's EXACT mathematical value to two fraction digits and, on
// an exact tie, picks the larger result; strconv.FormatFloat rounds ties to even instead, so it
// prints 0.125 as "0.12" where JS prints "0.13". math/big keeps the double's exact dyadic value, so
// the tie is detected rather than hidden by the shortest-decimal representation. The expected
// outputs are pinned by TestFormatBytesJSToFixed from a node -e run of the real TS function.
func jsToFixed2(value float64) string {
	rational := new(big.Rat).SetFloat64(value)
	if rational == nil {
		return "NaN" // NaN / ±Inf never reach formatBytes
	}
	negative := rational.Sign() < 0
	if negative {
		rational.Neg(rational)
	}
	scaled := rational.Mul(rational, big.NewRat(100, 1))
	// floor(scaled + 1/2): an exact tie goes to the larger integer, matching ECMAScript.
	scaled = scaled.Add(scaled, big.NewRat(1, 2))
	integer := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	digits := integer.String()
	if len(digits) < 3 {
		digits = strings.Repeat("0", 3-len(digits)) + digits
	}
	text := digits[:len(digits)-2] + "." + digits[len(digits)-2:]
	if negative {
		text = "-" + text
	}
	return text
}
