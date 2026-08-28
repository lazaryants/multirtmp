// Package buildinfo содержит версию сборки — заполняется через
// -ldflags="-X multirtmp/internal/buildinfo.Version=... -X ...Commit=..."
// в build.ps1. Отдельный пакет (не main), чтобы internal/api тоже мог
// отдавать эти значения в /status без зависимости от cmd/multirtmp.
package buildinfo

var (
	Version = "dev"
	Commit  = "unknown"
)
