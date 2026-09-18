package pathconv

import "testing"

// All cases below are ported verbatim from pdf-triage's src/domain/path-conversion.test.ts.

func TestWindowsToWSLPath(t *testing.T) {
	t.Run("leaves plain POSIX paths untouched", func(t *testing.T) {
		if got := WindowsToWSLPath("/custom/in", "linux"); got != "/custom/in" {
			t.Fatalf("got %q, want %q", got, "/custom/in")
		}
		if got := WindowsToWSLPath("./relative", "linux"); got != "./relative" {
			t.Fatalf("got %q, want %q", got, "./relative")
		}
	})

	t.Run("converts Windows drive paths to lowercase /mnt mounts on non-Windows hosts", func(t *testing.T) {
		if got := WindowsToWSLPath(`C:\Users\you\Documents\__raws`, "linux"); got != "/mnt/c/Users/you/Documents/__raws" {
			t.Fatalf("got %q, want %q", got, "/mnt/c/Users/you/Documents/__raws")
		}
		if got := WindowsToWSLPath("D:/Some/Folder", "linux"); got != "/mnt/d/Some/Folder" {
			t.Fatalf("got %q, want %q", got, "/mnt/d/Some/Folder")
		}
	})

	t.Run("repairs mangled WSL spellings with backslashes and/or an uppercase drive letter", func(t *testing.T) {
		if got := WindowsToWSLPath(`\mnt\C:\Users\you\__raws`, "linux"); got != "/mnt/c/Users/you/__raws" {
			t.Fatalf("got %q, want %q", got, "/mnt/c/Users/you/__raws")
		}
		if got := WindowsToWSLPath(`\mnt\C\Users\you\__raws`, "linux"); got != "/mnt/c/Users/you/__raws" {
			t.Fatalf("got %q, want %q", got, "/mnt/c/Users/you/__raws")
		}
		if got := WindowsToWSLPath("/mnt/C/Users/you/__raws", "linux"); got != "/mnt/c/Users/you/__raws" {
			t.Fatalf("got %q, want %q", got, "/mnt/c/Users/you/__raws")
		}
	})

	t.Run("is a no-op on Windows hosts", func(t *testing.T) {
		if got := WindowsToWSLPath(`C:\Users\you\__raws`, "win32"); got != `C:\Users\you\__raws` {
			t.Fatalf("got %q, want %q", got, `C:\Users\you\__raws`)
		}
	})
}

func TestWSLToWindowsPath(t *testing.T) {
	t.Run("converts a WSL /mnt path into Windows form for explorer.exe/chrome.exe", func(t *testing.T) {
		if got := WSLToWindowsPath("/mnt/c/Users/you/Documents/__raws"); got != `C:\Users\you\Documents\__raws` {
			t.Fatalf("got %q, want %q", got, `C:\Users\you\Documents\__raws`)
		}
		if got := WSLToWindowsPath("/mnt/d/SomeDrive/SomeFolder"); got != `D:\SomeDrive\SomeFolder` {
			t.Fatalf("got %q, want %q", got, `D:\SomeDrive\SomeFolder`)
		}
		if got := WSLToWindowsPath("/mnt/c"); got != `C:\` {
			t.Fatalf("got %q, want %q", got, `C:\`)
		}
	})

	t.Run("leaves native Windows and plain POSIX paths untouched", func(t *testing.T) {
		if got := WSLToWindowsPath(`C:\Users\you\__raws`); got != `C:\Users\you\__raws` {
			t.Fatalf("got %q, want %q", got, `C:\Users\you\__raws`)
		}
		if got := WSLToWindowsPath("/home/you/folder"); got != "/home/you/folder" {
			t.Fatalf("got %q, want %q", got, "/home/you/folder")
		}
		if got := WSLToWindowsPath(""); got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})
}

func TestIsWSLMountPath(t *testing.T) {
	t.Run("recognizes /mnt/<drive> paths", func(t *testing.T) {
		for _, in := range []string{"/mnt/c", "/mnt/c/Users/you/__raws", "/mnt/d"} {
			if !IsWSLMountPath(in) {
				t.Fatalf("IsWSLMountPath(%q) = false, want true", in)
			}
		}
	})

	t.Run("rejects non-mount paths", func(t *testing.T) {
		for _, in := range []string{`C:\Users\you`, "/home/you", ""} {
			if IsWSLMountPath(in) {
				t.Fatalf("IsWSLMountPath(%q) = true, want false", in)
			}
		}
	})
}
