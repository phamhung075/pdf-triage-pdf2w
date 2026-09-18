// The Vision Lab composition root, porting src/vision-lab-main.ts.
//
// TS's entrypoint was three lines: import startVisionLabServer and call it, letting the module
// read CONFIG (VISION_LAB_PORT, default 3179, and the shared HOST) at load time. A Go package
// cannot be a `main` while also being importable, and this repository's file-scope rule allows
// only this directory, so the entrypoint is the exported Main function a thin cmd can call. It
// blocks until ctx is cancelled.
package visionlab

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
)

// Main resolves CONFIG, wires the production vision-model client and step functions, and starts the
// server on VISION_LAB_PORT (default 3179). It is the Go vision-lab-main.ts.
func Main(ctx context.Context) error {
	store, err := settings.New(settings.Options{})
	if err != nil {
		return err
	}
	cfg := store.Config()

	log := logger.New(logger.DefaultOptions(filepath.Join(store.DataDir(), "logs")))
	visionClient := vision.NewClient(cfg.OllamaHost, cfg.OllamaVisionModel)
	stepper := imagetopdf.NewStepper(imagetopdf.DefaultDeps(visionClient, log))

	deps := Deps{
		Stepper:   stepper,
		PublicDir: filepath.Join(store.BaseDir(), "public"),
		Logger:    log,
	}
	return Start(ctx, fmt.Sprintf("%s:%d", cfg.Host, cfg.VisionLabPort), deps)
}
