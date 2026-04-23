package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0xsequence/ethkit/ethrpc"
	"github.com/0xsequence/ethkit/ethtxn"
	"github.com/0xsequence/ethkit/ethwallet"
	"github.com/0xsequence/ethkit/go-ethereum"
	"github.com/0xsequence/ethkit/go-ethereum/accounts/abi"
	"github.com/0xsequence/ethkit/go-ethereum/common"
	"github.com/0xsequence/ethkit/go-ethereum/core/types"
	sequence "github.com/0xsequence/go-sequence"
	v3 "github.com/0xsequence/go-sequence/core/v3"
	"github.com/0xsequence/go-sequence/relayer"
	"github.com/0xsequence/go-sequence/services/keymachine"
)

// ---------------------------------------------------------------------------
// Constants & ABI definitions
// ---------------------------------------------------------------------------

const (
	defaultConfigPath   = "config.json"
	defaultDirectoryURL = "https://keymachine.sequence.app"
	waitTimeout         = 5 * time.Minute
)

const (
	erc20TokenABIJSON   = `[{"inputs":[{"internalType":"address","name":"account","type":"address"}],"name":"balanceOf","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"address","name":"to","type":"address"},{"internalType":"uint256","name":"value","type":"uint256"}],"name":"transfer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}]`
	mintFunctionABIJSON = `[{"type":"function","name":"mint","inputs":[{"name":"to","type":"address"},{"name":"tokenId","type":"uint256"},{"name":"amount","type":"uint256"},{"name":"data","type":"bytes"}],"outputs":[],"stateMutability":"nonpayable"}]`
)

var (
	erc20TokenABI = mustLoadABI(erc20TokenABIJSON)
	mintFunction  = mustLoadABI(mintFunctionABIJSON)
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type appConfig struct {
	ProjectAccessKey string `json:"projectAccessKey"`
	PrivateKey       string `json:"privateKey"`
	ChainID          int64  `json:"chainId"`
	TargetAddress    string `json:"targetAddress"`
	NodeURL          string `json:"nodeUrl"`
	RelayerURL       string `json:"relayerUrl"`
	ExplorerURL      string `json:"explorerUrl"`
	DirectoryURL     string `json:"directoryUrl,omitempty"`
}

func (c *appConfig) validate() error {
	var missing []string
	if c.ProjectAccessKey == "" {
		missing = append(missing, "projectAccessKey")
	}
	if c.PrivateKey == "" {
		missing = append(missing, "privateKey")
	}
	if c.ChainID == 0 {
		missing = append(missing, "chainId")
	}
	if c.TargetAddress == "" {
		missing = append(missing, "targetAddress")
	}
	if c.NodeURL == "" {
		missing = append(missing, "nodeUrl")
	}
	if c.RelayerURL == "" {
		missing = append(missing, "relayerUrl")
	}
	if c.ExplorerURL == "" {
		missing = append(missing, "explorerUrl")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config values: %s", strings.Join(missing, ", "))
	}
	if !common.IsHexAddress(c.TargetAddress) {
		return fmt.Errorf("invalid target address: %s", c.TargetAddress)
	}
	if _, err := normalizePrivateKey(c.PrivateKey); err != nil {
		return err
	}
	return nil
}

func loadConfig(path string) (*appConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg appConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// ---------------------------------------------------------------------------
// Async transaction result
// ---------------------------------------------------------------------------

// txResult holds the outcome of a single relayed transaction. Used in both
// sync and async paths to collect results uniformly.
type txResult struct {
	Index     int
	TokenID   int64
	MetaTxnID sequence.MetaTxnID
	TxHash    string
	Err       error
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(0)

	// Parse CLI flags.
	cfgPath := flag.String("config", defaultConfigPath, "path to the config file")
	async := flag.Bool("async", false, "send transactions in parallel instead of sequentially")
	count := flag.Int("count", 1, "number of mint transactions to send")
	flag.Parse()

	if *count < 1 {
		log.Fatalf("count must be >= 1, got %d", *count)
	}

	// Load and validate configuration.
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	nodeURL := withAccessKey(cfg.NodeURL, cfg.ProjectAccessKey)

	fmt.Println("--- Sequence V3 Transaction Example ---")
	fmt.Printf("Chain ID: %d\n", cfg.ChainID)
	if *async {
		fmt.Printf("Mode:     async (%d transactions)\n", *count)
	} else {
		fmt.Printf("Mode:     sync (%d transactions)\n", *count)
	}

	// -----------------------------------------------------------------------
	// Wallet setup — create the Sequence smart wallet from a single EOA signer.
	// -----------------------------------------------------------------------

	privateKey, _ := normalizePrivateKey(cfg.PrivateKey)
	eoa, err := ethwallet.NewWalletFromPrivateKey(privateKey)
	if err != nil {
		log.Fatalf("init signer: %v", err)
	}

	signer := sequence.NewSigner(eoa)
	wallet, err := sequence.V3NewWalletSingleOwner(signer, sequence.V3SequenceContext())
	if err != nil {
		log.Fatalf("init wallet: %v", err)
	}

	fmt.Printf("Signer Address (EOA): %s\n", eoa.Address().Hex())
	fmt.Printf("Smart Wallet Address: %s\n", wallet.Address().Hex())
	fmt.Printf("Target Address:       %s\n", cfg.TargetAddress)

	// -----------------------------------------------------------------------
	// Provider & relayer — connect the wallet to the network and relay service.
	// -----------------------------------------------------------------------

	provider, err := ethrpc.NewProvider(nodeURL)
	if err != nil {
		log.Fatalf("init provider: %v", err)
	}
	eoa.SetProvider(provider)

	relayerClient, err := relayer.NewClient(cfg.RelayerURL, cfg.ProjectAccessKey, provider)
	if err != nil {
		log.Fatalf("init relayer: %v", err)
	}

	if err := wallet.Connect(provider, relayerClient); err != nil {
		log.Fatalf("connect wallet: %v", err)
	}

	// -----------------------------------------------------------------------
	// Publish wallet config to Keymachine (idempotent).
	// -----------------------------------------------------------------------

	if err := publishWalletConfig(ctx, wallet, cfg); err != nil {
		fmt.Printf("Note: Could not publish config (might already exist). Continuing... (%v)\n", err)
	} else {
		fmt.Println("Wallet configuration published to directory.")
	}

	// -----------------------------------------------------------------------
	// Deploy the wallet on-chain if it is still counterfactual.
	// -----------------------------------------------------------------------

	fmt.Println("Checking wallet deployment status...")
	if err := ensureWalletDeployed(ctx, wallet, provider, eoa); err != nil {
		log.Fatalf("deploy wallet: %v", err)
	}

	// -----------------------------------------------------------------------
	// Send transactions — choose sync or async path based on the -async flag.
	// -----------------------------------------------------------------------

	target := common.HexToAddress(cfg.TargetAddress)
	explorerBase := strings.TrimSuffix(cfg.ExplorerURL, "/")

	if *async {
		results := sendAsync(ctx, wallet, provider, target, *count)
		printResultsSummary(results, explorerBase)
	} else {
		results := sendSync(ctx, wallet, provider, target, *count)
		printResultsSummary(results, explorerBase)
	}
}

// ---------------------------------------------------------------------------
// Sync path — send transactions one at a time, blocking between each.
// ---------------------------------------------------------------------------

func sendSync(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, target common.Address, count int) []txResult {
	results := make([]txResult, 0, count)

	for i := range count {
		tokenID := int64(i + 1)
		fmt.Printf("\n[tx %d/%d] Sending mint for tokenId=%d...\n", i+1, count, tokenID)

		result := sendOneMint(ctx, wallet, provider, target, i, tokenID)
		results = append(results, result)

		if result.Err != nil {
			fmt.Printf("[tx %d/%d] Failed: %v\n", i+1, count, result.Err)
		} else {
			fmt.Printf("[tx %d/%d] Confirmed: %s\n", i+1, count, result.TxHash)
		}
	}

	return results
}

// ---------------------------------------------------------------------------
// Async path — fire all transactions concurrently and collect results.
// ---------------------------------------------------------------------------

func sendAsync(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, target common.Address, count int) []txResult {
	fmt.Printf("\nFiring %d transactions in parallel...\n", count)

	results := make([]txResult, count)
	var wg sync.WaitGroup

	for i := range count {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokenID := int64(idx + 1)
			results[idx] = sendOneMint(ctx, wallet, provider, target, idx, tokenID)
		}(i)
	}

	wg.Wait()
	return results
}

// ---------------------------------------------------------------------------
// Single mint transaction — shared by both sync and async paths.
// ---------------------------------------------------------------------------

// sendOneMint builds, relays, and waits for a single mint transaction.
// It returns a txResult capturing the outcome (success or error).
func sendOneMint(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, target common.Address, index int, tokenID int64) txResult {
	// Encode the mint(address,uint256,uint256,bytes) calldata.
	mintCalldata, err := encodeMintCalldata(wallet.Address(), big.NewInt(tokenID), big.NewInt(1), nil)
	if err != nil {
		return txResult{Index: index, TokenID: tokenID, Err: fmt.Errorf("encode calldata: %w", err)}
	}

	tx := &sequence.Transaction{
		To:            target,
		Value:         big.NewInt(0),
		GasLimit:      big.NewInt(0),
		Data:          mintCalldata,
		DelegateCall:  false,
		RevertOnError: true,
	}

	// Sign, attach fee payment, and relay via the Sequence relayer.
	metaTxnID, _, waitReceipt, err := sendTransactionsWithFees(ctx, wallet, provider, sequence.Transactions{tx})
	if err != nil {
		return txResult{Index: index, TokenID: tokenID, Err: fmt.Errorf("relay: %w", err)}
	}

	// Block until the chain confirms the transaction.
	receipt, err := waitForReceipt(ctx, waitReceipt)
	if err != nil {
		return txResult{Index: index, TokenID: tokenID, MetaTxnID: metaTxnID, Err: fmt.Errorf("wait: %w", err)}
	}

	return txResult{
		Index:     index,
		TokenID:   tokenID,
		MetaTxnID: metaTxnID,
		TxHash:    receipt.TxHash.Hex(),
	}
}

// ---------------------------------------------------------------------------
// Result summary
// ---------------------------------------------------------------------------

func printResultsSummary(results []txResult, explorerBase string) {
	fmt.Println("\n--- Results ---")
	fmt.Printf("%-6s %-10s %-68s %-10s\n", "Index", "TokenID", "TxHash", "Status")
	fmt.Println(strings.Repeat("-", 100))

	succeeded, failed := 0, 0
	for _, r := range results {
		status := "OK"
		txHash := r.TxHash
		if r.Err != nil {
			status = fmt.Sprintf("FAILED: %v", r.Err)
			txHash = "-"
			failed++
		} else {
			succeeded++
		}
		fmt.Printf("%-6d %-10d %-68s %s\n", r.Index+1, r.TokenID, txHash, status)
	}

	fmt.Printf("\nTotal: %d | Succeeded: %d | Failed: %d\n", len(results), succeeded, failed)

	if explorerBase != "" {
		for _, r := range results {
			if r.Err == nil {
				fmt.Printf("Explorer: %s/tx/%s\n", explorerBase, r.TxHash)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Transaction helpers — fee handling, signing, and relay
// ---------------------------------------------------------------------------

// sendTransactionsWithFees attaches a fee payment (if required by the relayer),
// signs the meta-transaction bundle, and sends it through the relayer.
func sendTransactionsWithFees(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, txs sequence.Transactions) (sequence.MetaTxnID, *types.Transaction, ethtxn.WaitReceipt, error) {
	txsWithFee, feeQuote, err := maybeAttachFeePayment(ctx, wallet, provider, txs)
	if err != nil {
		return "", nil, nil, err
	}

	signed, err := wallet.SignTransactions(ctx, txsWithFee)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sign transaction: %w", err)
	}

	if feeQuote != nil {
		return wallet.SendTransactions(ctx, signed, feeQuote)
	}
	return wallet.SendTransactions(ctx, signed)
}

// maybeAttachFeePayment queries the relayer for fee options. If fees are required,
// it picks the cheapest affordable option and prepends a fee payment transaction.
func maybeAttachFeePayment(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, txs sequence.Transactions) (sequence.Transactions, *sequence.RelayerFeeQuote, error) {
	feeOptions, feeQuote, err := wallet.FeeOptions(ctx, txs)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch fee options: %w", err)
	}

	if len(feeOptions) == 0 {
		return txs, feeQuote, nil
	}

	option, err := selectFeeOption(ctx, provider, wallet.Address(), feeOptions)
	if err != nil {
		return nil, nil, err
	}

	feeTxn, err := buildFeePaymentTransaction(option)
	if err != nil {
		return nil, nil, err
	}

	valueStr := "0"
	if option.Value != nil {
		valueStr = option.Value.String()
	}
	fmt.Printf("Including relayer fee payment of %s %s\n", valueStr, option.Token.Symbol)

	updated := make(sequence.Transactions, 0, len(txs)+1)
	updated = append(updated, feeTxn)
	updated = append(updated, txs...)
	return updated, feeQuote, nil
}

// encodeMintCalldata packs the arguments for mint(address,uint256,uint256,bytes).
func encodeMintCalldata(to common.Address, tokenID, amount *big.Int, data []byte) ([]byte, error) {
	if tokenID == nil || amount == nil {
		return nil, errors.New("tokenID and amount cannot be nil")
	}
	if data == nil {
		data = []byte{}
	}
	return mintFunction.Pack("mint", to, tokenID, amount, data)
}

// ---------------------------------------------------------------------------
// Fee option selection
// ---------------------------------------------------------------------------

// selectFeeOption iterates through the relayer's fee options and picks the
// cheapest one that the wallet can afford (checking on-chain balances).
func selectFeeOption(ctx context.Context, provider *ethrpc.Provider, walletAddr common.Address, options []*sequence.RelayerFeeOption) (*sequence.RelayerFeeOption, error) {
	var (
		selected    *sequence.RelayerFeeOption
		selectedVal *big.Int
	)

	for _, option := range options {
		canPay, err := hasSufficientBalance(ctx, provider, walletAddr, option)
		if err != nil {
			return nil, err
		}
		if !canPay {
			continue
		}

		value := option.Value
		if value == nil {
			value = big.NewInt(0)
		}

		if selected == nil || value.Cmp(selectedVal) < 0 {
			selected = option
			selectedVal = value
		}
	}

	if selected == nil {
		return nil, fmt.Errorf("no affordable fee options for wallet %s", walletAddr.Hex())
	}

	return selected, nil
}

// hasSufficientBalance checks whether the wallet holds enough of the given
// token (native or ERC-20) to cover the fee option's required value.
func hasSufficientBalance(ctx context.Context, provider *ethrpc.Provider, walletAddr common.Address, option *sequence.RelayerFeeOption) (bool, error) {
	required := option.Value
	if required == nil {
		required = big.NewInt(0)
	}

	if required.Sign() == 0 {
		return true, nil
	}

	if isNativeFeeOption(option) {
		balance, err := provider.BalanceAt(ctx, walletAddr, nil)
		if err != nil {
			return false, fmt.Errorf("native balance: %w", err)
		}
		return balance.Cmp(required) >= 0, nil
	}

	if option.Token.Type == sequence.ERC20_TOKEN && option.Token.ContractAddress != nil {
		balance, err := erc20BalanceOf(ctx, provider, *option.Token.ContractAddress, walletAddr)
		if err != nil {
			return false, err
		}
		return balance.Cmp(required) >= 0, nil
	}

	return false, fmt.Errorf("unsupported fee token type %d for %s", option.Token.Type, option.Token.Symbol)
}

// buildFeePaymentTransaction creates a Sequence transaction that pays the
// relayer fee — either as a native ETH transfer or an ERC-20 transfer.
func buildFeePaymentTransaction(option *sequence.RelayerFeeOption) (*sequence.Transaction, error) {
	feeTxn := &sequence.Transaction{
		DelegateCall:  false,
		RevertOnError: true,
	}

	if option.GasLimit != nil {
		feeTxn.GasLimit = cloneBigInt(option.GasLimit)
	}

	if isNativeFeeOption(option) {
		feeTxn.To = option.To
		feeTxn.Value = cloneBigInt(option.Value)
		return feeTxn, nil
	}

	if option.Token.Type != sequence.ERC20_TOKEN || option.Token.ContractAddress == nil {
		return nil, fmt.Errorf("unsupported fee token option")
	}

	calldata, err := erc20TokenABI.Pack("transfer", option.To, option.Value)
	if err != nil {
		return nil, fmt.Errorf("encode erc20 transfer: %w", err)
	}

	feeTxn.To = *option.Token.ContractAddress
	feeTxn.Value = big.NewInt(0)
	feeTxn.Data = calldata

	return feeTxn, nil
}

// ---------------------------------------------------------------------------
// ERC-20 balance helper
// ---------------------------------------------------------------------------

func erc20BalanceOf(ctx context.Context, provider *ethrpc.Provider, token common.Address, owner common.Address) (*big.Int, error) {
	calldata, err := erc20TokenABI.Pack("balanceOf", owner)
	if err != nil {
		return nil, fmt.Errorf("encode erc20 balanceOf: %w", err)
	}

	output, err := provider.CallContract(ctx, ethereum.CallMsg{To: &token, Data: calldata}, nil)
	if err != nil {
		return nil, fmt.Errorf("erc20 balanceOf call: %w", err)
	}

	results, err := erc20TokenABI.Unpack("balanceOf", output)
	if err != nil {
		return nil, fmt.Errorf("decode erc20 balanceOf: %w", err)
	}

	if len(results) == 0 {
		return nil, errors.New("erc20 balanceOf returned no results")
	}

	balance, ok := results[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected erc20 balance type %T", results[0])
	}

	return balance, nil
}

// ---------------------------------------------------------------------------
// Wallet lifecycle — config publishing and deployment
// ---------------------------------------------------------------------------

// publishWalletConfig pushes the wallet configuration to the Keymachine
// directory so other Sequence services can resolve it. This is idempotent.
func publishWalletConfig(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], cfg *appConfig) error {
	dirURL := cfg.DirectoryURL
	if dirURL == "" {
		dirURL = defaultDirectoryURL
	}

	client := keymachine.NewClient(keymachine.Options{
		ProjectAccessKey:     cfg.ProjectAccessKey,
		KeymachineServiceURL: dirURL,
	})

	sessions, ok := client.(keymachine.Sessions)
	if !ok {
		return errors.New("keymachine client does not satisfy Sessions interface")
	}

	if err := wallet.SetSessions(sessions); err != nil {
		return fmt.Errorf("set sessions: %w", err)
	}

	if err := wallet.UpdateSessionsWallet(ctx); err != nil {
		return err
	}

	return nil
}

// ensureWalletDeployed checks whether the smart wallet is already on-chain.
// If not, it sends a deployment transaction from the EOA signer and waits
// for confirmation.
func ensureWalletDeployed(ctx context.Context, wallet *sequence.Wallet[*v3.WalletConfig], provider *ethrpc.Provider, deployer *ethwallet.Wallet) error {
	isDeployed, err := wallet.IsDeployed()
	if err != nil {
		return fmt.Errorf("check deployment: %w", err)
	}

	if isDeployed {
		fmt.Println("Wallet already deployed on-chain.")
		return nil
	}

	fmt.Println("Wallet is not deployed. Deploying from signer EOA...")

	_, factoryAddress, deployData, err := sequence.EncodeWalletDeployment(wallet.GetWalletConfig(), wallet.GetWalletContext())
	if err != nil {
		return fmt.Errorf("encode deployment: %w", err)
	}

	chainID, err := provider.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("fetch chain id: %w", err)
	}

	txReq := &ethtxn.TransactionRequest{
		To:       &factoryAddress,
		Data:     deployData,
		GasLimit: 3_000_000,
	}

	rawTx, err := deployer.NewTransaction(ctx, txReq)
	if err != nil {
		return fmt.Errorf("prepare deployment tx: %w", err)
	}

	signedTx, err := deployer.SignTransaction(rawTx, chainID)
	if err != nil {
		return fmt.Errorf("sign deployment tx: %w", err)
	}

	nativeTx, waitDeploy, err := deployer.SendTransaction(ctx, signedTx)
	if err != nil {
		return fmt.Errorf("send deployment tx: %w", err)
	}

	fmt.Printf("Deployment Sent! Tx Hash: %s\n", nativeTx.Hash().Hex())
	fmt.Println("Waiting for deployment confirmation...")

	receipt, err := waitForReceipt(ctx, waitDeploy)
	if err != nil {
		return fmt.Errorf("deployment confirmation: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("deployment tx failed with status %d", receipt.Status)
	}

	ok, err := wallet.IsDeployed()
	if err != nil {
		return fmt.Errorf("post-deploy check: %w", err)
	}
	if !ok {
		return errors.New("wallet still not deployed after deployment tx")
	}

	fmt.Printf("Wallet deployed at %s\n", wallet.Address().Hex())

	return nil
}

// ---------------------------------------------------------------------------
// Receipt waiting
// ---------------------------------------------------------------------------

// waitForReceipt blocks until the transaction is confirmed on-chain or
// the wait timeout is reached.
func waitForReceipt(ctx context.Context, waitFn ethtxn.WaitReceipt) (*types.Receipt, error) {
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	type receiptResult struct {
		receipt *types.Receipt
		err     error
	}

	resultCh := make(chan receiptResult, 1)
	go func() {
		receipt, err := waitFn(waitCtx)
		resultCh <- receiptResult{receipt: receipt, err: err}
	}()

	select {
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		return result.receipt, nil
	}
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

func normalizePrivateKey(key string) (string, error) {
	key = strings.TrimPrefix(strings.TrimSpace(key), "0x")
	if len(key) != 64 {
		return "", errors.New("private key must be 32 bytes (64 hex chars)")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", fmt.Errorf("invalid private key: %w", err)
	}
	return key, nil
}

func withAccessKey(baseURL, accessKey string) string {
	if strings.HasSuffix(baseURL, "/") {
		return baseURL + accessKey
	}
	return fmt.Sprintf("%s/%s", baseURL, accessKey)
}

func isNativeFeeOption(option *sequence.RelayerFeeOption) bool {
	return option.Token.ContractAddress == nil || *option.Token.ContractAddress == (common.Address{})
}

func cloneBigInt(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}

func mustLoadABI(def string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(def))
	if err != nil {
		panic(err)
	}
	return parsed
}
