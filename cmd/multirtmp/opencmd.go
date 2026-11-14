package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"multirtmp/internal/config"
	"multirtmp/internal/winservice"
)

const (
	// openProbeTimeout — таймаут одной HTTP-пробы веб-интерфейса.
	openProbeTimeout = 1500 * time.Millisecond
	// openStartupWait — сколько ждать, пока служба в состоянии
	// start_pending/running не начнёт отвечать по HTTP (типичный случай —
	// клик по ярлыку сразу после загрузки Windows).
	openStartupWait = 10 * time.Second
	// openRetryInterval — пауза между пробами в течение openStartupWait.
	openRetryInterval = 500 * time.Millisecond
)

// cmdOpen — подкоманда open (её запускает ярлык на рабочем столе, см.
// winservice.CreateDesktopShortcut): открывает веб-интерфейс в Edge в режиме
// приложения, а если Edge нет — в браузере по умолчанию. Если интерфейс не
// отвечает — понятное окно с сообщением вместо страницы "не удаётся
// подключиться".
//
// Конфиг только читается: в отличие от runApp, образец при отсутствии файла
// здесь НЕ создаётся.
func cmdOpen(args []string) {
	flags := flag.NewFlagSet("open", flag.ExitOnError)
	cfgPathFlag := flags.String("config", "", "config.json path (default: next to the executable)")
	_ = flags.Parse(args)

	cfgPath, err := resolveConfigPath(*cfgPathFlag)
	if err != nil {
		openFail(fmt.Sprintf("Failed to locate config.json: %v", err))
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			openFail(fmt.Sprintf("config.json not found:\n%s\n\nRun multirtmp.exe once to create a sample, edit it, then install and start the service.", cfgPath))
		}
		openFail(fmt.Sprintf("config.json is invalid, multirtmp cannot run with it:\n\n%v", err))
	}

	pageURL, err := uiURL(cfg.APIAddr, cfg.APIToken)
	if err != nil {
		openFail(fmt.Sprintf("Invalid api.addr in config.json: %v", err))
	}

	if msg := waitForUI(pageURL, cfg.APIAddr); msg != "" {
		openFail(msg)
	}

	if err := launchBrowser(pageURL); err != nil {
		openFail(fmt.Sprintf("Failed to open a browser: %v\n\nOpen this address manually:\n%s", err, pageURL))
	}
}

// uiURL — адрес веб-интерфейса для браузера на ЭТОЙ машине по api.addr.
//
//   - loopback или "все интерфейсы" (0.0.0.0, ::, пустой host) — через
//     127.0.0.1 (или сам loopback-IP из addr) без токена: такие запросы
//     сервер пускает по loopback-исключению (см. api.requireToken).
//   - конкретный не-loopback адрес (LAN-IP, имя) — сервер вообще не слушает
//     127.0.0.1, запрос уйдёт с LAN-адреса и под исключение не попадёт;
//     поэтому — этот адрес плюс токен из того же конфига.
func uiURL(apiAddr, token string) (string, error) {
	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", apiAddr, err)
	}

	ip := net.ParseIP(host)
	switch {
	case host == "" || strings.EqualFold(host, "localhost") || (ip != nil && ip.IsUnspecified()):
		return fmt.Sprintf("http://127.0.0.1:%s/", port), nil
	case ip != nil && ip.IsLoopback():
		return fmt.Sprintf("http://%s/", net.JoinHostPort(host, port)), nil
	}

	u := fmt.Sprintf("http://%s/", net.JoinHostPort(host, port))
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u, nil
}

// probeUI — отвечает ли веб-интерфейс вообще. Любой HTTP-ответ (200, 401,
// 403) означает, что сервер жив; токен в пробу не передаётся — он не нужен,
// чтобы это понять.
func probeUI(pageURL string) bool {
	u, err := url.Parse(pageURL)
	if err != nil {
		return false
	}
	u.Path = "/status"
	u.RawQuery = ""

	client := &http.Client{Timeout: openProbeTimeout}
	resp, err := client.Get(u.String())
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// waitForUI возвращает "" если интерфейс отвечает (возможно, после ожидания
// стартующей службы), иначе — текст для оператора, почему он недоступен.
func waitForUI(pageURL, apiAddr string) string {
	deadline := time.Now().Add(openStartupWait)

	for {
		if probeUI(pageURL) {
			return ""
		}

		state, installed, err := winservice.QueryStatusReadOnly(serviceName)
		switch {
		case err != nil:
			// Не удалось узнать состояние — ждём как стартующую службу, а по
			// истечении времени сообщаем общий текст.
		case !installed:
			return "multirtmp is not running: the service is not installed.\n\n" +
				"Install it once (as administrator):\n  multirtmp.exe install\n  multirtmp.exe start"
		case state == "stopped" || state == "stop_pending":
			msg := "multirtmp is not running (the service is stopped).\n\n" +
				"Start it (as administrator):\n  multirtmp.exe start"
			if line, ok := lastErrorLogLine(); ok {
				msg += "\n\nLast error in multirtmp.log:\n" + line
			}
			return msg
		}

		if time.Now().After(deadline) {
			msg := fmt.Sprintf("The multirtmp service is running, but the web interface does not respond on %s.\n\n"+
				"Check multirtmp.log next to multirtmp.exe. If api.addr in config.json was changed, restart the service:\n"+
				"  multirtmp.exe stop\n  multirtmp.exe start", apiAddr)
			if err != nil {
				msg = fmt.Sprintf("The multirtmp web interface does not respond on %s, and the service state could not be checked: %v", apiAddr, err)
			}
			return msg
		}

		time.Sleep(openRetryInterval)
	}
}

// launchBrowser — Edge в режиме приложения (отдельное окно без адресной
// строки), иначе браузер по умолчанию.
func launchBrowser(pageURL string) error {
	if edge := findEdge(); edge != "" {
		if err := exec.Command(edge, "--app="+pageURL).Start(); err == nil {
			return nil
		}
	}

	verb, _ := windows.UTF16PtrFromString("open")
	target, err := windows.UTF16PtrFromString(pageURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if err := windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("default browser: %w", err)
	}
	return nil
}

// findEdge — путь к msedge.exe: из App Paths в реестре (так его регистрирует
// установщик Edge), затем стандартные места установки. Пусто — не найден.
func findEdge() string {
	const appPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`

	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		k, err := registry.OpenKey(root, appPath, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		p, _, err := k.GetStringValue("")
		k.Close()
		if err == nil && fileExists(p) {
			return p
		}
	}

	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if dir := os.Getenv(env); dir != "" {
			p := filepath.Join(dir, "Microsoft", "Edge", "Application", "msedge.exe")
			if fileExists(p) {
				return p
			}
		}
	}

	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// openFail показывает сообщение в окне (консоль у ярлыка свёрнута — текст в
// ней никто не увидит), дублирует его в stderr и завершает процесс.
func openFail(msg string) {
	fmt.Fprintln(os.Stderr, "multirtmp open:", msg)

	text, err1 := windows.UTF16PtrFromString(msg)
	caption, err2 := windows.UTF16PtrFromString("multirtmp")
	if err1 == nil && err2 == nil {
		_, _ = windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONWARNING)
	}

	os.Exit(1)
}
