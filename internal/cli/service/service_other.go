//go:build !windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package service

// scmInstall / scmUninstall and the scm* control helpers are the Windows SCM
// entry points. On other OSes they are unreachable — Supported(), Install(),
// Uninstall(), and the runtime.GOOS switches in Installed()/Start()/Stop()/
// Restart()/Status() guard on runtime.GOOS — and exist only so the shared
// service.go compiles everywhere.
func scmInstall(Spec, bool) (string, bool, error) { return "", false, ErrUnsupported }

func scmUninstall() (string, error) { return "", ErrUnsupported }

func scmInstalled() (bool, error) { return false, nil }

func scmStart() error { return ErrUnsupported }

func scmStop() error { return ErrUnsupported }

func scmRestart() error { return ErrUnsupported }

func scmStatus() (ServiceStatus, error) { return ServiceStatus{}, ErrUnsupported }
