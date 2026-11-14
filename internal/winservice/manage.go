package winservice

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// recoveryActions — restart on 1st failure after 5s, 2nd after 30s, 3rd+ (и
// далее) после 60s.
var recoveryActions = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
}

// recoveryResetPeriod — сутки: счётчик отказов обнуляется, если сутки не
// было ни одного падения (в секундах — так их ждёт SetRecoveryActions).
const recoveryResetPeriod = 24 * 60 * 60

// Install регистрирует службу name (автозапуск), запускающую exePath с args,
// и настраивает recovery: restart на всех трёх видах отказа — в т.ч. на
// "чистой" остановке с ненулевым кодом, не только на крэше процесса (см.
// SetRecoveryActionsOnNonCrashFailures) — иначе PermanentError-разграничение
// в handler.go было бы бессмысленным: обычные фатальные ошибки тоже должны
// приводить к рестарту.
func Install(name, displayName, description, exePath string, args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(name); err == nil {
		_ = existing.Close()
		return fmt.Errorf("winservice: service %q already exists", name)
	}

	s, err := m.CreateService(name, exePath, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: displayName,
		Description: description,
	}, args...)
	if err != nil {
		return fmt.Errorf("winservice: failed to create service: %w", err)
	}
	defer s.Close()

	if err := s.SetRecoveryActions(recoveryActions, recoveryResetPeriod); err != nil {
		return fmt.Errorf("winservice: failed to set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("winservice: failed to enable recovery on non-crash failures: %w", err)
	}

	// Не фатально, если не получилось: служба всё равно установлена и
	// работает, просто PermanentError не попадёт в Event Log (в файловый лог
	// он попадёт в любом случае).
	_ = eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info)

	return nil
}

// Uninstall останавливает (если запущена) и удаляет службу.
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("winservice: service %q not found: %w", name, err)
	}
	defer s.Close()

	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		if err := stopAndWait(s); err != nil {
			return fmt.Errorf("winservice: failed to stop service before removing it: %w", err)
		}
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("winservice: failed to delete service: %w", err)
	}

	_ = eventlog.Remove(name)

	return nil
}

// StartService запускает уже установленную службу.
func StartService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("winservice: service %q not found: %w", name, err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("winservice: failed to start service: %w", err)
	}

	return nil
}

// StopService останавливает запущенную службу и ждёт фактической остановки.
func StopService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("winservice: service %q not found: %w", name, err)
	}
	defer s.Close()

	return stopAndWait(s)
}

func stopAndWait(s *mgr.Service) error {
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("failed to send stop control: %w", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return fmt.Errorf("failed to query status: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}

	return fmt.Errorf("service did not stop within 20s")
}

// QueryStatus возвращает текущее состояние службы человекочитаемой строкой.
func QueryStatus(name string) (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return "", fmt.Errorf("winservice: service %q not found: %w", name, err)
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return "", fmt.Errorf("winservice: failed to query status: %w", err)
	}

	return stateString(status.State), nil
}

func stateString(st svc.State) string {
	switch st {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue_pending"
	case svc.PausePending:
		return "pause_pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", st)
	}
}

// QueryStatusReadOnly — то же, что QueryStatus, но с минимальными правами
// (SC_MANAGER_CONNECT + SERVICE_QUERY_STATUS): работает под обычным
// пользователем без прав администратора. QueryStatus идёт через
// mgr.Connect()/OpenService, которые запрашивают полный доступ к SCM и
// службе, — у оператора без админ-прав это "Access is denied". Нужна
// подкоманде open, которую запускают ярлыком с рабочего стола.
//
// installed == false (без ошибки), если службы с таким именем нет.
func QueryStatusReadOnly(name string) (state string, installed bool, err error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", false, fmt.Errorf("winservice: failed to connect to SCM: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", false, fmt.Errorf("winservice: invalid service name %q: %w", name, err)
	}

	h, err := windows.OpenService(scm, namePtr, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("winservice: failed to open service %q: %w", name, err)
	}
	defer windows.CloseServiceHandle(h)

	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "", true, fmt.Errorf("winservice: failed to query status: %w", err)
	}

	return stateString(svc.State(st.CurrentState)), true, nil
}
