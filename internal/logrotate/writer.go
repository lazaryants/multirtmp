// Package logrotate — простой ротирующий io.Writer по размеру файла: свой,
// без сторонней зависимости (обычно для этого берут lumberjack, но правило
// проекта — новые зависимости по согласованию, а ротация по размеру не
// настолько сложна, чтобы её не сделать самим).
package logrotate

import (
	"fmt"
	"os"
	"sync"
)

// Writer — io.Writer, пишущий в файл path; при достижении maxBytes закрывает
// текущий файл, сдвигает path.(N-1)->path.N ... path->path.1 (храня не
// больше maxFiles файлов всего, включая текущий) и открывает файл заново.
type Writer struct {
	path     string
	maxBytes int64
	maxFiles int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// New открывает (или создаёт) path и готовит ротацию: не больше maxBytes
// байт на файл, не больше maxFiles файлов в сумме (текущий + maxFiles-1
// архивных).
func New(path string, maxBytes int64, maxFiles int) (*Writer, error) {
	if maxFiles < 1 {
		maxFiles = 1
	}

	w := &Writer{path: path, maxBytes: maxBytes, maxFiles: maxFiles}

	if err := w.openLocked(); err != nil {
		return nil, err
	}

	return w, nil
}

func (w *Writer) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logrotate: failed to open %s: %w", w.path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logrotate: failed to stat %s: %w", w.path, err)
	}

	w.f = f
	w.size = info.Size()

	return nil
}

// Write реализует io.Writer. Ротирует ПЕРЕД записью, если текущая запись не
// поместится в лимит, — так что отдельная запись никогда не режется пополам
// между старым и новым файлом.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)

	return n, err
}

func (w *Writer) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("logrotate: failed to close %s: %w", w.path, err)
	}

	if w.maxFiles == 1 {
		// Архивов нет вовсе — просто начинаем текущий файл заново.
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logrotate: failed to remove %s: %w", w.path, err)
		}
		return w.openLocked()
	}

	// Самый старый архив выходит за лимит хранения (maxFiles всего, включая
	// текущий) — удаляем его перед сдвигом остальных.
	_ = os.Remove(w.archivePath(w.maxFiles - 1))

	for i := w.maxFiles - 2; i >= 1; i-- {
		src := w.archivePath(i)
		dst := w.archivePath(i + 1)

		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("logrotate: failed to rotate %s -> %s: %w", src, dst, err)
			}
		}
	}

	if err := os.Rename(w.path, w.archivePath(1)); err != nil {
		return fmt.Errorf("logrotate: failed to rotate %s -> %s: %w", w.path, w.archivePath(1), err)
	}

	return w.openLocked()
}

func (w *Writer) archivePath(n int) string {
	return fmt.Sprintf("%s.%d", w.path, n)
}

// Close закрывает текущий файл.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
