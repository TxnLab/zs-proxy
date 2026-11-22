//go:build (darwin && !cgo) || (!windows && !linux && !darwin)

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

// processExe has no portable implementation here: a CGO_ENABLED=0 darwin build
// (no proc_pidpath) and other unixes we don't target (freebsd, openbsd, …). It
// reports ok=false so ProcessMatches fails open — a wedged-but-legitimate daemon
// stays stoppable. Windows has its own stub in proc_windows.go.
func processExe(int) (string, bool) { return "", false }
