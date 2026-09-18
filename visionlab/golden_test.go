package visionlab

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
)

// Golden-file contract check.
//
// Every testdata/golden/*.json was captured from the REAL TypeScript createVisionLabApp mounted
// with supertest by the throwaway scratch/capture-vision-lab-golden.test.ts harness (same
// image-to-pdf mock as vision-lab-server.test.ts), before that harness was deleted. Each case below
// replays the same request against visionlab.NewApp and asserts the status, Content-Type and a
// structurally identical JSON body.
//
// The fake step results mirror the capture harness exactly, so the only variation between the two
// apps under test is the implementation, not the fixture.
type goldenFile struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Body        any    `json:"body"`
}

var (
	orientOK    = imagetopdf.PipelineStepResult{Step: 1, Label: "oriented", ImageBase64: "abc", DurationMS: 5}
	cropOK      = imagetopdf.PipelineStepResult{Step: 2, Label: "cropped", ImageBase64: "abc", DurationMS: 5}
	enhanceOK   = imagetopdf.PipelineStepResult{Step: 3, Label: "enhanced", ImageBase64: "abc", DurationMS: 5}
	orientError = imagetopdf.PipelineStepResult{Step: 1, Label: "oriented", DurationMS: 5, Error: "model unreachable"}
)

func loadGolden(t *testing.T, name string) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var out goldenFile
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	return out
}

func TestGoldenContract(t *testing.T) {
	fake := map[string]any{"inputImageBase64": "ZmFrZQ=="}
	stepBody := func(step any) map[string]any {
		return map[string]any{"step": step, "inputImageBase64": "ZmFrZQ=="}
	}

	cases := []struct {
		name      string
		body      any
		configure func(*fakeStepper)
		wantInput string
	}{
		{name: "post-diagnose-missing-step", body: fake},
		{name: "post-diagnose-step-5", body: stepBody(5)},
		{name: "post-diagnose-step-4", body: stepBody(4)},
		{name: "post-diagnose-step-string", body: stepBody("1")},
		{name: "post-diagnose-missing-image", body: map[string]any{"step": 1}},
		{name: "post-diagnose-image-nonstring", body: map[string]any{"step": 1, "inputImageBase64": 123}},
		{name: "post-diagnose-empty-image", body: map[string]any{"step": 1, "inputImageBase64": ""}},
		{
			name: "post-diagnose-step-1-ok", body: stepBody(1),
			configure: func(s *fakeStepper) { s.results[1] = orientOK }, wantInput: "fake",
		},
		{
			name: "post-diagnose-step-2-ok", body: stepBody(2),
			configure: func(s *fakeStepper) { s.results[2] = cropOK }, wantInput: "fake",
		},
		{
			name: "post-diagnose-step-3-ok", body: stepBody(3),
			configure: func(s *fakeStepper) { s.results[3] = enhanceOK }, wantInput: "fake",
		},
		{
			name: "post-diagnose-step-1-result-error", body: stepBody(1),
			configure: func(s *fakeStepper) { s.results[1] = orientError }, wantInput: "fake",
		},
		{
			name: "post-diagnose-step-1-500", body: stepBody(1),
			configure: func(s *fakeStepper) { s.panics[1] = errors.New("unexpected crash") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			golden := loadGolden(t, tc.name)
			stepper := newFakeStepper()
			if tc.configure != nil {
				tc.configure(stepper)
			}
			rec := doRequest(t, NewApp(Deps{Stepper: stepper}), http.MethodPost, DiagnoseStepPath, tc.body, "")

			if rec.Code != golden.Status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, golden.Status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != golden.ContentType {
				t.Fatalf("Content-Type = %q, want %q", got, golden.ContentType)
			}
			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode Go body %q: %v", rec.Body.String(), err)
			}
			if !reflect.DeepEqual(golden.Body, got) {
				t.Fatalf("body = %#v, want %#v", got, golden.Body)
			}
			if tc.wantInput != "" {
				if len(stepper.inputs) != 1 {
					t.Fatalf("step calls = %v, want exactly 1", stepper.calls)
				}
				if string(stepper.inputs[0]) != tc.wantInput {
					t.Fatalf("step received %q, want %q", stepper.inputs[0], tc.wantInput)
				}
			}
		})
	}
}
