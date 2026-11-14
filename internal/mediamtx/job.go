package mediamtx

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killOnCloseJob — Job Object с JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: когда
// закрывается последний хэндл на job (то есть когда умирает процесс, который
// его держит, — как угодно, включая аварию и taskkill /f), Windows убивает
// все процессы в нём. Хэндл намеренно не закрывается до конца жизни
// процесса.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return job, nil
}

var (
	selfJobMu sync.Mutex
	selfJobOK bool
)

// BindChildrenToProcess помещает ТЕКУЩИЙ процесс в kill-on-close job. Все
// процессы, которые он запустит после этого, попадают в тот же job
// автоматически, ещё до того, как успеют выполнить хоть одну инструкцию, —
// поэтому при любом завершении multirtmp (в т.ч. аварийном) дочерний
// MediaMTX не останется сиротой.
//
// Привязывать job к самому ребёнку после запуска хуже: Go не умеет
// создавать процесс сразу в job, и между стартом ребёнка и
// AssignProcessToJobObject было бы окно, в котором падение multirtmp
// оставит сироту. Этот вариант остаётся только запасным (см.
// assignChildToJob), если привязать себя не удалось — например, процесс
// уже в job, который не разрешает вложенные (до Windows 8 или по политике
// запустившей программы).
//
// Вызывать один раз, в начале runApp — не в install/open и т.п.
func BindChildrenToProcess() error {
	selfJobMu.Lock()
	defer selfJobMu.Unlock()

	if selfJobOK {
		return nil
	}

	job, err := newKillOnCloseJob()
	if err != nil {
		return fmt.Errorf("mediamtx: %w", err)
	}

	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("mediamtx: AssignProcessToJobObject(self): %w", err)
	}

	selfJobOK = true
	return nil
}

func childrenBoundToProcess() bool {
	selfJobMu.Lock()
	defer selfJobMu.Unlock()
	return selfJobOK
}

// assignChildToJob — запасной путь: отдельный kill-on-close job только для
// ребёнка. Возвращённый хэндл держит этот процесс: при его аварийном
// завершении хэндл закрывает система и ребёнок умирает; после штатного
// выхода ребёнка вызывающий закрывает хэндл сам. Есть окно между стартом
// ребёнка и вызовом — см. BindChildrenToProcess.
func assignChildToJob(pid int) (windows.Handle, error) {
	job, err := newKillOnCloseJob()
	if err != nil {
		return 0, err
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("AssignProcessToJobObject(child): %w", err)
	}

	return job, nil
}
