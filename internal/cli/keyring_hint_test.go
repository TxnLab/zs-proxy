/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/TxnLab/zerosignal/go/wallet"
)

// The hint is for a denied or cancelled keychain prompt. A missing phrase or
// an existing wallet names the keychain too, and must not be told to approve a
// prompt that never appeared.
func TestKeyringHint(t *testing.T) {
	const hint = "keychain access was denied"
	denied := errors.New("keychain: User canceled the operation (-128)")
	if got := keyringHint(denied); !strings.Contains(got.Error(), hint) {
		t.Errorf("a cancelled prompt got no hint: %v", got)
	}
	for _, err := range []error{
		fmt.Errorf("%w: wallet.json names X, but the keychain store holds no phrase", sdk.ErrNotFound),
		fmt.Errorf("%w: the keychain store holds a recovery phrase", sdk.ErrExists),
	} {
		if got := keyringHint(err); got != err {
			t.Errorf("keyringHint(%v) = %v, want it unchanged", err, got)
		}
	}
	if keyringHint(nil) != nil {
		t.Error("keyringHint(nil) is not nil")
	}
}
