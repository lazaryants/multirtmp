package mediamtx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Тестовый бинарник умеет притворяться MediaMTX (и не только) — режим
// задаётся переменной окружения. Так супервизор запускает настоящий
// отдельный процесс, а не заглушку в памяти.
const fakeEnv = "MULTIRTMP_FAKE_MEDIAMTX"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeEnv) {
	case "":
		os.Exit(m.Run())
	case "bind":
		// Ровно то, что пишет MediaMTX v1.21.3 при занятом порте (снято с
		// реального запуска двух экземпляров).
		fmt.Println("2026/11/14 21:15:03 INF MediaMTX v1.21.3, windows, amd64")
		fmt.Println("2026/11/14 21:15:03 ERR listen tcp 127.0.0.1:19350: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.")
		fmt.Println("2026/11/14 21:15:03 INF waiting for running hooks")
		os.Exit(1)
	case "crash":
		fmt.Println("2026/11/14 21:15:03 INF MediaMTX v1.21.3, windows, amd64")
		fmt.Println("2026/11/14 21:15:03 ERR json: unknown field \"badKey\"")
		os.Exit(1)
	case "env":
		fmt.Println("ENV MTX_WEBRTCIPSFROMINTERFACESLIST=" + os.Getenv("MTX_WEBRTCIPSFROMINTERFACESLIST"))
		os.Exit(1)
	case "live":
		fmt.Println("2026/11/14 21:15:03 INF [RTMP] started")
		time.Sleep(time.Hour)
	case "parent", "parent-nobind":
		// Процесс-"multirtmp": привязывает себя к job (в контрольном режиме
		// parent-nobind — нет), запускает ребёнка и печатает его PID. Тест
		// убивает этот процесс и смотрит, что стало с ребёнком.
		if os.Getenv(fakeEnv) == "parent" {
			if err := BindChildrenToProcess(); err != nil {
				fmt.Println("ERR", err)
				os.Exit(2)
			}
		}
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fakeEnv+"=live")
		if err := child.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		fmt.Println("CHILD", child.Process.Pid)
		time.Sleep(time.Hour)
	}
	os.Exit(3)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newFake — супервизор, запускающий тестовый бинарник в режиме mode, с
// короткими таймингами.
func newFake(t *testing.T, mode string) *Supervisor {
	t.Helper()
	t.Setenv(fakeEnv, mode)
	dir := t.TempDir()
	s := New(os.Args[0], filepath.Join(dir, "mediamtx.yml"), filepath.Join(dir, "mediamtx.log"), nil, testLogger())
	s.runningAfter = 300 * time.Millisecond
	s.quickExit = 2 * time.Second
	s.stableUptime = time.Hour
	s.backoff = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	s.portBusyWait = 5 * time.Second
	return s
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNotInstalled(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "nope", "mediamtx.exe"), "x.yml", "x.log", nil, testLogger())
	if s.Installed() {
		t.Fatal("Installed() = true for a missing exe")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Run(ctx) // должен вернуться сразу, ничего не запуская
	if st := s.Status(); st.State != StateNotInstalled || !strings.Contains(st.Error, "not found") {
		t.Fatalf("status = %+v", st)
	}
}

// Занятый порт: понятное сообщение и ОДНА попытка за portBusyWait, а не
// цикл быстрых перезапусков.
func TestPortInUseNoFastLoop(t *testing.T) {
	s := newFake(t, "bind")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, "port_in_use", 5*time.Second, func() bool { return s.Status().State == StatePortInUse })
	st := s.Status()
	if !strings.Contains(st.Error, "127.0.0.1:19350") || !strings.Contains(st.Error, "already in use") {
		t.Fatalf("error = %q", st.Error)
	}
	if st.RetryInS <= 0 {
		t.Fatalf("retry_in_s = %v, want > 0", st.RetryInS)
	}

	time.Sleep(1500 * time.Millisecond) // за это время быстрый backoff дал бы ещё ~5 попыток
	if r := s.Status().Restarts; r != 1 {
		t.Fatalf("restarts = %d after 1.5s, want 1 (no fast restart loop)", r)
	}

	cancel()
	<-done
	if st := s.Status(); st.State != StateStopped {
		t.Fatalf("after stop: %+v", st)
	}
}

// Обычное падение: restarting с текстом ошибки из вывода и растущий backoff.
func TestCrashBackoff(t *testing.T) {
	s := newFake(t, "crash")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, "3 restarts", 10*time.Second, func() bool { return s.Status().Restarts >= 3 })
	st := s.Status()
	if st.State != StateRestarting && st.State != StateStarting {
		t.Fatalf("state = %s", st.State)
	}
	if !strings.Contains(st.Error, `unknown field "badKey"`) && st.State == StateRestarting {
		t.Fatalf("error = %q", st.Error)
	}

	cancel()
	<-done

	log, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), `unknown field "badKey"`) < 3 {
		t.Fatalf("mediamtx.log does not contain the child output:\n%s", log)
	}
}

func TestBackoffSequence(t *testing.T) {
	s := New("x", "x", "x", nil, testLogger())
	want := []time.Duration{1, 2, 5, 10, 30, 60, 60, 60}
	for i, w := range want {
		_, _, got := s.classifyExit(time.Hour, nil, nil, i+1)
		if got != w*time.Second {
			t.Errorf("attempt %d: %v, want %v", i+1, got, w*time.Second)
		}
	}
	// bind-ошибка после долгой работы — не "порт занят при старте", а обычное падение.
	st, _, _ := s.classifyExit(time.Hour, []string{"ERR listen tcp :8889: bind: x"}, nil, 1)
	if st != StateRestarting {
		t.Errorf("late bind error: state %s, want restarting", st)
	}
}

// Живой процесс: running, а при остановке ребёнок действительно убит.
func TestRunningAndStopKillsChild(t *testing.T) {
	s := newFake(t, "live")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor(t, "running", 5*time.Second, func() bool { return s.Status().State == StateRunning })
	pid := childPIDs(t)
	if len(pid) != 1 {
		t.Fatalf("expected one fake child, found %v", pid)
	}

	cancel()
	<-done
	waitFor(t, "child gone", 5*time.Second, func() bool { return !processAlive(pid[0]) })
	if st := s.Status(); st.State != StateStopped {
		t.Fatalf("after stop: %+v", st)
	}
}

// Аварийное завершение родителя (TerminateProcess, как taskkill /f): ребёнок
// в kill-on-close job умирает вместе с ним.
func TestChildDiesWithCrashedParent(t *testing.T) {
	childPID := startParentAndKill(t, "parent")
	waitFor(t, "orphan child to be killed", 5*time.Second, func() bool { return !processAlive(childPID) })
}

// Контроль: без job тот же сценарий оставляет сироту — значит, тест выше
// проверяет именно job, а не то, что ребёнок умирает сам по себе.
func TestChildSurvivesWithoutJob(t *testing.T) {
	childPID := startParentAndKill(t, "parent-nobind")
	time.Sleep(500 * time.Millisecond)
	if !processAlive(childPID) {
		t.Fatal("child died without a job — the job test above would prove nothing")
	}
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(childPID)); err == nil {
		_ = windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

// startParentAndKill запускает процесс-"multirtmp" в режиме mode, ждёт PID его
// ребёнка, убивает родителя через TerminateProcess и возвращает PID ребёнка.
func startParentAndKill(t *testing.T, mode string) int {
	t.Helper()
	parent := exec.Command(os.Args[0])
	parent.Env = append(os.Environ(), fakeEnv+"="+mode)
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer parent.Process.Kill()

	buf := make([]byte, 256)
	n, _ := out.Read(buf)
	line := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(line, "CHILD ") {
		t.Fatalf("parent said %q", line)
	}
	childPID, err := strconv.Atoi(strings.TrimPrefix(line, "CHILD "))
	if err != nil {
		t.Fatal(err)
	}
	if !processAlive(childPID) {
		t.Fatal("child is not running before the parent is killed")
	}

	if err := parent.Process.Kill(); err != nil { // TerminateProcess
		t.Fatal(err)
	}
	_ = parent.Wait()
	return childPID
}

func TestTailWriterAndLastErrorLine(t *testing.T) {
	var sink strings.Builder
	tw := &tailWriter{w: &sink, max: 3}
	_, _ = tw.Write([]byte("a\r\nb\nERR first\nc"))
	_, _ = tw.Write([]byte("\nd\n"))
	if got := tw.lines(); strings.Join(got, "|") != "ERR first|c|d" {
		t.Fatalf("tail = %q", got)
	}
	if sink.String() != "a\r\nb\nERR first\nc\nd\n" {
		t.Fatalf("sink = %q", sink.String())
	}
	if got := lastErrorLine([]string{"2026/11/14 21:15:03 ERR boom", "2026/11/14 21:15:03 INF waiting for running hooks"}); got != "boom" {
		t.Fatalf("lastErrorLine = %q", got)
	}
	// Без ERR-строк (процесс убили снаружи) — не подсовывать случайную INF-строку.
	if got := lastErrorLine([]string{"2026/11/14 21:18:45 INF [WebRTC] [session 1] closed: x"}); got != "" {
		t.Fatalf("lastErrorLine without ERR = %q, want empty", got)
	}
	s := New("x", "x", "x", nil, testLogger())
	if _, msg, _ := s.classifyExit(time.Minute, []string{"INF something"}, fmt.Errorf("exit status 1"), 1); msg != "process exited: exit status 1" {
		t.Fatalf("msg = %q", msg)
	}
}

// Ошибка записи в лог не должна останавливать чтение вывода (иначе
// MediaMTX повис бы на записи в pipe).
func TestTailWriterNeverFails(t *testing.T) {
	calls := 0
	tw := &tailWriter{w: failingWriter{}, max: 5, onErr: func(error) { calls++ }}
	for i := 0; i < 3; i++ {
		n, err := tw.Write([]byte("line\n"))
		if err != nil || n != 5 {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if calls != 1 {
		t.Fatalf("onErr called %d times, want 1", calls)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("disk full") }

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// childPIDs — PID прямых потомков текущего процесса.
func childPIDs(t *testing.T) []int {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)

	self := uint32(os.Getpid())
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var pids []int
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID == self && processAlive(int(pe.ProcessID)) {
			pids = append(pids, int(pe.ProcessID))
		}
	}
	return pids
}

// Переменные окружения доходят до процесса MediaMTX, и функция env
// вызывается заново перед каждым запуском (адаптеры могли смениться).
func TestEnvPassedOnEveryStart(t *testing.T) {
	s := newFake(t, "env")
	calls := 0
	s.env = func() []string {
		calls++
		return []string{fmt.Sprintf("MTX_WEBRTCIPSFROMINTERFACESLIST=Ethernet-%d", calls)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, "2 starts", 5*time.Second, func() bool { return s.Status().Restarts >= 2 })
	cancel()
	<-done

	log, err := os.ReadFile(s.logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"=Ethernet-1", "=Ethernet-2"} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("mediamtx.log lacks %q:\n%s", want, log)
		}
	}
}
