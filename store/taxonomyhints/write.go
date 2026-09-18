package taxonomyhints

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
)

// renameFile is a seam for the EPERM/EBUSY fallback test; production uses os.Rename.
var renameFile = os.Rename

// writeFileAtomic writes data to path via a temp file + rename, falling back to a copy on an
// EPERM/EBUSY-family rename failure. It mirrors infra/jsonregistry's unexported helper (which may
// not be modified); see the package comment.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}
	if err := renameFile(tmpPath, path); err != nil {
		if isPermOrBusy(err) {
			if copyErr := copyFileContents(tmpPath, path); copyErr != nil {
				return copyErr
			}
			_ = os.Remove(tmpPath)
			return nil
		}
		return err
	}
	return nil
}

// copyFileContents is the fallback replacement for fs.copyFileSync.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// marshalIndentNoHTMLEscape is `JSON.stringify(data, null, 2)`: two-space indent and no HTML
// escaping (Go escapes <, > and & by default). The encoder appends a newline that stringify does
// not, so it is trimmed. Duplicated from infra/settings / infra/jsonregistry for the same reason.
func marshalIndentNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
