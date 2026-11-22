//go:build darwin && cgo

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package daemon

/*
#include <libproc.h>
*/
import "C"

import "unsafe"

// processExe reads the live process's executable path via libproc's
// proc_pidpath (the macOS equivalent of Linux's /proc/<pid>/exe). It needs cgo,
// which darwin release builds already enable for the keychain backend; a
// CGO_ENABLED=0 darwin build falls back to the stub (proc_exe_stub.go) and the
// guard fails open. proc_pidpath returns the byte length on success, <=0 on
// failure (pid gone / not permitted).
func processExe(pid int) (string, bool) {
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(C.int(pid), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return "", false
	}
	return string(buf[:n]), true
}
