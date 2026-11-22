//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package service

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// scmInstall registers the proxy as a Windows service with the SCM and, when
// autostart, starts it. The serving binary detects it's running under the SCM
// (svc.IsWindowsService) and serves under svc.Run with WindowsServiceName, so
// that name must match what we register here. Returns the service name as the
// "path" for the caller's confirmation line.
func scmInstall(s Spec, autostart bool) (string, bool, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", false, fmt.Errorf("connect to service manager (run as Administrator?): %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(WindowsServiceName); err == nil {
		_ = existing.Close()
		return "", false, fmt.Errorf("service %q is already installed; run `zs-proxy proxy uninstall-service` first", WindowsServiceName)
	}

	startType := uint32(mgr.StartAutomatic)
	if !autostart {
		startType = uint32(mgr.StartManual)
	}
	cfg := mgr.Config{
		DisplayName: WindowsServiceDisplay,
		Description: WindowsServiceDesc,
		StartType:   startType,
		// Run as the installing user (resolved + password-prompted by the CLI) so
		// the service reads that user's Credential Manager wallet + per-user state
		// dir. Empty Account → SCM default (LocalSystem), which would see neither.
		ServiceStartName: s.Account,
		Password:         s.Password,
	}
	scmSvc, err := m.CreateService(WindowsServiceName, s.BinPath, cfg, s.Args...)
	if err != nil {
		return "", false, fmt.Errorf("create service: %w", err)
	}
	defer scmSvc.Close()

	// Restart on crash, mirroring the systemd Restart=on-failure posture.
	// Best-effort: a failure here doesn't undo a good install.
	_ = scmSvc.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	}, 60 /* reset the failure counter after 60s of health */)

	// Grant the run-as account the "Log on as a service" right. Without it the
	// SCM refuses the logon with ERROR_SERVICE_LOGON_FAILED (1069) — at Start
	// below, or whenever the user later starts it from services.msc. Creating a
	// service with a logon account does NOT auto-grant the right, so we do it
	// here via the LSA policy API. We already hold the admin rights it needs
	// (CreateService just succeeded), and it's idempotent (re-granting is a
	// no-op). LocalSystem (empty Account) already has the right — skip it.
	var grantErr error
	if s.Account != "" {
		grantErr = grantServiceLogonRight(s.Account)
	}

	if !autostart {
		// Not auto-starting (e.g. a file-backend wallet that needs its passphrase
		// wired in first). Surface a grant failure now — otherwise the user only
		// discovers it as a bare 1069 the first time they start the service.
		if grantErr != nil {
			return WindowsServiceName, false, logonRightError(s.Account, grantErr)
		}
		return WindowsServiceName, false, nil
	}
	if err := scmSvc.Start(); err != nil {
		// A logon failure means the account still can't log on as a service:
		// either the auto-grant above failed, or the password is wrong / blank /
		// changed since install. logonRightError tailors the guidance to which.
		// The service is installed regardless — the user fixes it and starts it.
		if s.Account != "" && errors.Is(err, windows.ERROR_SERVICE_LOGON_FAILED) {
			return WindowsServiceName, false, logonRightError(s.Account, grantErr)
		}
		return WindowsServiceName, false, fmt.Errorf("service created but failed to start: %w", err)
	}
	return WindowsServiceName, true, nil
}

// logonRightError builds the actionable error shown when a user-account service
// can't log on (1069). grantErr distinguishes the two causes: non-nil means the
// auto-grant of "Log on as a service" failed (point the user at the manual
// grant); nil means the right is in place, so the logon was refused for the
// other reason — a wrong / empty / changed password.
func logonRightError(account string, grantErr error) error {
	if grantErr != nil {
		return fmt.Errorf(
			"service registered to run as %q, but granting it the \"Log on as a service\" right failed: %v. "+
				"Grant it manually — secpol.msc → Local Policies → User Rights Assignment → \"Log on as a service\" → "+
				"add %s (on Windows Home, which has no secpol.msc, use services.msc → %s → Properties → Log On and "+
				"re-enter the password, which grants the right) — then run:  sc start %s",
			account, grantErr, account, WindowsServiceName, WindowsServiceName)
	}
	return fmt.Errorf(
		"service registered to run as %q and was granted the \"Log on as a service\" right, but Windows still refused "+
			"the logon (1069) — usually a wrong, empty, or since-changed password. Re-enter it in services.msc → %s → "+
			"Properties → Log On (which also re-confirms the right), then start the service — or reinstall: "+
			"`zs-proxy proxy uninstall-service` then `zs-proxy proxy install-service`",
		account, WindowsServiceName)
}

// Minimal LSA policy bindings — x/sys/windows doesn't wrap LsaOpenPolicy /
// LsaAddAccountRights, so call them through advapi32 directly (via
// syscall.SyscallN on proc.Addr(), the pointer-safe form — see grantServiceLogonRight).
var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy         = modadvapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights   = modadvapi32.NewProc("LsaAddAccountRights")
	procLsaClose              = modadvapi32.NewProc("LsaClose")
	procLsaNtStatusToWinError = modadvapi32.NewProc("LsaNtStatusToWinError")
)

const (
	// Access rights needed to add an account right to the local LSA policy.
	policyCreateAccount = 0x00000010
	policyLookupNames   = 0x00000800
	// seServiceLogonName is SE_SERVICE_LOGON_NAME — the privilege a service's
	// run-as account must hold for the SCM to log it on.
	seServiceLogonName = "SeServiceLogonRight"
)

// lsaUnicodeString is the LSA counted-string struct (lengths are in BYTES).
type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

// lsaObjectAttributes is passed (zeroed) to LsaOpenPolicy; only Length matters.
type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               *lsaUnicodeString
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// grantServiceLogonRight gives account the "Log on as a service" right via the
// local LSA policy, so the SCM can log the service on. Idempotent. Requires the
// caller to be elevated (it opens the policy with POLICY_CREATE_ACCOUNT).
func grantServiceLogonRight(account string) error {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return fmt.Errorf("look up SID for %q: %w", account, err)
	}

	// Call through syscall.SyscallN (not LazyProc.Call): the compiler only
	// guarantees a uintptr(unsafe.Pointer(&x)) argument keeps x alive across the
	// call for the SyscallN family — through the variadic .Call the pointers
	// could go stale if the stack moves. NTSTATUS is 32-bit, so truncate the
	// 64-bit return before the success check (its upper bits are undefined).
	var oa lsaObjectAttributes
	oa.Length = uint32(unsafe.Sizeof(oa))
	var policy windows.Handle
	st, _, _ := syscall.SyscallN(procLsaOpenPolicy.Addr(),
		0, // SystemName == NULL → the local machine
		uintptr(unsafe.Pointer(&oa)),
		uintptr(policyCreateAccount|policyLookupNames),
		uintptr(unsafe.Pointer(&policy)),
	)
	if uint32(st) != 0 {
		return fmt.Errorf("LsaOpenPolicy: %w", lsaError(st))
	}
	defer syscall.SyscallN(procLsaClose.Addr(), uintptr(policy))

	name, err := windows.UTF16FromString(seServiceLogonName)
	if err != nil {
		return err
	}
	right := lsaUnicodeString{
		Length:        uint16((len(name) - 1) * 2), // bytes, excluding the NUL
		MaximumLength: uint16(len(name) * 2),       // bytes, including the NUL
		Buffer:        &name[0],
	}
	st, _, _ = syscall.SyscallN(procLsaAddAccountRights.Addr(),
		uintptr(policy),
		uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(&right)),
		1, // CountOfRights
	)
	if uint32(st) != 0 {
		return fmt.Errorf("LsaAddAccountRights: %w", lsaError(st))
	}
	return nil
}

// lsaError turns an NTSTATUS returned by an Lsa* call into a Win32 error.
// LsaNtStatusToWinError reads only the low 32 bits, so the undefined upper bits
// of ntstatus don't matter here.
func lsaError(ntstatus uintptr) error {
	winErr, _, _ := syscall.SyscallN(procLsaNtStatusToWinError.Addr(), ntstatus)
	if winErr == 0 {
		return fmt.Errorf("NTSTATUS 0x%x", ntstatus)
	}
	return syscall.Errno(winErr)
}

// scmUninstall stops (best-effort) and deletes the service. A missing service
// is reported as an error so the caller can say "not installed".
func scmUninstall() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", fmt.Errorf("connect to service manager (run as Administrator?): %w", err)
	}
	defer m.Disconnect()

	scmSvc, err := m.OpenService(WindowsServiceName)
	if err != nil {
		return "", fmt.Errorf("service %q is not installed: %w", WindowsServiceName, err)
	}
	defer scmSvc.Close()

	// Stop first so Delete takes effect promptly; ignore "not running". The
	// service handler drains in-flight streams on Stop (the graceful path).
	if _, err := scmSvc.Control(svc.Stop); err == nil {
		waitForStop(scmSvc, 20*time.Second)
	}
	if err := scmSvc.Delete(); err != nil {
		return "", fmt.Errorf("delete service: %w", err)
	}
	return WindowsServiceName, nil
}

// waitForStop polls the service until it reports Stopped or timeout elapses.
func waitForStop(s *mgr.Service, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil || status.State == svc.Stopped {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// openService connects to the SCM and opens the proxy's service. The caller
// closes both (Disconnect via the returned manager, Close via the service).
func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to service manager: %w", err)
	}
	s, err := m.OpenService(WindowsServiceName)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, s, nil
}

// scmInstalled reports whether the SCM service exists.
func scmInstalled() (bool, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, fmt.Errorf("connect to service manager: %w", err)
	}
	defer windows.CloseServiceHandle(m)
	name, err := syscall.UTF16PtrFromString(WindowsServiceName)
	if err != nil {
		return false, err
	}
	s, err := windows.OpenService(m, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, nil
		}
		return false, fmt.Errorf("open service for query: %w", err)
	}
	_ = windows.CloseServiceHandle(s)
	return true, nil
}

// scmStart starts the installed SCM service (no extra args — its binPath/args
// were fixed at install time).
func scmStart() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

// scmStop stops the service and waits for the drain (the handler drains in-flight
// streams on Stop).
func scmStop() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	waitForStop(s, 20*time.Second)
	return nil
}

// scmRestart stops (waiting for the drain), then starts the service.
func scmRestart() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	if _, err := s.Control(svc.Stop); err == nil {
		waitForStop(s, 20*time.Second)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

// scmStatus reports the service's liveness from the SCM.
func scmStatus() (ServiceStatus, error) {
	m, s, err := openService()
	if err != nil {
		return ServiceStatus{}, nil // not installed / not openable ⇒ not running
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return ServiceStatus{}, fmt.Errorf("query service: %w", err)
	}
	return ServiceStatus{Running: st.State == svc.Running, PID: int(st.ProcessId)}, nil
}
