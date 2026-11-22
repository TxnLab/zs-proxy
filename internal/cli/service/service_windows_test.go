//go:build windows

/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package service

import (
	"strings"
	"testing"
)

// TestLogonRightError_GrantFailed: when the auto-grant failed, the guidance
// points at the manual "Log on as a service" grant (secpol / services.msc) and
// names both the account and the service.
func TestLogonRightError_GrantFailed(t *testing.T) {
	err := logonRightError(`MACHINE\alice`, errInjected)
	msg := err.Error()
	for _, want := range []string{
		`MACHINE\alice`,
		"Log on as a service",
		"secpol.msc",
		WindowsServiceName,
		"injected",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("grant-failed guidance missing %q:\n%s", want, msg)
		}
	}
}

// TestLogonRightError_PasswordCause: when the right is in place (grantErr nil)
// but the logon was still refused, the guidance blames the password.
func TestLogonRightError_PasswordCause(t *testing.T) {
	msg := logonRightError(`MACHINE\bob`, nil).Error()
	if !strings.Contains(msg, "password") {
		t.Errorf("password-cause guidance should mention the password:\n%s", msg)
	}
	if !strings.Contains(msg, `MACHINE\bob`) {
		t.Errorf("guidance should name the account:\n%s", msg)
	}
}

// TestGrantServiceLogonRight_BadAccount: a non-existent account fails at the SID
// lookup, before any LSA call — so this is exercisable without elevation.
func TestGrantServiceLogonRight_BadAccount(t *testing.T) {
	err := grantServiceLogonRight(`MACHINE\no-such-account-zzz-` + t.Name())
	if err == nil {
		t.Fatal("expected an error for a non-existent account")
	}
	if !strings.Contains(err.Error(), "look up SID") {
		t.Errorf("error should be a SID-lookup failure, got: %v", err)
	}
}

var errInjected = injectedErr("injected")

type injectedErr string

func (e injectedErr) Error() string { return string(e) }
