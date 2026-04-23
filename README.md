# Sequence V3 Backend Transactions (Go)

This repo contains a minimal Go program that demonstrates how to build and send Sequence V3 transactions from a backend process. It supports both **synchronous** (one-at-a-time) and **async parallel** transaction modes.

The program:

- Loads a local config file with your Sequence project access key, signer private key, and network metadata.
- Builds a single-owner Sequence smart wallet from your EOA.
- Publishes the wallet configuration to Keymachine (if needed) and deploys the wallet if it is still counterfactual.
- Encodes `mint(address to, uint256 tokenId, uint256 amount, bytes data)` calls against the configured `targetAddress` (expected to be an ERC-1155 contract deployed on-chain).
- Requests fee options from the Sequence relayer, picks the cheapest affordable option, prepends the fee payment, sends the transaction(s), and waits for receipts.

## Requirements

- Go 1.25.3+ (see `go.mod`).
- A Sequence project access key provisioned for the chosen network.
- An EOA private key funded on the same network (for the initial wallet deployment and/or paying relayer fees).
- A target contract that exposes the `mint` function shown above.
- Minter permissions assigned to the smart contract wallet on the ERC-1155 contract.

## Configuration

Copy the sample file and fill in the values that match your environment:

```sh
cp config.example.json config.json
```

| Field | Description |
| --- | --- |
| `projectAccessKey` | Access key from the Sequence project dashboard. Used for both the node and relayer. |
| `privateKey` | 32-byte hex string (with or without `0x`) for the EOA that will own the wallet. |
| `chainId` | Numeric chain ID the wallet should target. |
| `targetAddress` | Contract that exposes the `mint` function (typically an ERC-1155/Sequence-compatible mint helper). |
| `nodeUrl` | Sequence node base URL for the network (do **not** append the access key; the app does that automatically). |
| `relayerUrl` | Sequence relayer URL for the same network. |
| `explorerUrl` | Base URL of a block explorer; used only for printing a link. |
| `directoryUrl` | Optional Keymachine directory URL. Defaults to `https://keymachine.sequence.app`. |

> Tip: `config.example.json` is pre-populated with Arbitrum endpoints. Adjust the URLs to match the network you are targeting.

## Running the example

### Sync mode (default)

Send a single mint transaction and wait for confirmation:

```sh
go run main.go
```

Send multiple transactions sequentially:

```sh
go run main.go -count 3
```

### Async mode

Fire multiple mint transactions in parallel and collect all results:

```sh
go run main.go -async -count 5
```

### Flags

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `-config` | string | `config.json` | Path to the JSON config file. |
| `-async` | bool | `false` | Send transactions in parallel instead of sequentially. |
| `-count` | int | `1` | Number of mint transactions to send. Each uses a distinct `tokenId` (1 through N). |

## How it works

The important steps in `main.go` are:

1. **Configuration & wallet setup** — `loadConfig` validates the JSON, `sequence.NewSigner` wraps the EOA, and `sequence.V3NewWalletSingleOwner` constructs the smart wallet context.
2. **Publishing to Keymachine** — `publishWalletConfig` pushes the wallet config so other Sequence services can resolve it.
3. **Ensuring deployment** — `ensureWalletDeployed` sends the counterfactual deployment transaction when the wallet is not yet on-chain.
4. **Building the mint call** — `encodeMintCalldata` packs the call data for the configured `targetAddress`.
5. **Fee handling** — `maybeAttachFeePayment` inspects relayer fee options, checks balances (native or ERC-20), and prepends a fee payment transaction when required.
6. **Sending & waiting** — `sendTransactionsWithFees` signs the meta-transaction bundle, relays it, and `waitForReceipt` blocks (with timeout) until confirmation.

### Sync vs Async

Both paths share the same wallet setup, deployment, and `sendOneMint` function. The difference is orchestration:

- **Sync** (`sendSync`) — iterates through transactions one at a time, printing progress after each.
- **Async** (`sendAsync`) — launches a goroutine per transaction using `sync.WaitGroup`, waits for all to complete, then prints a summary table.

Each transaction uses a distinct `tokenId` (1 through N) so they are unique on-chain.

## Troubleshooting

- **`missing required config values`** — Ensure every field above (except `directoryUrl`) is set.
- **`invalid target address` / private key errors** — Confirm the address is a checksummed hex string and the private key is 64 hex chars.
- **`no affordable fee options`** — Fund the wallet (in native tokens or the ERC-20 the relayer quotes) so it can pay the relayer.
- **Wallet already deployed** — This is expected if you reused the same config; the script will skip deployment and continue.
