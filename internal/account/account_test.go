/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package account

import (
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
)

func TestUsdcFromAccount(t *testing.T) {
	const usdc = 10458941
	acct := models.Account{
		Amount: 5_000_000,
		Assets: []models.AssetHolding{
			{AssetId: 12345, Amount: 1},
			{AssetId: usdc, Amount: 2_500_000},
		},
	}
	amt, optedIn := usdcFromAccount(acct, usdc)
	if !optedIn || amt != 2_500_000 {
		t.Errorf("got (%d, %v), want (2500000, true)", amt, optedIn)
	}

	// Not opted in.
	amt, optedIn = usdcFromAccount(models.Account{Amount: 1}, usdc)
	if optedIn || amt != 0 {
		t.Errorf("got (%d, %v), want (0, false)", amt, optedIn)
	}

	// No USDC asset configured for the network.
	if _, optedIn := usdcFromAccount(acct, 0); optedIn {
		t.Error("zero usdcAsset should never report opted-in")
	}
}

func TestInfo_Funded(t *testing.T) {
	if (Info{MicroAlgos: MinFundedMicroAlgos}).Funded() != true {
		t.Error("at floor should be funded")
	}
	if (Info{MicroAlgos: MinFundedMicroAlgos - 1}).Funded() != false {
		t.Error("below floor should be unfunded")
	}
}
