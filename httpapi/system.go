// System/config routes #7, #8, #10-#13, ported from web-server.ts:239-440.
package httpapi

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// getConfigHandler ports web-server.ts:314-327, changed so API keys never leave the server: the raw
// *_api_key fields are gone and each provider carries an always-present *_api_key_set boolean.
func (s *server) getConfigHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()
	writeJSON(w, 200, configResponse(cfg))
}

// configResponse is the shared GET/PUT /api/config `config` object. It never includes a raw
// *_api_key value; for each provider it always includes `P_api_key_set` (true iff the stored key is
// non-empty after trimming), so the dashboard can show whether a key is configured without the key
// ever reaching the browser.
func configResponse(cfg settings.Config) map[string]any {
	resp := map[string]any{
		"language":               cfg.Language,
		"input_dir":              cfg.InputDir,
		"output_root_dir":        cfg.OutputRootDir,
		"ollama_model":           cfg.OllamaModel,
		"ollama_host":            cfg.OllamaHost,
		"personal_name_denylist": cfg.PersonalNameDenylist,
		"google_api_key_set":     strings.TrimSpace(cfg.GoogleAPIKey) != "",
		"anthropic_api_key_set":  strings.TrimSpace(cfg.AnthropicAPIKey) != "",
		"deepseek_api_key_set":   strings.TrimSpace(cfg.DeepSeekAPIKey) != "",
		"openai_api_key_set":     strings.TrimSpace(cfg.OpenAIAPIKey) != "",
	}
	if cfg.AIProvider != "" {
		resp["ai_provider"] = cfg.AIProvider
	}
	if cfg.CloudProvider != "" {
		resp["cloud_provider"] = cfg.CloudProvider
	}
	if cfg.GoogleModel != "" {
		resp["google_model"] = cfg.GoogleModel
	}
	if cfg.GoogleBaseURL != "" {
		resp["google_base_url"] = cfg.GoogleBaseURL
	}
	if cfg.AnthropicModel != "" {
		resp["anthropic_model"] = cfg.AnthropicModel
	}
	if cfg.AnthropicBaseURL != "" {
		resp["anthropic_base_url"] = cfg.AnthropicBaseURL
	}
	if cfg.DeepSeekModel != "" {
		resp["deepseek_model"] = cfg.DeepSeekModel
	}
	if cfg.DeepSeekBaseURL != "" {
		resp["deepseek_base_url"] = cfg.DeepSeekBaseURL
	}
	if cfg.OpenAIModel != "" {
		resp["openai_model"] = cfg.OpenAIModel
	}
	if cfg.OpenAIBaseURL != "" {
		resp["openai_base_url"] = cfg.OpenAIBaseURL
	}
	return resp
}

// keepAPIKey maps an incoming *_api_key request field to the settings patch. An omitted or null
// value (nil pointer), or an empty/whitespace-only value, means "keep the stored key": nil is passed
// to UpdateSettings so the store leaves the current value untouched. Only a non-empty value replaces
// it. There is deliberately no way to clear a stored key through the API (documented limitation).
func keepAPIKey(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// withoutNullAPIKeys removes a JSON null for any of the four *_api_key fields before schema parsing.
// SystemSettingsSchema's shared string helper rejects null, but the config contract treats a null
// key as "keep the stored key", so the field is dropped here and the same "keep" path as an omitted
// field applies. A body that is empty or not a JSON object is returned unchanged so the schema
// parser reports its own error.
func withoutNullAPIKeys(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	changed := false
	for _, name := range []string{"google_api_key", "anthropic_api_key", "deepseek_api_key", "openai_api_key"} {
		if v, ok := fields[name]; ok && string(bytes.TrimSpace(v)) == "null" {
			delete(fields, name)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// putConfigHandler ports web-server.ts:422-440: SystemSettingsSchema.parse then updateConfig.
func (s *server) putConfigHandler(w http.ResponseWriter, r *http.Request) {
	parsed, err := documentschema.ParseSystemSettings(withoutNullAPIKeys(bodyBytes(r)))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	patch := settings.UpdateSettings{
		Language:         parsed.Language,
		InputDir:         &parsed.InputDir,
		OutputRootDir:    &parsed.OutputRootDir,
		OllamaModel:      &parsed.OllamaModel,
		OllamaHost:       &parsed.OllamaHost,
		AIProvider:       parsed.AIProvider,
		CloudProvider:    parsed.CloudProvider,
		GoogleAPIKey:     keepAPIKey(parsed.GoogleAPIKey),
		GoogleModel:      parsed.GoogleModel,
		GoogleBaseURL:    parsed.GoogleBaseURL,
		AnthropicAPIKey:  keepAPIKey(parsed.AnthropicAPIKey),
		AnthropicModel:   parsed.AnthropicModel,
		AnthropicBaseURL: parsed.AnthropicBaseURL,
		DeepSeekAPIKey:   keepAPIKey(parsed.DeepSeekAPIKey),
		DeepSeekModel:    parsed.DeepSeekModel,
		DeepSeekBaseURL:  parsed.DeepSeekBaseURL,
		OpenAIAPIKey:     keepAPIKey(parsed.OpenAIAPIKey),
		OpenAIModel:      parsed.OpenAIModel,
		OpenAIBaseURL:    parsed.OpenAIBaseURL,
	}
	// nil means the field was omitted; an explicit [] is a provided empty denylist (TS `if (arr)`).
	if parsed.PersonalNameDenylist != nil {
		patch.PersonalNameDenylist = *parsed.PersonalNameDenylist
	}

	if err := s.deps.Settings.UpdateConfig(patch); err != nil {
		writeError(w, 400, err.Error())
		return
	}

	// Make the change effective immediately (AI provider switch, Ollama host/model) instead of
	// waiting for the next triage scan's ReloadConfig.
	if s.deps.OnConfigChanged != nil {
		s.deps.OnConfigChanged(s.deps.Settings.Config())
	}

	cfg := s.deps.Settings.Config()

	writeJSON(w, 200, map[string]any{
		"message": "System settings updated successfully",
		"config":  configResponse(cfg),
	})
}

// systemStatsCacheTTL is how long a computed (raws, archive) directory-stats pair is served before
// the next request walks the two trees again. Those trees live on a slow 9p/OneDrive mount, so a
// repeat Settings-modal call must not pay the ~3.5 s walk again; 30 s is the accepted staleness
// window after a scan, and no cache-invalidation wiring is added to scan/relocalize/clear.
const systemStatsCacheTTL = 30 * time.Second

// systemStatsNow and systemStatsWalk are package-level seams that let the cache tests substitute a
// fake clock and a counting walker without touching exported API. Tests save the originals and
// restore them in t.Cleanup; production never reassigns them.
//
// The preferred home for the cache itself is a field on server, but this job's change scope covers
// only httpapi/system.go and its test file while server is declared in server.go. The per-server
// state therefore lives in the package-level sync.Map below, keyed by *server: every server still
// gets its own independent cache and mutex, so the behavior is identical.
var (
	systemStatsNow  = time.Now
	systemStatsWalk = getDirStats
)

// systemStatsEntry is one server's cached walk result plus the directory key and timestamp that
// decide whether it is still fresh.
type systemStatsEntry struct {
	mu          sync.Mutex
	initialized bool
	inputDir    string
	outputRoot  string
	computedAt  time.Time
	raw         dirStats
	archive     dirStats
}

// systemStatsEntries maps a *server to its cache entry. sync.Map because handlers run concurrently
// and each server's entry is created lazily on its first request.
var systemStatsEntries sync.Map

func (s *server) systemStatsEntry() *systemStatsEntry {
	if v, ok := systemStatsEntries.Load(s); ok {
		return v.(*systemStatsEntry)
	}
	entry := &systemStatsEntry{}
	actual, _ := systemStatsEntries.LoadOrStore(s, entry)
	return actual.(*systemStatsEntry)
}

// cachedDirStats returns the (raws, archive) pair. It serves the cached value while it is fresh and
// its directory key still matches, and otherwise recomputes under the entry's mutex so a concurrent
// miss walks each tree only once (single flight: waiters block, then hit the just-refreshed cache).
func (s *server) cachedDirStats(inputDir, outputRoot string) (dirStats, dirStats) {
	entry := s.systemStatsEntry()
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.initialized && entry.inputDir == inputDir && entry.outputRoot == outputRoot &&
		systemStatsNow().Sub(entry.computedAt) < systemStatsCacheTTL {
		return entry.raw, entry.archive
	}

	entry.raw, entry.archive = walkDirStatsPair(inputDir, outputRoot)
	entry.initialized = true
	entry.inputDir = inputDir
	entry.outputRoot = outputRoot
	entry.computedAt = systemStatsNow()
	return entry.raw, entry.archive
}

// walkDirStatsPair walks the two trees concurrently, each goroutine writing its own result value.
func walkDirStatsPair(inputDir, outputRoot string) (dirStats, dirStats) {
	var (
		wg      sync.WaitGroup
		raw     dirStats
		archive dirStats
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		raw = systemStatsWalk(inputDir)
	}()
	go func() {
		defer wg.Done()
		archive = systemStatsWalk(outputRoot)
	}()
	wg.Wait()
	return raw, archive
}

// systemStatsHandler ports web-server.ts:330-419 verbatim, including the format buckets and the
// parseFloat(toFixed(2)) size formatting.
func (s *server) systemStatsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()

	// The directory walks are cached for systemStatsCacheTTL; the DB size below is a fresh stat on
	// every request.
	rawStats, archiveStats := s.cachedDirStats(cfg.InputDir, cfg.OutputRootDir)

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
