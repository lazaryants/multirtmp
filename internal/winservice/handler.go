package winservice

import (
	"context"
	"errors"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// handler implements svc.Handler: bridges SCM control requests (Stop,
// Shutdown, Interrogate) to ctx cancellation and back to a Win32 exit code.
type handler struct {
	run  RunFunc
	name string
}

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	s <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- h.run(ctx) }()

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	var runErr error

loop:
	for {
		select {
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				s <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				runErr = <-errCh
				break loop
			}

		case runErr = <-errCh:
			// Приложение остановилось само (не по запросу SCM) — например,
			// ingest.ListenAndServe не смог занять порт.
			break loop
		}
	}

	var permErr *PermanentError
	if errors.As(runErr, &permErr) {
		// Конфиг не годится — перезапуск не поможет, нужен человек. Пишем в
		// Event Log (лучшее старание — если источник не зарегистрирован через
		// Install, Open просто вернёт ошибку, которую тут же игнорируем: в
		// файловый лог ошибка уже попала раньше, это не единственное место).
		if l, err := eventlog.Open(h.name); err == nil {
			_ = l.Error(1, permErr.Error())
			_ = l.Close()
		}
		s <- svc.Status{State: svc.Stopped, Win32ExitCode: 0}
		return false, 0
	}

	if runErr != nil {
		s <- svc.Status{State: svc.Stopped, Win32ExitCode: 1}
		return false, 1
	}

	s <- svc.Status{State: svc.Stopped}
	return false, 0
}
