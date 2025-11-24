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

func main() {
	log.SetFlags(0)

	cfgPath := flag.String("config", defaultConfigPath, "path to the config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	nodeURL := withAccessKey(cfg.NodeURL, cfg.ProjectAccessKey)

	fmt.Println("--- Sequence V3 Transaction Example ---")
	fmt.Printf("Chain ID: %d\n", cfg.ChainID)

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

	if err := publishWalletConfig(ctx, wallet, cfg); err != nil {
		fmt.Printf("Note: Could not publish config (might already exist). Continuing... (%v)\n", err)
	} else {
		fmt.Println("Wallet configuration published to directory.")
	}

	fmt.Println("Checking wallet deployment status...")
	if err := ensureWalletDeployed(ctx, wallet, provider, eoa); err != nil {
		log.Fatalf("deploy wallet: %v", err)
	}

	target := common.HexToAddress(cfg.TargetAddress)
	mintCalldata, err := encodeMintCalldata(wallet.Address(), big.NewInt(1), big.NewInt(1), nil)
	if err != nil {
		log.Fatalf("encode mint calldata: %v", err)
	}

	tx := &sequence.Transaction{
		To:            target,
		Value:         big.NewInt(0),
		GasLimit:      big.NewInt(0),
		Data:          mintCalldata,
		DelegateCall:  false,
		RevertOnError: true,
	}

	fmt.Println("Preparing transaction...")
	fmt.Println("Relaying transaction...")
	metaTxnID, _, waitReceipt, err := sendTransactionsWithFees(ctx, wallet, provider, sequence.Transactions{tx})
	if err != nil {
		log.Fatalf("relay transaction: %v", err)
	}

	fmt.Printf("Transaction Sent! OpHash: %s\n", metaTxnID)
	fmt.Println("Waiting for confirmation...")

	receipt, err := waitForReceipt(ctx, waitReceipt)
	if err != nil {
		log.Fatalf("wait for confirmation: %v", err)
	}

	explorerBase := strings.TrimSuffix(cfg.ExplorerURL, "/")
	fmt.Printf("\n✅ Transaction Confirmed!\n")
	fmt.Printf("Tx Hash:  %s\n", receipt.TxHash.Hex())
	fmt.Printf("Explorer: %s/tx/%s\n", explorerBase, receipt.TxHash.Hex())
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

	signedTx, err := deployer.SignTx(rawTx, chainID)
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

func encodeMintCalldata(to common.Address, tokenID, amount *big.Int, data []byte) ([]byte, error) {
	if tokenID == nil || amount == nil {
		return nil, errors.New("tokenID and amount cannot be nil")
	}
	if data == nil {
		data = []byte{}
	}
	return mintFunction.Pack("mint", to, tokenID, amount, data)
}

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
