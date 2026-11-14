package winservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteShortcutAwkwardPaths — пути с пробелами, кириллицей и одинарной
// кавычкой (ломает наивное экранирование для PowerShell): ярлык создаётся, и
// все поля читаются обратно ровно такими, какими их записали.
func TestWriteShortcutAwkwardPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Программы", "multi rtmp O'Brien")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(dir, "multirtmp.exe")
	cfg := filepath.Join(dir, "конфиг с пробелом.json")
	lnk := filepath.Join(dir, "multirtmp.lnk")

	if err := writeShortcut(lnk, exe, []string{"open", "-config", cfg}); err != nil {
		t.Fatal(err)
	}

	out, err := runPowerShell(`[Console]::OutputEncoding = [Text.Encoding]::UTF8; ` +
		`$s = (New-Object -ComObject WScript.Shell).CreateShortcut(` + psQuote(lnk) + `); ` +
		`$s.TargetPath; $s.Arguments; $s.WorkingDirectory; $s.IconLocation; $s.WindowStyle`)
	if err != nil {
		t.Fatalf("read back: %v (%s)", err, out)
	}

	got := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	want := []string{
		exe,
		`open -config "` + cfg + `"`,
		dir,
		`%SystemRoot%\System32\shell32.dll,13`,
		"7",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %q", len(got), got, want)
	}
	for i := range want {
		if !strings.EqualFold(got[i], want[i]) {
			t.Errorf("field %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
