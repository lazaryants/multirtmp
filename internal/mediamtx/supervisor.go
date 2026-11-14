// Package mediamtx запускает MediaMTX (сервер WebRTC-превью) дочерним
// процессом и следит за ним: перезапуск с backoff, распознавание занятого
// порта, отдельный ротирующий лог, состояние для /status.
//
// С ingest, буфером и выходами пакет не делит ни одной горутины, мьютекса
// или канала: для них MediaMTX — просто внешний RTMP-сервер, на который
// смотрит обычный выход превью.
package mediamtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"multirtmp/internal/logrotate"
)

// State — состояние MediaMTX для /status и интерфейса.
type State string

const (
	StateNotInstalled State = "not_installed" // preview.enabled, но mediamtx.exe нет
	StateStarting     State = "starting"      // процесс запущен, ещё не прожил runningAfter
	StateRunning      State = "running"
	StateRestarting   State = "restarting" // процесс завершился, ждём backoff
	StatePortInUse    State = "port_in_use"
	StateStopped      State = "stopped" // служба останавливается
)

// Status — снимок для /status.
type Status struct {
	State    State   `json:"state"`
	Error    string  `json:"error,omitempty"`
	Restarts int     `json:"restarts"`
	RetryInS float64 `json:"retry_in_s,omitempty"`
}

const (
	logMaxBytes = 10 * 1024 * 1024
	logMaxFiles = 5
	tailLines   = 20
)

// Supervisor — один дочерний MediaMTX. Создаётся через New, запускается
// Run, состояние — Status.
type Supervisor struct {
	exe, conf string
	logPath   string
	logger    *slog.Logger

	// Тайминги — поля, а не константы, чтобы тесты не ждали минутами.
	runningAfter time.Duration   // сколько прожить, чтобы считаться running
	quickExit    time.Duration   // выход быстрее — "не смог стартовать"
	stableUptime time.Duration   // после стольких секунд работы backoff сбрасывается
	backoff      []time.Duration // последний элемент — потолок
	portBusyWait time.Duration   // пауза перед повтором при занятом порте
	killWait     time.Duration   // сколько ждать выхода после Kill

	mu       sync.Mutex
	st       Status
	retryAt  time.Time
	notFound bool
}

// New готовит супервизор для exe с конфигом conf и логом logPath. Если exe
// не существует, Run ничего не запускает, а Status навсегда
// StateNotInstalled — релей важнее превью, это не ошибка старта.
func New(exe, conf, logPath string, logger *slog.Logger) *Supervisor {
	s := &Supervisor{
		exe:          exe,
		conf:         conf,
		logPath:      logPath,
		logger:       logger,
		runningAfter: 3 * time.Second,
		quickExit:    5 * time.Second,
		stableUptime: 60 * time.Second,
		backoff:      []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second},
		portBusyWait: 60 * time.Second,
		killWait:     5 * time.Second,
	}

	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		s.notFound = true
		s.st = Status{State: StateNotInstalled, Error: fmt.Sprintf("%s not found", exe)}
	} else {
		s.st = Status{State: StateStarting}
	}

	return s
}

// Installed — найден ли mediamtx.exe.
func (s *Supervisor) Installed() bool { return !s.notFound }

// Status — снимок текущего состояния.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.st
	if !s.retryAt.IsZero() {
		if d := time.Until(s.retryAt); d > 0 {
			st.RetryInS = float64(d.Round(100*time.Millisecond)) / float64(time.Second)
		}
	}
	return st
}

func (s *Supervisor) set(fn func(st *Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

// Run держит MediaMTX запущенным, пока не отменён ctx; при отмене гасит
// процесс и возвращается. Паника не выходит наружу — превью не должно
// ронять релей.
func (s *Supervisor) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("mediamtx: supervisor panic", "panic", r)
			s.set(func(st *Status) { st.State = StateRestarting; st.Error = fmt.Sprintf("internal error: %v", r) })
		}
	}()

	if s.notFound {
		return
	}

	lw, err := logrotate.New(s.logPath, logMaxBytes, logMaxFiles)
	if err != nil {
		s.logger.Error("mediamtx: failed to open log file, preview server not started", "path", s.logPath, "error", err)
		s.set(func(st *Status) {
			st.State = StateRestarting
			st.Error = fmt.Sprintf("cannot open %s: %v", s.logPath, err)
		})
		return
	}
	defer lw.Close()

	attempt := 0
	for {
		started := time.Now()
		tail, runErr := s.runOnce(ctx, lw)
		if ctx.Err() != nil {
			s.set(func(st *Status) { st.State = StateStopped; st.Error = "" })
			s.mu.Lock()
			s.retryAt = time.Time{}
			s.mu.Unlock()
			return
		}

		uptime := time.Since(started)
		if uptime >= s.stableUptime {
			attempt = 0
		}
		attempt++

		state, msg, wait := s.classifyExit(uptime, tail, runErr, attempt)

		s.mu.Lock()
		s.st.State = state
		s.st.Error = msg
		s.st.Restarts++
		s.retryAt = time.Now().Add(wait)
		s.mu.Unlock()

		if state == StatePortInUse {
			s.logger.Error("mediamtx: port already in use, preview server not running", "error", msg, "retry_in", wait)
		} else {
			s.logger.Warn("mediamtx: preview server exited, restarting", "uptime", uptime.Round(time.Millisecond), "error", msg, "retry_in", wait)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			s.set(func(st *Status) { st.State = StateStopped; st.Error = "" })
			s.mu.Lock()
			s.retryAt = time.Time{}
			s.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// classifyExit решает, что показать и сколько ждать перед следующим
// запуском.
func (s *Supervisor) classifyExit(uptime time.Duration, tail []string, runErr error, attempt int) (State, string, time.Duration) {
	if uptime < s.quickExit {
		if addr, ok := findBindError(tail); ok {
			return StatePortInUse,
				fmt.Sprintf("%s is already in use — is another MediaMTX running? Close it; the preview will start by itself", addr),
				s.portBusyWait
		}
	}

	msg := lastErrorLine(tail)
	if msg == "" && runErr != nil {
		msg = "process exited: " + runErr.Error()
	}
	if msg == "" {
		msg = "process exited"
	}

	i := attempt - 1
	if i >= len(s.backoff) {
		i = len(s.backoff) - 1
	}
	return StateRestarting, msg, s.backoff[i]
}

// bindRe — так Go (а значит и MediaMTX) сообщает о неудачном listen:
// "listen tcp :19350: bind: <текст ОС>". Префикс до "bind:" от языка Windows
// не зависит, текст после него — локализован, поэтому смотрим только на
// префикс.
var bindRe = regexp.MustCompile(`listen (?:tcp|udp)[46]? (\S+): bind:`)

func findBindError(tail []string) (string, bool) {
	for i := len(tail) - 1; i >= 0; i-- {
		if m := bindRe.FindStringSubmatch(tail[i]); m != nil {
			return "Port " + m[1], true
		}
	}
	return "", false
}

// lastErrorLine — текст последней строки уровня ERR из вывода MediaMTX
// (формат "2026/11/14 10:00:00 ERR ..."). Только ERR: если процесс убили
// снаружи, ERR-строки нет, а последняя INF-строка (например, про закрытую
// WebRTC-сессию зрителя) к причине выхода отношения не имеет — тогда
// вызывающий показывает код выхода.
func lastErrorLine(tail []string) string {
	for i := len(tail) - 1; i >= 0; i-- {
		if j := strings.Index(tail[i], " ERR "); j >= 0 {
			return strings.TrimSpace(tail[i][j+5:])
		}
	}
	return ""
}

// runOnce запускает процесс и ждёт его выхода или отмены ctx. Возвращает
// последние строки вывода.
func (s *Supervisor) runOnce(ctx context.Context, lw io.Writer) ([]string, error) {
	tw := &tailWriter{w: lw, max: tailLines, onErr: func(err error) {
		s.logger.Warn("mediamtx: failed to write preview server log", "path", s.logPath, "error", err)
	}}

	cmd := exec.Command(s.exe, s.conf)
	cmd.Dir = filepath.Dir(s.exe)
	cmd.Stdout = tw
	cmd.Stderr = tw
	// Без своей консоли: Ctrl+C в окне, где multirtmp запущен вручную, не
	// должен доходить до MediaMTX в обход супервизора.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

	s.set(func(st *Status) { st.State = StateStarting })
	s.mu.Lock()
	s.retryAt = time.Time{}
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.exe, err)
	}

	if !childrenBoundToProcess() {
		job, err := assignChildToJob(cmd.Process.Pid)
		if err != nil {
			s.logger.Warn("mediamtx: could not bind the preview server to this process; it may outlive a crash of multirtmp", "error", err)
		} else {
			defer windows.CloseHandle(job)
		}
	}

	s.logger.Info("mediamtx: preview server started", "pid", cmd.Process.Pid, "exe", s.exe)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	running := time.NewTimer(s.runningAfter)
	defer running.Stop()

	for {
		select {
		case err := <-done:
			return tw.lines(), err

		case <-running.C:
			s.set(func(st *Status) { st.State = StateRunning; st.Error = "" })

		case <-ctx.Done():
			// MediaMTX ничего не пишет на диск — мягкая остановка не нужна.
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.logger.Warn("mediamtx: failed to stop preview server", "error", err)
			}
			select {
			case <-done:
			case <-time.After(s.killWait):
				s.logger.Warn("mediamtx: preview server did not exit after kill", "pid", cmd.Process.Pid)
			}
			s.logger.Info("mediamtx: preview server stopped")
			return tw.lines(), nil
		}
	}
}

// tailWriter пишет всё в w (лог-файл) и попутно держит последние max строк
// для разбора причины выхода.
//
// Write никогда не возвращает ошибку: os/exec на ошибке перестаёт читать
// pipe, и MediaMTX повис бы на записи в stdout (например, когда кончилось
// место на диске). Ошибка записи в лог логируется один раз через onErr.
type tailWriter struct {
	w     io.Writer
	max   int
	onErr func(error)
	erred bool

	mu      sync.Mutex
	partial []byte
	tail    []string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(t.partial[:i]), "\r")
		t.partial = t.partial[i+1:]
		t.tail = append(t.tail, line)
		if len(t.tail) > t.max {
			t.tail = t.tail[len(t.tail)-t.max:]
		}
	}
	// Защита от бесконечной строки без перевода строки.
	if len(t.partial) > 64*1024 {
		t.partial = t.partial[len(t.partial)-64*1024:]
	}
	t.mu.Unlock()

	if _, err := t.w.Write(p); err != nil {
		t.mu.Lock()
		first := !t.erred
		t.erred = true
		t.mu.Unlock()
		if first && t.onErr != nil {
			t.onErr(err)
		}
	}
	return len(p), nil
}

func (t *tailWriter) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.tail...)
	if len(t.partial) > 0 {
		out = append(out, strings.TrimRight(string(t.partial), "\r\n"))
	}
	return out
}
