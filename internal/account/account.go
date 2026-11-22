/*
 * Copyright (c) 2026. TxnLab Inc.
 */

// Package account does the read-only on-chain balance reads and the USDC
// opt-in the CLI needs (status/doctor/fund). It talks to algod through the
// Algorand SDK client directly rather than the proto algod.Client facade,
// which intentionally exposes only the four escrow-flow operations and no
// account reads.
package account

import (
	"context"
	"fmt"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/transaction"

	"github.com/TxnLab/zerosignal/go/keystore"
)

// MinFundedMicroAlgos is the ALGO floor below which the payer can't even cover
// the bare account min-balance + the USDC opt-in slot + the fees to opt in.
// Conservative (0.3 ALGO). It does NOT cover the prepaid ticket-MBR pool
// deposit, which is a SEPARATE, LARGER, fully-recoverable requirement
// (≈ zs.concurrent_slots × mbrForTicket() ≈ 1.15 ALGO at the default 10
// slots, plus the local-schema opt-in MBR). `zs fund` surfaces that deposit
// requirement explicitly and the wallet's EnsureDeposit fails with a clear
// shortfall (zs.ErrDepositUnaffordable) rather than letting the deposit
// bounce opaquely at algod — so passing Funded() means "can onboard", not
// "can fund the full pool". See the plan's min-balance math.
const MinFundedMicroAlgos = 300_000

// Info is a snapshot of the payer account relevant to serving.
type Info struct {
	Exists      bool
	MicroAlgos  uint64
	USDC        uint64 // microUSDC held (0 if not opted in)
	OptedInUSDC bool
}

// Funded reports whether the account holds enough ALGO to operate.
func (i Info) Funded() bool { return i.MicroAlgos >= MinFundedMicroAlgos }

// Dial builds a read/write SDK algod client for endpoint+token (the resolved
// algod connection from config).
func Dial(endpoint, token string) (*algod.Client, error) {
	c, err := algod.MakeClient(endpoint, token)
	if err != nil {
		return nil, fmt.Errorf("algod client %q: %w", endpoint, err)
	}
	return c, nil
}

// Read fetches the account's ALGO balance and USDC holding/opt-in state.
func Read(ctx context.Context, c *algod.Client, addr string, usdcAsset uint64) (Info, error) {
	acct, err := c.AccountInformation(addr).Do(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("account info: %w", err)
	}
	usdc, optedIn := usdcFromAccount(acct, usdcAsset)
	return Info{
		Exists:      acct.Amount > 0 || len(acct.Assets) > 0,
		MicroAlgos:  acct.Amount,
		USDC:        usdc,
		OptedInUSDC: optedIn,
	}, nil
}

// usdcFromAccount is the pure holding-lookup core of Read.
func usdcFromAccount(acct models.Account, usdcAsset uint64) (amount uint64, optedIn bool) {
	if usdcAsset == 0 {
		return 0, false
	}
	for _, h := range acct.Assets {
		if h.AssetId == usdcAsset {
			return h.Amount, true
		}
	}
	return 0, false
}

// OptInUSDC submits a 0-amount asset transfer to self so the account can hold
// USDC — required before the first escrow open (which locks USDC = max_price).
// Idempotent: returns ("", nil) when already opted in. Needs ALGO for the fee
// plus the +0.1 ALGO MBR the new asset slot costs.
func OptInUSDC(ctx context.Context, c *algod.Client, ks *keystore.KeyStore, addr string, usdcAsset uint64) (txid string, err error) {
	if usdcAsset == 0 {
		return "", fmt.Errorf("no USDC asset id is configured for this network")
	}
	info, err := Read(ctx, c, addr, usdcAsset)
	if err != nil {
		return "", err
	}
	if info.OptedInUSDC {
		return "", nil
	}
	sp, err := c.SuggestedParams().Do(ctx)
	if err != nil {
		return "", fmt.Errorf("suggested params: %w", err)
	}
	txn, err := transaction.MakeAssetTransferTxn(addr, addr, 0, nil, sp, "", usdcAsset)
	if err != nil {
		return "", fmt.Errorf("build opt-in txn: %w", err)
	}
	txid, signed, err := ks.SignWithAccount(ctx, txn, addr)
	if err != nil {
		return "", fmt.Errorf("sign opt-in: %w", err)
	}
	if _, err := c.SendRawTransaction(signed).Do(ctx); err != nil {
		return "", fmt.Errorf("submit opt-in: %w", err)
	}
	if _, err := transaction.WaitForConfirmation(c, txid, 4, ctx); err != nil {
		return "", fmt.Errorf("confirm opt-in: %w", err)
	}
	return txid, nil
}
