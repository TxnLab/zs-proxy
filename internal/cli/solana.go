/*
 * Copyright (c) 2026. TxnLab Inc.
 */

package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/TxnLab/zerosignal/go/keystore"
	"github.com/TxnLab/zerosignal/go/solana"
	"github.com/TxnLab/zerosignal/go/solana/rpc"
	"github.com/TxnLab/zerosignal/go/solana/zsescrow"
	"github.com/TxnLab/zerosignal/go/zs"

	"github.com/TxnLab/zs-proxy/internal/app"
	"github.com/TxnLab/zs-proxy/internal/config"
	"github.com/TxnLab/zs-proxy/internal/wallet"
)

// solanaEnv is runtimeEnv on the Solana rail: what the wallet commands share
// there. The mint and decimals are the program Config's, as the SDK's wallet
// reads them, so the CLI never draws on a mint the program doesn't.
type solanaEnv struct {
	cfg      *config.Config
	meta     wallet.Metadata
	payer    solana.PublicKey
	endpoint string
	rpc      rpc.Client
	d        zsescrow.Deployment
	conf     *zsescrow.Config
}

// solanaTxFee is the lamports a one-signature transaction costs, the floor's
// fee term. The cluster sets it; this is its value on every cluster today.
const solanaTxFee = 5_000

// openSolanaRuntime is openRuntime on the Solana rail. Like it, it needs the
// wallet's address alone, so a wallet whose store doesn't open here can still
// be inspected, once the address is recorded.
func openSolanaRuntime(ctx context.Context, cfg *config.Config) (*solanaEnv, error) {
	if !wallet.DefaultExists() {
		return nil, errors.New("no wallet yet — run `zs-proxy wallet login`, `zs-proxy wallet import`, or `zs-proxy wallet new`")
	}
	meta, err := wallet.DefaultMetadata()
	if err != nil {
		return nil, err
	}
	addr, err := wallet.SolanaAddress(meta)
	if err != nil {
		return nil, err
	}
	payer, err := solana.ParsePublicKey(addr)
	if err != nil {
		return nil, fmt.Errorf("wallet's Solana address: %w", err)
	}
	dep, _, err := zs.ResolveNetwork(app.NetworkFor(cfg))
	if err != nil {
		return nil, fmt.Errorf("resolve the network: %w", err)
	}
	rc, err := rpc.NewClient(dep.Solana.RPCEndpoint)
	if err != nil {
		return nil, err
	}
	conf, err := zs.ReadSolanaConfig(ctx, rc, dep.Solana.Program)
	if err != nil {
		return nil, fmt.Errorf("read the program's Config: %w", err)
	}
	return &solanaEnv{
		cfg:      cfg,
		meta:     meta,
		payer:    payer,
		endpoint: dep.Solana.RPCEndpoint,
		rpc:      rc,
		d:        zsescrow.Deployment{Program: dep.Solana.Program, Mint: conf.USDCMint},
		conf:     conf,
	}, nil
}

// solanaRuntime is the Solana runtime when the resolved config is on that
// rail, and nil otherwise, for a command whose Algorand path opens its own.
func (f *proxyFlags) solanaRuntime(ctx context.Context) (*solanaEnv, error) {
	cfg, err := f.resolveConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.OnSolana() {
		return nil, nil
	}
	return openSolanaRuntime(ctx, cfg)
}

// solanaInfo is account.Info on the Solana rail.
type solanaInfo struct {
	lamports    uint64
	usdcAccount bool
	usdcFrozen  bool
	usdc        uint64
	// accountRent is a token account's rent, which creating the payer's USDC
	// account pays.
	accountRent uint64
}

// funded is account.Info.Funded on Solana: SOL enough to create the USDC
// account, when it is missing, and pay two fees, the account's and the rent
// pool's first deposit. The top-up itself needs more, which EnsureDeposit
// names when it can't afford it.
func (i solanaInfo) funded() bool {
	return i.lamports >= i.floor()
}

func (i solanaInfo) floor() uint64 {
	f := uint64(2 * solanaTxFee)
	if !i.usdcAccount {
		f += i.accountRent
	}
	return f
}

func (e *solanaEnv) read(ctx context.Context) (solanaInfo, error) {
	var i solanaInfo
	var err error
	if i.lamports, err = e.rpc.GetBalance(ctx, e.payer, rpc.Confirmed); err != nil {
		return i, fmt.Errorf("read SOL balance: %w", err)
	}
	if i.accountRent, err = e.rpc.GetMinimumBalanceForRentExemption(ctx, zsescrow.TokenAccountSize, rpc.Confirmed); err != nil {
		return i, fmt.Errorf("read token account rent: %w", err)
	}
	acct, err := e.rpc.GetAccountInfo(ctx, e.d.TokenAccount(e.payer), rpc.Confirmed)
	switch {
	case errors.Is(err, rpc.ErrNotFound):
		return i, nil
	case err != nil:
		return i, fmt.Errorf("read the USDC token account: %w", err)
	case acct.Owner != solana.TokenProgramID:
		return i, nil
	}
	t, err := zsescrow.DecodeTokenAccount(acct.Data)
	if err != nil {
		return i, fmt.Errorf("read the USDC token account: %w", err)
	}
	i.usdcAccount, i.usdcFrozen, i.usdc = true, t.State == zsescrow.TokenFrozen, t.Amount
	return i, nil
}

// pool reads the payer's rent pool without a signer.
func (e *solanaEnv) pool(ctx context.Context) (zs.SolanaRentPoolStatus, error) {
	return zs.ReadSolanaRentPool(ctx, e.rpc, e.d, e.payer)
}

// wallet is the SDK's Solana wallet for the payer, for the commands that
// sign; building it unlocks the store.
func (e *solanaEnv) wallet(ctx context.Context) (*zs.SolanaWallet, error) {
	sk, err := buildSolanaKeystore(ctx)
	if err != nil {
		return nil, err
	}
	if !sk.HasAccount(e.payer.String()) {
		return nil, fmt.Errorf("no key for the wallet's Solana address %s is loaded", e.payer)
	}
	return zs.NewSolanaWallet(ctx, zs.SolanaWalletConfig{
		RPC:          e.rpc,
		Deployment:   e.d,
		Signer:       sk.Signer(e.payer.String()),
		DepositSlots: e.cfg.Zs.ConcurrentSlots,
	})
}

// testCluster is whether the RPC serves a cluster that hands out free SOL:
// devnet, or a validator on this machine. Only there does fund airdrop, or
// take a mint authority.
func (e *solanaEnv) testCluster(ctx context.Context) (bool, error) {
	g, err := e.rpc.GetGenesisHash(ctx)
	if err != nil {
		return false, fmt.Errorf("read the genesis hash: %w", err)
	}
	return rpc.FaucetCluster(g, e.endpoint), nil
}

// airdropLamports is what fund asks a test cluster's faucet for: enough for
// the USDC account, the rent pool's default working set and fees, and within
// what devnet's faucet grants at once.
const airdropLamports = 1_000_000_000

// airdrop asks the cluster's faucet for SOL and waits until it lands.
func (e *solanaEnv) airdrop(ctx context.Context) error {
	height, err := e.rpc.GetBlockHeight(ctx, rpc.Confirmed)
	if err != nil {
		return err
	}
	sig, err := rpc.RequestAirdrop(ctx, e.endpoint, e.payer, airdropLamports)
	if err != nil {
		return err
	}
	// The faucet chose the blockhash; one is valid for 150 blocks.
	res, err := rpc.Confirm(ctx, e.rpc, sig, height+151, rpc.Confirmed, rpc.ConfirmOptions{Poll: 500 * time.Millisecond})
	if err != nil {
		return err
	}
	if res.Err != nil {
		return fmt.Errorf("airdrop %s failed: %v", sig, res.Err)
	}
	return nil
}

// testUSDCUnits is what --mint-authority mints: 1000 USDC.
const testUSDCUnits = 1000

// mintTestUSDC mints test USDC to the payer's token account, signed and paid
// for by the mint's authority, which creates the account first when needed.
func (e *solanaEnv) mintTestUSDC(ctx context.Context, keyFile string) (string, uint64, error) {
	authority, err := keystore.ReadSolanaKeypairFile(keyFile)
	if err != nil {
		return "", 0, err
	}
	amount := uint64(testUSDCUnits)
	for range e.conf.USDCDecimals {
		amount *= 10
	}
	auth := solana.PublicKey(authority.Public().(ed25519.PublicKey))
	sig, err := sendSigned(ctx, e.rpc, authority, []solana.Instruction{
		solana.CreateAssociatedTokenAccountIdempotent(auth, e.payer, e.d.Mint),
		solana.MintTo(e.d.Mint, e.d.TokenAccount(e.payer), auth, amount),
	})
	return sig, amount, err
}

// sendSigned sends ixs paid for and signed by key alone, and waits until they
// land.
func sendSigned(ctx context.Context, rc rpc.Client, key ed25519.PrivateKey, ixs []solana.Instruction) (string, error) {
	bh, err := rc.GetLatestBlockhash(ctx, rpc.Confirmed)
	if err != nil {
		return "", err
	}
	m, err := solana.Compile(solana.PublicKey(key.Public().(ed25519.PublicKey)), bh.Hash, ixs, solana.Legacy)
	if err != nil {
		return "", err
	}
	tx := solana.NewTransaction(m)
	if err := tx.Sign(key); err != nil {
		return "", err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	sig, err := rc.SendTransaction(ctx, raw, rpc.SendOptions{PreflightCommitment: rpc.Confirmed})
	if err != nil {
		return "", err
	}
	res, err := rpc.Confirm(ctx, rc, sig, bh.LastValidBlockHeight, rpc.Confirmed, rpc.ConfirmOptions{})
	if err != nil {
		return sig.String(), err
	}
	if res.Err != nil {
		return sig.String(), fmt.Errorf("%s landed and failed: %v", sig, res.Err)
	}
	return sig.String(), nil
}

// lamportsSOL renders lamports as SOL, to the lamport.
func lamportsSOL(v uint64) string {
	return fmt.Sprintf("%d.%09d SOL", v/1_000_000_000, v%1_000_000_000)
}

// tokenUnits renders base units of a token with decimals.
func tokenUnits(v uint64, decimals uint8, symbol string) string {
	if decimals == 0 {
		return fmt.Sprintf("%d %s", v, symbol)
	}
	d := uint64(1)
	for range decimals {
		d *= 10
	}
	return fmt.Sprintf("%d.%0*d %s", v/d, int(decimals), v%d, symbol)
}

var plainDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// solToLamports parses a decimal SOL amount exactly; a float would round
// 0.1 SOL to a lamport off.
func solToLamports(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	// SetString also reads fractions, exponents and base prefixes, so 0x10
	// would withdraw 16 SOL; only a plain decimal is an amount.
	if !plainDecimal.MatchString(s) {
		return 0, fmt.Errorf("invalid SOL amount %q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("invalid SOL amount %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt64(1_000_000_000))
	if !r.IsInt() {
		return 0, fmt.Errorf("SOL amount %q is finer than a lamport", s)
	}
	n := r.Num()
	if !n.IsUint64() {
		return 0, fmt.Errorf("SOL amount %q is too large", s)
	}
	return n.Uint64(), nil
}

// printSolanaFundManual says where SOL and USDC come from when fund can't
// get them itself.
func printSolanaFundManual(out io.Writer, e *solanaEnv, testCluster bool) {
	fmt.Fprintf(out, "Send SOL (and USDC, mint %s) to:\n  %s\n", e.d.Mint, e.payer)
	if testCluster {
		fmt.Fprintln(out, "Devnet SOL: https://faucet.solana.com")
	}
}
