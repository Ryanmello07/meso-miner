# Bittensor Subnet 25 Operations & Commands Guide

This guide details operating procedures, command references, and telemetry monitoring for URNetwork Connect nodes participating in Bittensor Subnet 25.

---

## 1. Subnet 25 Architecture & Earning Mechanics

URNetwork Connect providers operate on a dual-incentive model:
1. **Network Provider Traffic**: Direct bandwidth earnings based on billable byte delivery and client session routing.
2. **Bittensor Subnet 25 Incentive Pool**: On-chain emissions distributed proportionally to verified miners based on Merkle-proven bandwidth scores across finalized payout epochs.

### Identity & Wallet Model
- **Network Account Token (`jwt`)**: The provider authenticates to the URNetwork control plane using a signed JWT stored at `~/.urnetwork/jwt` (or container volume `/home/urnet/.urnetwork/jwt`).
- **Claim Coldkey (`ss58`)**: Bittensor coldkey address (prefix `42`, e.g. `5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5`) registered with the platform to authorize and receive pool emissions.
- **Mirror Account**: EVM smart contract accounts mirrored on Subtensor via `evm:<H160_address>` hash derivation for non-custodial claims.

### Two Distinct Miner Tiers: Pool Providers vs. Top 200 Head Fleets

Subnet 25 operates a two-channel architecture under a hard 256-UID metagraph ceiling (`urfoundation/sn` WHITEPAPER §8.4, §11.4; `protocol/policy.go`):
`256 >= (Top-Level Head Miners ~200) + (NO-Pool UIDs) + (Validator UIDs)`

The 41% miner emission is divided into two distinct processes:

| Dimension | Tail Providers (Pool Process) | Top ~200 Fleets (Head Miner Process) |
|---|---|---|
| **On-Chain Identity** | **No UID.** Operates as a `client_id` inside an operator's pool. | **Holds its own Subnet 25 Miner UID** (claimed via `burned_register`). |
| **Scoring Basis** | Demand-coupled: `deposit × quality` across the operator's pool. | **Pure measured routable-IP breadth** (`score(u) = Σ 1/claim(h)` across verified /29 IPv4 & /48 IPv6 subnets). |
| **Emission & Payout** | **Custodied in EVM Contract** (`STSubnet.sol`). Requires Merkle inclusion proofs. | **Native Bittensor Protocol Emission** via Yuma Consensus credited as α stake directly to the coldkey. |
| **Payout Timing** | Finalized **once per epoch (~7 days / 50,400 blocks)**. | Deposited **every block (~12 seconds)**. |
| **Gas & Claiming** | **Manual Claim Required:** Operators submit an on-chain `claimMiner` transaction and pay gas. | **Zero Gas, Zero Claims:** Automated native emission. No EVM contract claims, no Merkle trees. |
| **Intermediary** | Operator commits the Merkle payout root (auditable on-chain). | **Trust-minimized:** Direct consensus-to-coldkey. No operator in the payout path. |
| **Setup Required** | Linked via `POST /sn/wallet` (or `provider wallet set`). | Requires **Dual-Signed Fleet Binding (`provider bind-head`)** linking provider server keys to the Bittensor Substrate hotkey. |
| **Lifecycle Tournament** | **The Baseline On-Ramp:** Guaranteed floor without UID registration burn. | **The Merit Apex:** Earns directly while in the top 200. If IP breadth falls below the cutoff, it is evicted by Yuma churn and **falls back into the pool**. |

---

## 2. Coldkey Registration & Setup

Before claiming Subnet 25 emissions, link your Bittensor coldkey to your network account using any of the following methods.

### Method 1: Single-Line curl (Direct API)
Run this single command on the machine running your provider:
```bash
curl -s -X POST "https://api.bringyour.com/sn/wallet" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $(cat ~/.urnetwork/jwt)" \
  -d '{"coldkey_ss58": "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5"}'
```
Returns `{}` on success.

> [!NOTE]
> This is the same unsigned request that `provider wallet set` sends. The platform is moving wallet binding to a signed consent, and whether it still accepts the unsigned request is a server-side policy. If it is refused, set the wallet in the URnetwork app or web account.

### Method 2: Host / Bare-Metal CLI
```bash
# Register coldkey for the local provider
provider wallet set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
```

`provider wallet set` and `provide --wallet=<coldkey>` send the unsigned network wallet request, which the platform may refuse under its signed wallet rules. Without `--legacy-network-wallet` they do not send it: `wallet set` exits 1, and `provide` logs the refusal and keeps providing. Set the wallet in the URnetwork app or web account, or add `--legacy-network-wallet` to send the unsigned request anyway.

### Method 3: Docker Deployments
Pass your coldkey directly via `docker-compose.yml` or container flags:
```yaml
services:
  provider:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.30.9
    command: ["provide", "--wallet=5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5", "--legacy-network-wallet"]
    volumes:
      - ur_config_1:/home/urnet/.urnetwork
```

### Beginner FAQ: Why Didn't My Browser Wallet Pop Up to Sign?

If you are used to Web3 dApps prompting your browser extension (like Talisman, SubWallet, Polkadot{.js}, or MetaMask) to approve transactions, you might wonder why linking your wallet here didn't trigger a popup:

1. **Linking your wallet (Step 1 — Direct Deposit Setup):**
   - Think of this like giving an employer your account number for direct deposit.
   - You are only telling URNetwork where to send your rewards. Because sharing a public wallet address cannot move or spend funds from your wallet, **no signature, password, or browser wallet popup is required**.
   - The system verifies your node using your local authentication file (`~/.urnetwork/jwt`).

2. **Claiming your rewards (Step 2 — Withdrawing Tokens):**
   - **This is where your browser wallet extension is used.**
   - At the end of every reward cycle (epoch), emissions are calculated and locked on the blockchain. When you go to claim those tokens to your wallet on the web dashboard or portal, your browser wallet extension will pop up and ask you to click **Approve / Sign** to finalize the on-chain transfer.

---

## 3. Real-Time Status & Telemetry (`sn-status`)

Use `sn-status` to inspect your global ranking, bandwidth delivery, coldkey linkage, and Top 200 cutoff eligibility in real time without exposing credentials in the process table.

### Commands
```bash
# Host CLI (systemd / bare-metal)
urnet-tools sn-status

# Docker CLI (delegates directly into running container)
urnet-docker sn-status

# Provider binary directly
provider sn-status

# JSON format for monitoring agents and Prometheus scrapers
urnet-tools sn-status --json
```

### Dashboard Output
```text
════════════════════════════════════════════════════════════════════════════════
  URNetwork Subnet 25 — Node & Miner Status
════════════════════════════════════════════════════════════════════════════════
  Provider:         urnetwork.service (State: /var/lib/urnetwork)
  Network:          miner-node-01 (f078e470-36d0-48e0-bb15-b5d1e2e9c1aa)
  Global Rank:      #7 [Public] — Tier 1 Elite (Rank #7 Globally)
  Net Bandwidth:    1,234,567.89 MiB (1205.63 GiB) provided
  Coldkey (SS58):   5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
  Coldkey (Hex):    0xa21b...
  Subnet Epoch:     #1054 (Start: #1054000, Finalize: #1054720)
  Contract:         0x5FbDB2315678afecb367f032d93F642f64180aa3 (Chain ID: 964)
  Payout Share:     4.85% (485 bps in Epoch #1053)
════════════════════════════════════════════════════════════════════════════════
```

### Ranking Tiers & Eligibility
- **Tier 1 Elite (Rank 1–10)**: Top global emission allocation; highest validator query preference.
- **Tier 2 High-Volume (Rank 11–50)**: High yield; fast validator probe acceptance.
- **Tier 3 Active (Rank 51–200)**: Subnet 25 emission cutoff eligibility boundary.
- **Unranked / Sub-200**: Displays exact distance to Top 200 emission threshold.

---

## 4. Epoch Payout Claims

Subnet 25 uses cryptographic Merkle tree payout roots committed on-chain at the conclusion of each epoch.

### Epoch Stages
1. **Active Epoch ($e$)**: Nodes stream billable bandwidth; validators collect signed trails.
2. **Commit Window**: Validator consensus commits the payout root hash.
3. **Dispute / Finalization**: Payout root is locked on-chain (`finalize_block`).
4. **Claim Window**: Verified claims become redeemable.

### Claim credential (required)
`provider claim` authenticates with the login of a client that served traffic, because only such a client has a payout. Pass exactly one of:

| Flag | Credential |
|---|---|
| `--store-client=<key>` | The client token of one identity in `~/.urnetwork/.client_jwts.json`. The key is a proxy address or `direct`. A bare address also finds the single credentialed identity at that address; when several proxy accounts share it the command refuses and asks for the full key (`address`, a `\x1f` byte, then the user). Pick an identity that served traffic. |
| `--provider-jwt=<path>` | A client token in a file. |
| `--legacy-coldkey=<coldkey_ss58>` | The network token plus this coldkey. Only for an epoch without a provider artifact. |

The network token alone is refused, and so is an expired client token or (with `--legacy-coldkey`) an expired network token (start the provider so it renews, or pass a fresher file). A command with no credential flag exits 1 and names these options. The examples below show `--store-client=direct`; substitute your own credential flag.

### Workflow 1: Air-Gapped / Offline Calldata (Recommended)
Generates ABI-encoded calldata and cryptographic inclusion proofs without exposing private keys on the provider host:
```bash
provider claim --store-client=direct --epoch=1053
```
Output yields ready-to-submit calldata compatible with `snclaim` or web3 wallets.

### Workflow 2: Direct On-Chain Submission
Submit the claim transaction directly with a local private key file and RPC endpoint:
```bash
provider claim \
  --store-client=direct \
  --epoch=1053 \
  --rpc=https://rpc.subtensor.network \
  --key_file=/path/to/coldkey_evm.key
```

### Dry Run Verification
Simulate verification and root matching without broadcasting transactions:
```bash
provider claim --store-client=direct --epoch=1053 --rpc=https://rpc.subtensor.network --dry-run
```

---

## 5. Head Node Delegation (`bind-head`)

Head nodes aggregate proofs from client nodes for on-chain batch verification.

> [!NOTE]
> `bind-head` and `unbind-head` are unchanged in this release (not removed, no rename); the `provider fleet` group in section 6 is added alongside them.

### Binding a Hotkey
```bash
provider bind-head \
  --manifest=/path/to/fleet-manifest.json \
  --hotkey_seed_file=/path/to/hotkey.seed \
  --valid_from_epoch=<n> \
  --valid_to_epoch=<n> \
  --rpc=<rpc_url> \
  --key_file=/path/to/evm.key
```
The manifest is the single source for the fleet identity. Without `--key_file` the command prints the ready-to-submit calldata for the offline path; with it, the signed binding is submitted through `--rpc`. `--client_id` and `--client_seed_file` override the provider's own client identity for the binding signature; `--dry-run` signs but sends nothing.

> [!IMPORTANT]
> The fleet binding is dual-signed: the sr25519 hotkey seed must match the manifest hotkey, and the Ed25519 client side signs with the provider's own client key. Both signatures are checked against the same digest.

### Unbinding a Hotkey
```bash
provider unbind-head \
  --manifest=/path/to/fleet-manifest.json \
  --effective_epoch=<n> \
  --rpc=<rpc_url> \
  --key_file=/path/to/evm.key
```
`--effective_epoch` is the future epoch at which the revocation takes effect.

---

## 6. Fleet Group (`provider fleet`)

> [!NOTE]
> The `fleet`, `wallet hotkey`, claim daemon and operator families documented in sections 6 to 9 ship with the v33 Bittensor port in this release. The grammar shown is this release's final grammar; these commands are new in this release.

The fleet group drives the head-tier lifecycle for one fleet node from a single canonical manifest: burned registration of the fleet hotkey, commitment publication, the dual-signed client and hotkey binding, local submission signing, status, and revocation. Every subcommand reads the same `urnetwork-fleet-manifest-v1` JSON (`--manifest=<path>`: schema, chain id, netuid, coordinator, fleet id, hotkey, generation and members). The manifest is the single source for the fleet identity.

```text
provider fleet manifest --manifest=<path>
    [-v...]
provider fleet register --manifest=<path> --hotkey_seed_file=<path> --coldkey_seed_file=<path> --substrate=<ws_url>...
    [--burn_limit_rao=<n>] [--fee_limit_rao=<n>] [--apply | --dry-run]
    [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
    [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
provider fleet publish --manifest=<path> --substrate=<ws_url>... --hotkey_seed_file=<path>
    [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
    [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
provider fleet bind --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> --valid_from_epoch=<e> --valid_to_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
    [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
provider fleet sign --manifest=<path> [--client_id=<hex>] [--client_seed_file=<path>] [--hotkey_seed_file=<path>] --valid_from_epoch=<e> --valid_to_epoch=<e>
    [-v...]
provider fleet status --manifest=<path> --client_id=<hex> --substrate=<ws_url>... --rpc=<rpc_url>...
    [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
    [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
provider fleet revoke --manifest=<path> --client_id=<hex> --client_seed_file=<path> --effective_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
    [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
```

- `provider fleet manifest` prints the manifest's canonical form, its commitment sha256 and its member count.
- `provider fleet register` registers the manifest hotkey on the manifest netuid (the head-tier fleet's own burned registration), signed by the fleet coldkey through `--substrate` (repeatable, tried in order). It is a plan unless `--apply` is given: without it, the command reads the live burn economics and the coldkey balance, refuses a burn above the cap, builds and signs the `register_limit` extrinsic, quotes its fee and stops, and broadcasts nothing (`--dry-run` selects the same no-broadcast plan). With `--apply` it submits the write and waits for canonical finality. `--burn_limit_rao` caps the registration burn (default: the burn observed at the finalized read); `--fee_limit_rao` caps the native transaction fee (default 10,000,000 rao, sized above the observed registration fee). Every broadcast is journaled under the provider state, and replaying a retained native write requires `--apply`.
- `provider fleet publish` writes the fleet commitment (the manifest commitment hash) to the chain through `--substrate` and verifies the write is finalized. It has no plan flag: publish always writes.
- `provider fleet bind` builds the manifest binding and submits it through the EVM relayer key (`--relayer_key_file`, which receives no binding ownership) via `--rpc`. Without `--dry-run` the transaction is sent and waited on; `--dry-run` stops at the preflight (chain id check, call simulation, gas estimate) and sends nothing.
- `provider fleet sign` builds the same binding and signs it locally: it prints the payload the Top 200 submission page takes, plus the hotkey signature when `--hotkey_seed_file` is given. It reads only local files, uses no RPC and no relayer key, and never prints or writes a seed.
- `provider fleet status` reads the on-chain state for `--client_id` (native commitment, coordinator record, epochs, UID) and checks it against the canonical manifest.
- `provider fleet revoke` schedules the binding's revocation at `--effective_epoch` (a future epoch), signed by the client key and submitted through `--relayer_key_file`; without `--dry-run` it is sent, and `--dry-run` stops at the preflight and sends nothing.
- `--mainnet-runtime-authority` with `--mainnet-runtime-authority-sha256` pins the independently reviewed mainnet fleet runtime authority document by its approved SHA-256 (64 lowercase hex digits), so mainnet writes only run against a runtime matching the reviewed pin; `--provisional-runtime-compatibility` with `--runtime-observation-dir` is the explicit testnet consumed-runtime profile; `--durable-volumes` with `--durable-volumes-sha256` declares the external storage and its reviewed digest (fleet writes require the owner-local schema).
- Seed files (`--hotkey_seed_file`, `--coldkey_seed_file`, `--client_seed_file`) are private files the caller owns; the CLI never creates them and never prints a seed. Keep the fleet coldkey seed off mining hosts.
- Broadcast defaults in this group: `register` plans (signs and quotes, no broadcast) unless `--apply`; `bind` and `revoke` send unless `--dry-run`; `publish` always writes and verifies finality; `manifest`, `sign` and `status` never write to any chain.

## 7. Hotkey Wallet (`provider wallet hotkey`)

One coldkey signature pays the miner on every operator. The hotkey wallet maps the hotkey to the coldkey with a global consent that both keys sign; it is kept as a chain under `hotkey-wallet/` in the provider state directory, and each authenticated operator stores the chain and a delegation of the miner's network there to the chain's head. The coldkey never signs per operator.

```text
provider wallet hotkey challenge <coldkey_ss58> --hotkey_seed_file=<path>
    [--wallet-from-epoch=<epoch>] [--wallet-through-epoch=<epoch>] [--operators-url=<url>]
    [-v...]
provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path>
    [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]
    [--wallet-from-epoch=<epoch>] [--wallet-through-epoch=<epoch>] [--operators-url=<url>]
    [-v...]
provider wallet hotkey status [--hotkey_seed_file=<path>] [--operators-url=<url>] [-v...]
```

- `provider wallet hotkey challenge` builds the next global consent statement and prints the exact message for an offline coldkey to sign; the statement is kept pending in `hotkey-wallet/pending.json` under the provider state directory.
- `provider wallet hotkey set` takes the coldkey signature with `--coldkey_seed_file`, or verifies `--message` and `--signature` against the pending statement, then adds the hotkey signature, stores the chain, and submits the chain and the delegation to every authenticated operator. A failing operator never stops the others. Setting the same `--message`/`--signature` again resends the chain.
- `provider wallet hotkey status` prints the chain (each generation and the head) and the pending statement while one is waiting, then the delegation each authenticated operator holds: whether it adopts the head, the earning window, and any caveat (a window that ends before the operator's epoch; an operator-side signed network consent that outranks the delegation).
- `--wallet-from-epoch` and `--wallet-through-epoch` select the earning-epoch window the consent applies to (at most 65536 epochs apart); `--operators-url` selects the operator list (see section 9).

## 8. Claim Daemons (`provider claim-daemon`, `provider claim-daemon-v2`)

Both daemons keep claims submitted as epochs finalize, so no one has to run `provider claim` per epoch by hand. Each takes a strict YAML config file (`--config=<path>`); relative paths inside the config are anchored at the config file.

### v1 daemon
```text
provider claim-daemon --config=<path>
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
```
`provider claim-daemon` runs the v1 claim loop: it polls on the config's cadence, discovers the claimable epochs for the configured credential, verifies the payout root on chain, and submits the claim (already signed queue entries can reconcile without a credential). Progress is journaled under the config's state directory; already signed entries survive a restart.

The v1 config is one strict YAML document: required `schema_version: 1` and `release: "1.0"`, `api_url`, `rpc` (list), `key_file` (the relayer key, which must exist), `state_dir`, and `poll_seconds` (default 30; 5 to 3600); optional `legacy_coldkey`, `jwt_file` (a private regular file when set), `lookback_epochs` (default 2, at most 256) and `progress_pool`. Unknown keys are refused, and relative `key_file`, `state_dir` and `jwt_file` paths are anchored at the config file.

The daemon never mints or refreshes a credential. It reads the retained claim credential: `jwt_file` when the config sets one, otherwise the retained provider token in the provider state directory (`.provider.jwt`, or `jwt` in the explicit `--legacy-coldkey` network mode). While that credential is missing, empty or no longer accepted, the daemon prints one line to stderr (`claim-daemon: <reason>; driving retained signed work only`) and stops taking new work, while still driving entries whose exact bytes were already signed; when there is no retained signed entry it exits 1 with the credential error. A valid credential that appears later resumes new work without a restart.

### v2 daemon
```text
provider claim-daemon-v2 --config=<path> [--v1-config=<path>] [--threshold-rao=<rao>] [--claim-now]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
```
`provider claim-daemon-v2` runs the settlement-v2 claim loop (the v2 vault claims a per-account claimable balance):

- `--v1-config=<path>` also runs the unchanged v1 claim daemon of that configuration in the same process; both use one relayer key and share its nonce admission.
- `--threshold-rao=<rao>` claims an account once its claimable balance reaches this many rao. The default is 1,900,000,000 rao (1.9 alpha), and it is never below the 15,200,000 rao (0.0152 alpha) transfer floor.
- `--claim-now` claims every account with anything claimable, once, at start.
- `--durable-volumes` with `--durable-volumes-sha256` declares the external storage and its reviewed digest; the daemons require the daemon-volume schema.
- The v2 config is one strict YAML document with `schema_version: 2`, `api_url`, `rpc` (list), `chain_id`, `vault`, `coldkeys` (list), `key_file`, `state_dir` and `poll_seconds` (default 30; 5 to 3600); `batcher` is optional. The vault is pinned in the config and never taken from the operator's endpoint, and relative `key_file` and `state_dir` paths are anchored at the config file. The annotated example ships at `provider/testdata/claim-daemon-v2.example.yml`.
- The v2 loop needs no operator credential of its own: for each configured coldkey it reads the finalized accounts from `api_url` and checks each account's Merkle proof against the vault root at a finalized block before treating it as claimable. With `--v1-config` the v1 daemon beside it keeps the v1 credential model above.

## 9. Operator Fan-Out (`provider operators`, `provider provide --all-operators`)

The operator surface runs one provider per operator in the operator list.

```text
provider operators [--operators-url=<url>] [-v...]
provider provide --all-operators [--operators-url=<url>] [--operators-refresh=<duration>]
    [--api_url=<api_url>]
    [-v...]
```

- `provider operators` prints each listed operator with its credential state (`jwt present` or `awaiting auth`), and the list's source (fetched or cached) and digest.
- `provider provide --all-operators` runs one provider per operator, each with its operator directory as home and its provider state under `.urnetwork` inside it, and follows the list as it changes: the list is fetched from `--operators-url` (https, or http only for loopback; omitted, the built-in default list), the last good copy is kept as `operators.yml` in the provider state directory, and `--operators-refresh` sets how often the list is refetched (a Go duration of at least 1m; default 1h). Each child gets its own client identity under its operator directory.
- The layout under the provider state directory is the contract: `operators/<domain>` is the child's home (an inherited `HOME` is dropped), the child's state is the `.urnetwork` inside it, and the network token the supervisor watches is `operators/<domain>/.urnetwork/jwt`. An operator without that token is reported as `awaiting auth`, gets no child, and is rechecked every 5 minutes, so it starts as soon as its token is written.
- `--api_url` is refused with `--all-operators`: every operator's urls come from the operator list.

---

## 10. Operational Security Guidelines

1. **Never pass tokens via CLI flags**: Avoid passing tokens in process flags (`-jwt=...` or `-pass=...`) where they are visible in `ps aux` or `/proc/<pid>/cmdline`.
2. **File Permissions**: Ensure `~/.urnetwork/jwt` and private keys are set to `0600` permissions.
3. **Container Isolation**: Ensure each container mount uses a dedicated Docker volume (e.g. `ur_config_1`, `ur_config_2`) to prevent state corruption.

---

## 11. Third-party licenses

Provider builds from this repository statically link the vendored substrate RPC client, and the repository keeps one inventory for everything third-party: `THIRD_PARTY_LICENSES.md` lists each component, its license, its vendored path and its license files. The note worth repeating here is the LGPL one: inside `third_party/go-substrate-rpc-client`, the `gethrpc` package is LGPL-3.0 (its license files are kept in place there), while the rest of that tree is Apache-2.0, and the complete source of the linked version is in this repository at the vendored path.
