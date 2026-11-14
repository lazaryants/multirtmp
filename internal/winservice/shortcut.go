package winservice

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// shortcutFileName — ярлык на общем рабочем столе (виден всем пользователям
// машины). Имя фиксировано: по нему же RemoveDesktopShortcut его и удаляет.
const shortcutFileName = "multirtmp.lnk"

// shortcutIcon — значок из shell32.dll. Своей иконки в exe пока нет (для
// неё нужен .syso-ресурс, а собирать его без внешнего инструмента
// нецелесообразно), поэтому берём стандартный системный.
const shortcutIcon = `%SystemRoot%\System32\shell32.dll,13`

// shortcutWindowMinimized — WindowStyle=7 (SW_SHOWMINNOACTIVE) у ярлыка:
// multirtmp.exe консольный, и без этого по клику на долю секунды мелькало бы
// чёрное окно консоли. Сообщения об ошибках подкоманда open показывает
// MessageBox'ом, а не в консоли, так что свёрнутая консоль ничего не прячет.
const shortcutWindowMinimized = 7

// publicDesktopShortcutPath — полный путь ярлыка на общем рабочем столе
// (FOLDERID_PublicDesktop, обычно C:\Users\Public\Desktop).
func publicDesktopShortcutPath() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_PublicDesktop, 0)
	if err != nil {
		return "", fmt.Errorf("winservice: failed to locate public desktop folder: %w", err)
	}
	return filepath.Join(dir, shortcutFileName), nil
}

// psQuote — строковый литерал PowerShell в одинарных кавычках: внутри них
// PowerShell ничего не раскрывает ($, `), единственный спецсимвол — сама
// одинарная кавычка, она удваивается.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// CreateDesktopShortcut создаёт (или перезаписывает) ярлык multirtmp на общем
// рабочем столе, запускающий `exePath <args...>`. .lnk — двоичный формат
// (MS-SHLLINK), поэтому создаётся через COM WScript.Shell из PowerShell — тем
// же способом, что и правило firewall (см. firewall.go), без новых
// зависимостей. Требует прав администратора (общий рабочий стол), как и сам
// install.
func CreateDesktopShortcut(exePath string, args []string) (string, error) {
	lnkPath, err := publicDesktopShortcutPath()
	if err != nil {
		return "", err
	}

	if err := writeShortcut(lnkPath, exePath, args); err != nil {
		return "", err
	}

	return lnkPath, nil
}

// writeShortcut записывает .lnk по произвольному пути — отдельно от
// CreateDesktopShortcut, чтобы её можно было проверить на временной папке, не
// трогая общий рабочий стол.
func writeShortcut(lnkPath, exePath string, args []string) error {
	script := fmt.Sprintf(`$s = (New-Object -ComObject WScript.Shell).CreateShortcut(%s); `+
		`$s.TargetPath = %s; $s.Arguments = %s; $s.WorkingDirectory = %s; `+
		`$s.IconLocation = %s; $s.WindowStyle = %d; $s.Description = %s; $s.Save()`,
		psQuote(lnkPath), psQuote(exePath), psQuote(windowsCommandLine(args)),
		psQuote(filepath.Dir(exePath)), psQuote(shortcutIcon), shortcutWindowMinimized,
		psQuote("Open the multirtmp web interface"))

	if out, err := runPowerShell(script); err != nil {
		return fmt.Errorf("winservice: failed to create desktop shortcut: %w (%s)", err, out)
	}

	return nil
}

// RemoveDesktopShortcut удаляет ярлык, если он есть. Отсутствие ярлыка — не
// ошибка (uninstall может вызываться на машине, где его не создавали или
// где его уже удалили руками).
func RemoveDesktopShortcut() error {
	lnkPath, err := publicDesktopShortcutPath()
	if err != nil {
		return err
	}

	if err := os.Remove(lnkPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("winservice: failed to remove desktop shortcut: %w", err)
	}

	return nil
}

// windowsCommandLine склеивает аргументы в одну строку по правилам
// CommandLineToArgvW — именно так её разберёт exe при запуске по ярлыку.
func windowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	return strings.Join(quoted, " ")
}
