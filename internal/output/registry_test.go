package output

import (
	"context"
	"errors"
	"testing"
)

// Выход без url/ключа регистрируется без воркера: виден как not_configured,
// Start отклоняется ConflictError (в API — 409), Stop — тоже конфликт.
func TestRegistryNotConfigured(t *testing.T) {
	r := NewRegistry(context.Background())
	r.Register("youtube", nil)

	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Name != "youtube" || snap[0].State != StateNotConfigured {
		t.Fatalf("snapshot = %+v", snap)
	}

	err := r.Start("youtube")
	var ce ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("Start: got %v, want a ConflictError", err)
	}

	if err := r.Stop("youtube"); !errors.As(err, &ce) {
		t.Fatalf("Stop: got %v, want a ConflictError", err)
	}

	r.StartAll([]string{"youtube"}) // не должно ничего запустить и не должно паниковать
	if st := r.Snapshot()[0].State; st != StateNotConfigured {
		t.Fatalf("after StartAll: %s", st)
	}
	r.Wait()
}
