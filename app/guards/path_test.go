package guards

import (
	"os"
	"path/filepath"
	"testing"
)

// Ported guard cases from web-server.test.ts:937-975 ("GET /api/documents/:id/file &
// GET /api/documents/file-by-path"), plus the boundary cases taxonomy.test.ts documents. The
// upstream file-by-path 404-vs-403 case is the known-red path-boundary case; its actual behavior is
// pinned here (REJECT -> 403), not the test's stated 404 expectation.
func TestResolveManagedPath(t *testing.T) {
	const (
		winInput   = "C:/pdf-triage-test/__raws"
		winOutput  = "C:/pdf-triage-test/__archive"
		wslInput   = "/mnt/c/pdf-triage-test/__raws"
		wslOutput  = "/mnt/c/pdf-triage-test/__archive"
		outsideMsg = "Path is outside the managed input/output directories — not allowed."
	)

	cases := []struct {
		name      string
		candidate string
		inputDir  string
		outputDir string
		wantOK    bool
		wantPath  string
	}{
		{
			// web-server.test.ts:956 "rejects a path outside INPUT_DIR/OUTPUT_ROOT_DIR".
			name:      "rejects a path outside the managed roots",
			candidate: "C:/pdf-triage-test/package.json",
			inputDir:  winInput,
			outputDir: winOutput,
			wantOK:    false,
		},
		{
			// KNOWN-RED PIN. web-server.test.ts:946 expects 404 for this candidate; the live TS
			// server returns 403 because Node path.resolve treats "C:/..." as relative on POSIX and
			// prepends cwd, so it is not inside the drive-form root. The guard pins the ACTUAL 403.
			name:      "PINNED RED: drive-form candidate against drive-form roots is rejected (actual 403, test expected 404)",
			candidate: "C:/pdf-triage-test/__archive/nonexistent.pdf",
			inputDir:  winInput,
			outputDir: winOutput,
			wantOK:    false,
		},
		{
			// web-server.test.ts:966 "rejects a traversal attempt that only escapes OUTPUT_ROOT_DIR".
			name:      "rejects a ../ traversal out of a WSL root",
			candidate: "/mnt/c/pdf-triage-test/__archive/../../Windows/System32/drivers/etc/hosts",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    false,
		},
		{
			name:      "rejects a deeper ../ traversal out of a WSL root",
			candidate: "/mnt/c/pdf-triage-test/__archive/invoices/../../../etc/passwd",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    false,
		},
		{
			// Prefix-sharing sibling: "__archive" vs "__archive_old" must not be treated as nested.
			name:      "rejects a prefix-sharing sibling (__archive vs __archive_old)",
			candidate: "/mnt/c/pdf-triage-test/__archive_old/x.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    false,
		},
		{
			name:      "accepts a nested path under the WSL output root",
			candidate: "/mnt/c/pdf-triage-test/__archive/invoices/sfr/2026/facture.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__archive/invoices/sfr/2026/facture.pdf",
		},
		{
			name:      "accepts a nested path under the WSL input root",
			candidate: "/mnt/c/pdf-triage-test/__raws/incoming.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__raws/incoming.pdf",
		},
		{
			// WSL/Windows form: settings.ts produces /mnt/... roots on WSL, so a Windows-spelled
			// candidate is translated by pathconv before the boundary check.
			name:      "translates a Windows backslash candidate when the roots are WSL mount paths",
			candidate: `C:\pdf-triage-test\__archive\invoices\facture.pdf`,
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__archive/invoices/facture.pdf",
		},
		{
			name:      "translates an uppercase-drive POSIX candidate",
			candidate: "/mnt/C/pdf-triage-test/__archive/facture.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__archive/facture.pdf",
		},
		{
			name:      "accepts the managed root itself",
			candidate: "/mnt/c/pdf-triage-test/__archive",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__archive",
		},
		{
			name:      "normalizes . and .. components inside a root",
			candidate: "/mnt/c/pdf-triage-test/__archive/invoices/../sfr/facture.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    true,
			wantPath:  "/mnt/c/pdf-triage-test/__archive/sfr/facture.pdf",
		},
		{
			name:      "rejects a bare relative path",
			candidate: "invoices/sfr/facture.pdf",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    false,
		},
		{
			name:      "rejects an empty candidate",
			candidate: "   ",
			inputDir:  wslInput,
			outputDir: wslOutput,
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, violation := ResolveManagedPath(tc.candidate, tc.inputDir, tc.outputDir)
			if tc.wantOK {
				if violation != nil {
					t.Fatalf("violation = %q, want nil", violation.Message)
				}
				if got != tc.wantPath {
					t.Fatalf("path = %q, want %q", got, tc.wantPath)
				}
				return
			}
			if violation == nil {
				t.Fatalf("path %q accepted, want rejection", got)
			}
			if violation.Code != CodePathOutsideManaged || violation.HTTPStatus != 403 {
				t.Fatalf("code/status = %q/%d, want %q/403", violation.Code, violation.HTTPStatus, CodePathOutsideManaged)
			}
			if violation.Message != outsideMsg {
				t.Fatalf("message = %q, want %q", violation.Message, outsideMsg)
			}
		})
	}
}

// The boundary check is lexical: it does not resolve symlinks, exactly like Node's path.normalize.
// A symlink whose textual path sits inside a root is accepted (and the host filesystem still
// resolves it under that path), which is the behavior the TS guard has today.
func TestResolveManagedPath_IsLexicalForSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archive")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.pdf")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.pdf")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, violation := ResolveManagedPath(link, root, filepath.Join(base, "other")); violation != nil {
		t.Fatalf("lexically-inside symlink rejected: %q", violation.Message)
	}
}

func TestPathOutsideManagedDirectoriesViolation(t *testing.T) {
	v := PathOutsideManagedDirectoriesViolation()
	if v.Code != CodePathOutsideManaged || v.HTTPStatus != 403 {
		t.Fatalf("code/status = %q/%d", v.Code, v.HTTPStatus)
	}
	if v.Message != "Path is outside the managed input/output directories — not allowed." {
		t.Fatalf("message = %q", v.Message)
	}
}
