# Pluto Blockchain Baseline Audit (Phase 0)

Audit date: 2026-09-16  
Base branch/commit: `pqc` / `5346a81abfdc2b1110796649d4eb649a9981ef21`  
Work branch: `blockchain-baseline-opt`

## Audit scope and repository condition

This is a source audit only. No blockchain behavior was changed. At the start of
the audit, the worktree already contained a large, uncommitted package-layout
refactor:

- `internal/app` -> `internal/node/app`
- `internal/config` -> `internal/node/config`
- `internal/store` -> `internal/platform/storage`
- `internal/ethrpc` -> `modules/ethereumrpc`
- `internal/evm` -> `modules/evm`
- baseline transaction validation -> `modules/transaction`
- PQC code -> `modules/pqc`

The refactor originated as local, uncommitted work on `pqc` after base commit
`5346a81` (the relocated files have modification times from 2026-09-11). It was
already present when this audit branch was created on 2026-09-16. Git log and
reflog contain no originating commit, so it cannot be attributed more narrowly
than that local worktree. Phase 0 review correction compared every old/new file
pair and committed the structural change separately as `68e1771`.

The complete relocation diff is available with:

```bash
git show --find-renames=50% 68e1771
```

Production differences in that commit are package declarations/imports, the
split of baseline transaction contracts from PQC transaction code, and exporting
`NewValidationError` so the split package can preserve the same error categories.
Documentation, ignore rules, and one indirect `go.mod` entry are also included.
No transaction, PQC, EVM, persistence, RPC, or consensus behavior was changed.
The relocation is therefore isolated from all future correctness work.

Source code, not README claims, was treated as authoritative. The inspected scope
included all Go files under `cmd/plutod`, `internal/node`,
`internal/platform/storage`, `modules/ethereumrpc`, `modules/evm`, and
`modules/transaction`; the PQC production boundary and its related tests were read
only to establish dependency and client/server boundaries. Relevant CometBFT
v1.0.0, go-ethereum v1.16.8, and Pebble v1.1.5 dependency source was also checked
where Pluto relies on their lifecycle or persistence semantics.

## 1. Current architecture

### Runtime write path

```text
Ethereum client / MetaMask
  -> HTTP JSON-RPC
     modules/ethereumrpc.Server
  -> modules/ethereumrpc.EthAPI.SendRawTransaction
  -> local CometBFT RPC client (ConsensusClient.BroadcastTxCommit)
  -> CometBFT mempool / proposal / consensus / block store
  -> internal/node/app.App.CheckTx
  -> internal/node/app.App.ProcessProposal
  -> internal/node/app.App.FinalizeBlock
  -> modules/transaction.TransactionValidator.Validate
     (ECDSA baseline, or a PQC wrapper selected at node composition)
  -> go-ethereum vm.EVM.Call or vm.EVM.Create
  -> modules/evm.PebbleStateDB
  -> internal/platform/storage.PebbleDB
  -> <home>/data/pluto.db
```

The client does not directly access the application DB or EVM. The JSON-RPC
adapter reaches the in-process CometBFT node through `rpc/client/local`, but uses
the same CometBFT RPC interface and ABCI path as a remote client.

### Runtime query path

```text
Ethereum client
  -> modules/ethereumrpc.EthAPI
  -> ConsensusClient.ABCIQuery
  -> internal/node/app.App.Query
  -> a read-through modules/evm.PebbleStateDB
  -> pluto.db
```

Block and result queries go to CometBFT (`Block`, `BlockByHash`, and
`BlockResults`), not to the application DB. Receipt and transaction-by-hash
lookups first require an in-memory Ethereum-hash map owned by `EthAPI`.

### Component responsibilities

| Component | Key type / function | Responsibility | Persistent? | Consensus-critical? | Redundancy / action |
| --- | --- | --- | ---: | ---: | --- |
| `cmd/plutod/main.go` | `run`, `runStart`, `startNodeWithRuntimeConfig`, `transactionValidatorFromGenesis` | CLI dispatch, process lifecycle, CometBFT/app/RPC composition, validator-policy selection | No | Composition is critical | Too many server, admin, and client commands in one binary |
| `cmd/plutod/init_command.go` | `runInit` | Generate node/validator keys and genesis | Files | Yes | Node administration, not runtime server logic |
| `modules/ethereumrpc` | `ConsensusClient`, `EthAPI`, `Server` | Minimal Ethereum protocol adapter | RAM index only | No, except submitted bytes | Correctly uses CometBFT for history; RAM index duplicates full wire tx |
| CometBFT v1.0.0 | `NewNode`, ABCI calls | Consensus, P2P, mempool, block/history storage, index | Yes | Yes, except rebuildable indexes | Correct technology boundary |
| `internal/node/app` | `App` ABCI methods | Genesis, validation boundaries, sequential block execution, metadata | Yes | Yes | State is persisted too early in `FinalizeBlock` |
| `modules/transaction` | `TransactionValidator`, `ECDSAValidator` | Decode/authenticate and normalize a transaction | No | Yes | Good insertion boundary for PQC |
| `modules/evm` | `PebbleStateDB` | go-ethereum `vm.StateDB`, cache, journal, persistence and AppHash | Yes | Yes | Main correctness and bloat hotspot |
| `internal/platform/storage` | `PebbleDB`, `PebbleBatch` | Pebble adapter used by both app and CometBFT | Yes | Depends on caller | One large cache is created per opened DB |
| `modules/pqc` | `HybridValidator`, registry, proxy | Optional validation wrapper and client companion | Genesis + RAM | Policy is consensus-critical | Logic is outside this task and must remain unchanged |

### Documentation discrepancies

1. `docs/architecture.md` describes the new module layout, but that layout is
   uncommitted relative to base commit `5346a81`; the base tree still uses
   `internal/{app,config,ethrpc,evm,store,tx,...}`.
2. The architecture document says business logic is not in `cmd`, while
   `cmd/plutod` currently contains node lifecycle, genesis administration, PQC
   key generation, transaction wrapping, and the client-side signer proxy.
3. The intended EVM layer is shown as transaction execution, but
   `modules/evm/vm_runner.go` is empty. `App.FinalizeBlock` performs EVM setup
   and direct execution itself.
4. README correctly warns that the implementation is not full Ethereum RPC and
   that receipt lookup is RAM-only. It does not describe the incomplete AppHash,
   non-atomic ABCI persistence, or direct-EVM execution problems found here.

## 2. Persistent-state layout

### A. CometBFT blockchain/node data

`cmd/plutod.PebbleDBProvider` gives every CometBFT database a separate Pebble
directory under `<home>/data`:

| Data | Physical location / source | Classification | Growth / lifecycle |
| --- | --- | --- | --- |
| Blocks, parts, commits, block metadata | `blockstore.db`; CometBFT `node/initDBs` ID `blockstore` | CONSENSUS-CRITICAL history | Per block/tx; Comet pruning policy applies |
| Consensus state, validator sets, consensus parameters, ABCI responses | `state.db`; DB ID `state` | CONSENSUS-CRITICAL | Per height plus versioned state; Comet-managed |
| Evidence pool data | `evidence.db`; DB ID `evidence` | CONSENSUS-CRITICAL while live | Evidence lifecycle/expiry; Comet-managed |
| Transaction and block query indexes | `tx_index.db`; default indexer `kv` | REBUILDABLE | Per indexed block/tx; separate from AppHash and prunable by Comet |
| Consensus WAL | `<home>/data/cs.wal/wal` | RUNTIME-ONLY crash recovery | Append/rotate under Comet lifecycle |
| Private-validator signing state | `<home>/data/priv_validator_state.json` | CONSENSUS-CRITICAL local safety | Overwritten as H/R/S advances; prevents double signing |
| Validator and node private keys | `<home>/config/priv_validator_key.json`, `node_key.json` | CONSENSUS-CRITICAL identity | Fixed-size, created once |
| Genesis | `<home>/config/genesis.json` | CONSENSUS-CRITICAL configuration | Fixed after initialization |
| P2P address book | `<home>/config/addrbook.json` when populated | RUNTIME-ONLY / REBUILDABLE | Peer-discovery dependent |

The application does **not** copy blocks or full transactions into `pluto.db`.
The default Comet `kv` index does duplicate selected block/transaction query
metadata in its own rebuildable database, which is an appropriate separation.

### B. Application consensus state

All application state is in `<home>/data/pluto.db`:

- RLP account records: nonce, balance, placeholder storage root, code hash;
- raw contract code by address;
- raw non-zero (and currently cleared-but-still-present) storage slots;
- last application height;
- last application hash.

These items are intended to be consensus-critical. The current AppHash does not
actually commit to all of them; see section 8.

PQC registry entries are not stored in `pluto.db`. They remain in genesis JSON
and are materialized into an immutable in-memory registry at startup.

### C. RPC/index/query data and runtime caches

| Item | Owner | Classification | Notes |
| --- | --- | --- | --- |
| Ethereum tx hash -> parsed tx, full wire tx, height, result | `EthAPI.committed` in `modules/ethereumrpc/service.go` | RUNTIME-ONLY | Unbounded RAM map; lost on restart; full wire tx duplicates Comet block data in RAM only |
| Account/code/storage caches and dirty maps | `PebbleStateDB` | RUNTIME-ONLY | New state object per block/query |
| Journal, snapshots, refund, logs, access list, transient storage | `PebbleStateDB` | RUNTIME-ONLY | Per-block object; finalization boundaries are currently wrong |
| Mempool and its cache | CometBFT | RUNTIME-ONLY | WAL disabled by default |
| Comet `tx_index.db` | CometBFT | REBUILDABLE | Separate from consensus application state and AppHash |

No benchmark metrics or receipts are currently persisted in application state.

## 3. Storage key schema

The following is the complete application key inventory found in source:

| Key / prefix | Source / writer | Key bytes | Value | Value bytes | Growth factor | Delete lifecycle | Included in AppHash? |
| --- | --- | ---: | --- | ---: | --- | --- | --- |
| `"acc-" || address` | `PebbleStateDB.getAccount`, `Commit` | 24 | RLP `Account{Nonce,Balance,StorageRoot,CodeHash}` | Variable; empty current-format account is 37 bytes | Per persisted account/address | No account delete exists | Only when address is dirty in current block |
| `"code-" || address` | `codeKey`, `GetCode`, `SetCode`, `Commit` | 25 | Raw bytecode | `len(code)` | Per contract/code replacement | No persistent delete exists | Raw code no; `CodeHash` may be in a dirty account |
| `"storage-" || address || slot` | `storageKey`, storage getters/setters, `Commit` | 60 | 32-byte word, or zero-length value after clear | 32 or 0 | Per unique touched slot; cleared keys remain | No delete; zero uses `Set(key,nil)` | No |
| `"height"` | `App.Commit` | 6 | Base-10 ASCII height | Number of decimal digits | One overwritten key, not per block | Never; overwritten | No (reported alongside AppHash during handshake) |
| `"appHash"` | `App.Commit` | 7 | SHA-256 result | 32 | One overwritten key, not per block | Never; overwritten | This is the stored commitment itself |

There is no database/schema version key. Any account RLP or key-format change
would otherwise be read as if it were compatible.

### What one native transfer grows

For an already-funded sender and a new receiver, CometBFT persists the block and
its indexes. The application rewrites both loaded account records and creates the
receiver account record; it creates no app-side transaction/receipt record. A
transfer between two existing accounts should add no new logical app keys, but it
still rewrites cached accounts and creates Pebble write/compaction amplification.

For contract execution, each new contract can add one account key, one code key,
and one 60-byte-key/32-byte-value record for each non-zero storage slot. Current
clear/delete behavior means those logical keys need not shrink when state dies.

## 4. Possible state bloat

### 4.1 Account write amplification — confirmed

`modules/evm/state.go: PebbleStateDB.Commit` iterates `for addr, acc := range
s.state`, not `dirtyAccounts`. `getAccount` places both existing and non-existing
addresses in `s.state` on read. Therefore every account loaded during a block is
RLP-encoded and written, even if unchanged. A read of a nonexistent address during
execution can also become a persisted empty account because existence is not
tracked separately.

The direct logical fix is dirty-only account writes, but it must follow rollback
tests. Current dirty flags are not journaled/reverted; a failed transaction can
leave an address dirty even when its value has been restored. That is harmless
for a correct dirty-only value rewrite but matters for create/delete semantics.

### 4.2 Zero-valued storage — confirmed persistent key bloat

`PebbleStateDB.Commit` calls `batch.Set(storageKey, nil)` for a zero word. Pebble
v1.1.5 `Batch.Set` emits an `InternalKeyKindSet` with a zero-length value; it is
not a deletion tombstone. Consequently `Has(key)` remains true and each cleared
slot retains its 60-byte logical key plus LSM metadata. Reads happen to interpret
both missing and zero-length values as zero, hiding the leak.

Required Phase 1 regression: non-zero set -> commit -> zero set -> commit ->
close/reopen -> `Has(storageKey) == false` and `GetState == zero`.

### 4.3 Contract deletion / self-destruct — confirmed incomplete

The lifecycle is broken in two independent ways:

1. `App.FinalizeBlock` never invokes `StateDB.Finalise`, even though the
   go-ethereum `vm.StateDB` interface explicitly requires it at each transaction
   boundary. Thus `SelfDestruct` only marks an in-memory map and zeros balance.
2. Even if `Finalise` were called, it only deletes entries from the `code` and
   `storage` caches. `Commit` has no account/code deletion branch and no prefix
   iteration/deletion for persisted storage. It also leaves `Account.CodeHash`
   intact.

Result: old account, code, and storage keys can remain in Pebble. `SetCode(addr,
nil, ...)` similarly marks code dirty, but `Commit` merely skips an empty code;
it does not delete a previously persisted `code-` key.

Protocol semantics must be selected explicitly before the fix. The current chain
config activates through London but not Shanghai/Cancun, so the VM uses legacy
`SELFDESTRUCT`, not EIP-6780 behavior. Whatever semantics Pluto chooses must delete
or retain account/code/storage consistently and test restart persistence.

### 4.4 Empty-account and rollback bloat

`CreateAccount` and `CreateContract` are not journaled. Failed contract creation
can therefore leave a zero/partial account in the cache; all-cache `Commit` can
persist it. `Finalise(deleteEmptyObjects)` ignores `deleteEmptyObjects`, so EIP-161
empty-account cleanup is absent.

### 4.5 StorageRoot overhead

`Account.StorageRoot` is decoded/encoded and therefore persisted and hashed, but:

- no function computes or updates it;
- `GetStorageRoot` always returns `common.Hash{}` instead of the account field;
- execution does not consult the field;
- current code initializes/resets it to zero.

It is therefore a placeholder for states produced by this implementation. An RLP
zero hash occupies 33 bytes (`0xa0` plus 32 bytes). For the current empty/EOA
shape, the account encoding is 37 bytes versus 4 bytes without the field: 33
extra logical value bytes per account. For an account whose code hash makes the
RLP list exceed 55 payload bytes, removing it can also save one list-length byte,
for 34 bytes total. Approximate logical payload overhead is at least 33,000 bytes
for 1,000 accounts and 330,000 bytes for 10,000 accounts, before Pebble/WAL/
compression effects.

Removing the field is not a source-only cleanup: RLP list arity changes, existing
records require migration, and AppHash values change. It therefore requires a
schema version, migration/compatibility policy, and measured DB sizes at N=1,
1,000, and 10,000. It was not removed in Phase 0.

## 5. Duplicate data

1. CometBFT `blockstore.db` is the canonical transaction/block history. Pluto
   does not duplicate it in `pluto.db`.
2. Default CometBFT `tx_index.db` stores a rebuildable query index separately,
   which is architecturally appropriate but must be included separately in disk
   benchmarks.
3. `EthAPI.committed` stores parsed Ethereum tx, full wire bytes, height, and
   execution result in RAM. It is unbounded and duplicates data available from
   block store/results. It is lost on restart, so receipt and tx-hash RPC methods
   fail for old transactions.
4. `height` and `appHash` are necessary application-handshake metadata, not block
   history copies, but their write must be atomic with the state they describe.

If a persistent Ethereum-hash lookup is later required, it should be a separate,
optional, rebuildable/prunable index containing only Ethereum hash -> Comet height
and tx index. It must not enter `pluto.db` or AppHash, and must not duplicate the
full transaction body.

## 6. Incorrect persistence candidates

### Confirmed P0 issues

1. **Non-atomic ABCI state commit.** `App.FinalizeBlock` writes account/code/
   storage state synchronously before the ABCI `Commit` call. `App.Commit` later
   writes `height` and `appHash` in two additional independent synchronous writes.
   A crash between these points can leave state from height H with metadata for
   H-1; CometBFT replay then executes H against already-mutated state. Even a crash
   between the two metadata writes can pair a new height with an old hash.
2. **DB read errors are discarded.** `getAccount`, `GetCode`, `getStorage`, and
   `GetCommittedState` use `data, _ := db.Get(...)`. An I/O/corruption error can be
   interpreted as a missing/zero value, causing deterministic state divergence.
   Account RLP corruption instead panics the node.
3. **Persistent deletion is absent.** Zero storage, empty code, self-destructed
   accounts/code/storage, and empty accounts do not receive Pebble deletes.
4. **No schema version.** The database cannot reject or migrate incompatible RLP
   or key layouts.

### EVM/transaction correctness issues that affect persisted state

1. `App.FinalizeBlock` increments sender nonce before both call and creation.
   `vm.EVM.Create` itself derives the contract address from the current nonce and
   increments that nonce. Contract creation therefore derives from `txNonce+1`
   and ends with the sender nonce incremented twice.
2. The app calls `vm.EVM.Call/Create` directly instead of `core.ApplyMessage` (or
   an equivalent complete transition). It bypasses intrinsic gas, the block gas
   pool, transaction prechecks, access-list/transient-state preparation, refund
   calculation, and standard nonce handling. Native transfer `GasUsed` can be
   zero despite a 21,000 gas limit.
3. `StateDB.Finalise` is never called per transaction. Besides deletion, journals
   and snapshots are not given the expected transaction boundary.
4. `GetCommittedState` reads Pebble directly. State is committed only after the
   whole block, so transaction N+1 sees block-start storage rather than storage
   committed by transaction N for EIP-2200 original-value semantics.
5. **`SetCode` violates the go-ethereum StateDB contract.** The interface says
   `SetCode(address, newCode, reason) []byte` returns the previous bytecode, if
   any. `modules/evm/state.go: PebbleStateDB.SetCode` correctly captures
   `prevCode` in the journal, then updates `acc.CodeHash`, and returns the newly
   assigned hash bytes instead of `prevCode`. Current go-ethereum contract-create
   call sites ignore the return, so no present execution difference was observed;
   the wrong type/content can nevertheless break tracing, future callers, or a
   dependency upgrade. Phase 1A needs a regression that seeds old code, calls
   `SetCode`, asserts that the exact old bytecode is returned (including the
   no-prior-code case), and separately verifies new code/hash plus snapshot revert.
6. The EVM block context advertises 30,000,000 gas while genesis defaults to
   CometBFT block `MaxGas=10,000,000`. `CheckTx` returns no `GasWanted`, direct EVM
   execution does not use a shared block gas pool, and the app does not enforce
   cumulative gas. The consensus limit is therefore not a meaningful EVM block
   gas limit.

## 7. Write amplification

1. Every cached account is rewritten once per block by `PebbleStateDB.Commit`.
2. `PebbleDB.Set`, `SetSync`, `Delete`, and `DeleteSync` all force `pebble.Sync`.
   State itself is batched once, but `height` and `appHash` are two additional
   synchronous writes per block.
3. Zero slots are rewritten as empty values rather than deleted.
4. Every call to `NewPebbleDB` creates an independent 512 MiB cache. A default
   node opens application, blockstore, state, evidence, and default tx-index DBs,
   so five independent cache capacities are configured (nominally 2.5 GiB), plus
   64 MiB memtable configuration per DB. Allocation is demand-driven, but these
   are not shared budgets and can produce excessive RAM use.

No benchmark in the repository justifies these values or quantifies commit/write
amplification.

## 8. AppHash analysis

`PebbleStateDB.ComputeAppHash`:

1. takes only addresses in `dirtyAccounts` for the current StateDB/block;
2. sorts them deterministically by hex address;
3. hashes `address || RLP(Account)` with SHA-256;
4. omits raw code, every storage key/value, deletion markers, metadata, and all
   unchanged accounts;
5. does not chain the previous AppHash;
6. silently skips an account if RLP encoding fails.

Field coverage is therefore:

| State item | Covered? | Detail |
| --- | ---: | --- |
| Balance | Partial | Only for accounts dirty in this block |
| Nonce | Partial | Only for accounts dirty in this block |
| Code | Partial/indirect | Account `CodeHash` may be included; raw code key and deletion are not |
| Contract storage | No | `StorageRoot` is always zero and storage records are never hashed |
| Deleted account/code/storage | No | No canonical deletion representation; persistent deletion is missing |
| Full state from prior blocks | No | Untouched accounts are omitted and previous hash is not chained |

**Correctness conclusion:** two nodes can have the same AppHash while holding
different live storage values, orphan code, deleted keys, or untouched account
state. A transaction that changes only contract storage can leave the serialized
account identical, producing the same AppHash for different storage. Likewise,
divergence in account A is invisible in a later block whose dirty set contains
only account B.

Deterministic sorting makes the implemented partial hash repeatable, and the
existing two-node test proves repeatability of the same execution. It does not
prove completeness as a state commitment.

A minimal Phase 1 design does not need an Ethereum Merkle Patricia Trie. A
canonical full-state hash can iterate only live consensus namespaces in raw-byte
key order and hash unambiguous length-delimited key/value records. Metadata and
query indexes must be excluded. This is O(total state) per block but simple,
deterministic, testable, and acceptable as an initial research baseline; an
incremental authenticated structure should only follow measurement.

## 9. Hard-coded parameters

### Exact CometBFT v1.0.0 and Pluto values

`cmd/plutod/runInit` leaves `GenesisDoc.ConsensusParams` nil. Its call to
CometBFT `GenesisDoc.ValidateAndComplete` therefore installs
`DefaultConsensusParams`. These are the actual values written to Pluto genesis,
not values inferred from a runtime config:

| Parameter | Exact value | Parameter class | Source and effect |
| --- | ---: | --- | --- |
| CometBFT consensus `Block.MaxBytes` | 4,194,304 bytes (4 MiB) | CONSENSUS PARAMETER | Written into genesis; bounds the complete Comet block, so available tx bytes are smaller after evidence/header/commit overhead |
| CometBFT consensus `Block.MaxGas` | 10,000,000 | CONSENSUS PARAMETER | Written into genesis and supplied to mempool reaping; Pluto currently returns `GasWanted=0`, so it does not effectively bound selected EVM work |
| EVM `BlockContext.GasLimit` | 30,000,000 | APPLICATION EVM PARAMETER | Hard-coded in `App.FinalizeBlock`; currently no shared block gas pool enforces it |
| Mempool `MaxTxBytes` | 1,048,576 bytes (1 MiB) | MEMPOOL PARAMETER | Comet `DefaultMempoolConfig`; maximum accepted individual mempool tx |
| Mempool `Size` | 5,000 transactions | MEMPOOL PARAMETER | Comet `DefaultMempoolConfig`; count bound |
| Mempool `MaxTxsBytes` | 67,108,864 bytes (64 MiB) | MEMPOOL PARAMETER | Comet `DefaultMempoolConfig`; aggregate mempool byte bound |

The 4 MiB/10,000,000 values are therefore correct for genesis consensus state,
but they must not be conflated with the 30,000,000 EVM context or with mempool
capacity. The mismatch and zero `GasWanted` are the correctness findings, not the
numeric values themselves.

| Parameter | Current source/value | Classification | Finding |
| --- | --- | --- | --- |
| EVM chain ID | `internal/node/config.DefaultEVMChainID = 700001` | PROTOCOL / CONSENSUS CRITICAL | Stored/validated in genesis but node composition still hard-codes default |
| Comet chain ID | `DefaultCometChainID = pluto-local-1`; `plutod init --chain-id` | PROTOCOL / CONSENSUS CRITICAL | Correctly in genesis and CLI-configurable at init |
| EVM fork schedule | `App.FinalizeBlock`, Homestead through London at block 0; later timestamp forks unset | PROTOCOL / CONSENSUS CRITICAL | Must be explicit/versioned; comment “all EIPs” is inaccurate |
| EVM block gas limit | `App.FinalizeBlock: 30,000,000` | PROTOCOL / RESEARCH PARAMETER | Duplicated as RPC constant and not actually enforced cumulatively |
| RPC-reported gas limit | `modules/ethereumrpc.BlockGasLimit = 30,000,000` | CLIENT VIEW / RESEARCH PARAMETER | Manual duplication can drift from execution |
| Comet block max bytes | library genesis default `4,194,304` | PROTOCOL / CONSENSUS CRITICAL | Implicit library default, not Pluto config |
| Comet block max gas | library genesis default `10,000,000` | PROTOCOL / CONSENSUS CRITICAL | Conflicts with 30M EVM context and ineffective current gas accounting |
| Mempool max tx | Comet default `1 MiB` | RUNTIME TUNING | ECDSA validator has no smaller app-level cap; hybrid has its existing bound |
| Mempool count/bytes/cache | defaults `5,000`, `64 MiB`, `10,000` | RUNTIME TUNING / RESEARCH | Implicit and not benchmark-backed |
| Baseline/hybrid wire limit | hybrid module: Ethereum payload `128 KiB`; envelope derived from fixed fields | PROTOCOL / RESEARCH PARAMETER | PQC format is out of scope and must not change here |
| Ethereum RPC address | `127.0.0.1:8545`, `--eth-rpc` | RUNTIME / CLIENT CONFIGURATION | Exposed minimally; empty disables server |
| Comet RPC/P2P addresses | library defaults `127.0.0.1:26657`, `0.0.0.0:26656` | RUNTIME TUNING | No generated/loaded Pluto config file; only tests inject changes |
| Pebble block cache | `512 MiB` per database | RUNTIME TUNING | No benchmark evidence; potentially five independent caches |
| Pebble memtable | `64 MiB` per database | RUNTIME TUNING | No benchmark evidence |
| Consensus timeouts | Comet defaults: propose 3s, prevote 1s, precommit 1s, commit 1s (with documented deltas) | RUNTIME / RESEARCH PARAMETER | Not exposed or persisted by Pluto |
| Empty blocks | Comet default enabled, interval 0 | RUNTIME / RESEARCH PARAMETER | Causes continuous block/history growth even without transactions |

`runInit` creates genesis and keys but does not write a `config.toml`.
`runStart` reconstructs `cfg.DefaultConfig()` every time, so there is no actual
“configure once, start service” path beyond genesis and two CLI flags. Runtime
defaults also depend on the linked CometBFT version.

## 10. Client/server coupling

### Current `cmd/plutod` classification

| Command/function | Classification | Recommended ownership |
| --- | --- | --- |
| `start`, `startNodeWithRuntimeConfig`, DB provider, validator composition | SERVER | Keep in `plutod` (composition may move into internal node package) |
| `init`, allocation/PQ binding parsing, validator/node-key generation | NODE ADMINISTRATION | May remain a compatible `plutod init` wrapper; reusable admin logic should not live in server startup |
| `pqc-keygen` | PQC CLIENT TOOLING | Move implementation to `plutoctl`/client package; keep wrapper |
| `pqc-wrap` | PQC CLIENT TOOLING | Move implementation to `plutoctl`/client package; keep wrapper |
| `pqc-proxy` | PQC CLIENT TOOLING / CLIENT SERVICE | Move implementation/entrypoint to client side; keep wrapper and crypto behavior |
| Ethereum RPC server | SERVER | Correctly part of the single node process |

The node already supports a long-running process, signal-driven graceful
shutdown, and `plutod start`. No GUI, microservice, Docker, or systemd dependency
is needed. The missing pieces are a small persisted runtime config and a clean
client abstraction (`GetBalance`, `GetNonce`, `SendRawTransaction`,
`GetTransaction`) that only speaks RPC.

Minimal target: retain one server process containing CometBFT + ABCI + EVM +
Pebble + Ethereum RPC; introduce `cmd/plutoctl` for client operations and preserve
old `plutod pqc-*` wrappers. PQC cryptographic behavior must not move across the
transaction-validation boundary or be redesigned.

### Ethereum JSON-RPC audit

| Category | Methods / behavior |
| --- | --- |
| Required for native-transfer baseline | `eth_chainId`, `eth_blockNumber`, `eth_getBalance`, `eth_getTransactionCount`, `eth_sendRawTransaction`, `eth_getTransactionReceipt` |
| Useful thin mappings to Comet history | `eth_getCode`, `eth_getTransactionByHash`, block queries, block tx count, `eth_syncing` |
| Explicit prototype placeholders | `eth_call` always empty; `eth_estimateGas` heuristic; gas price/tip zero; synthetic `eth_feeHistory`; empty accounts; receipts omit real logs/contract address; `net_peerCount=0`, `net_listening=true` |
| Ethereum-incompatible representations | `stateRoot` is incomplete AppHash; `transactionsRoot` is Comet tx hash, receipt root is zero; block `size` counts only raw tx bytes |

Placeholder methods should either remain clearly documented for the exact
MetaMask workflow or return unsupported errors. They must not motivate copying
block history into application consensus state.

## 11. Corrected implementation gates

### Phase 1A — Blockchain Execution Correctness

1. Make application state plus height/AppHash crash-atomic at ABCI `Commit` and
   add a crash/replay-oriented restart test.
2. Replace direct partial EVM execution with a complete, deterministic state
   transition (or implement every required equivalent step), fixing contract
   creation nonce/address, intrinsic/cumulative gas, `Prepare`, per-tx `Finalise`,
   and multi-transaction storage semantics.
3. Correct intrinsic gas, access-list/transient preparation, refunds, failed-tx
   rollback, and block-level gas accounting.
4. Correct `GetCommittedState` semantics for multiple transactions in one block.
5. Make `SetCode` return the previous bytecode and lock its interface behavior
   with a regression test.
6. Propagate database read/decoding errors rather than interpreting them as zero.
7. Make existing state, height, and existing AppHash persistence crash-atomic.

No state-size optimization belongs in Phase 1A unless it is strictly required to
make execution or crash recovery correct.

### Phase 1B — Persistent State Correctness and State Bloat

1. Commit only dirty accounts after correctness tests establish dirty/journal
   behavior for success, revert, multiple transactions, and restart.
2. Use Pebble delete for zero slots and empty/dead code; remove all storage under
   a deleted account via a deterministic prefix lifecycle.
3. Implement persistent code/storage/account deletion under the currently active
   pre-Cancun SELFDESTRUCT rules, plus empty/dead account cleanup.
4. Replace partial AppHash with a deterministic commitment over all live account,
   code, and storage state; add storage-only/code-only/deletion divergence tests.
5. Add schema versioning before changing account RLP or key schema.
6. Benchmark `StorageRoot` removal at 1/1,000/10,000 accounts before deciding on
   reinitialization and removal.
7. Measure logical and physical state size before/after each optimization.

### Phase 2 — Client / Server Separation

1. Separate client tooling into `plutoctl`/client packages with compatibility
   wrappers in `plutod`.
2. Introduce a client interface that communicates only through RPC.
3. Preserve compatible `plutod pqc-*` wrappers without changing PQC behavior.

### Phase 3 — Configuration and ECDSA Baseline Benchmark

1. Persist/load a minimal node runtime config; keep consensus-critical values in
   genesis and expose only meaningful runtime/research parameters.
2. Benchmark Pebble cache/memtable combinations and choose defaults from evidence.
3. Add reproducible ECDSA workloads and external JSON/CSV/log metrics.
4. Report protocol baseline separately from optional Comet transaction-index
   overhead.

### Phase 4 — Final Validation

Run unit, integration, restart, race, multi-validator, vet, and build checks;
reproduce storage and ECDSA benchmark reports; verify that PQC semantics and tests
remain unchanged.

## 12. Nice-to-have issues

1. Optional, separate, rebuildable/prunable Ethereum-hash -> height/index lookup.
2. Bound or remove `EthAPI.committed` full-wire RAM retention.
3. Benchmark shared/per-DB Pebble cache and memtable matrices before selecting
   defaults; include app, Comet, total disk, RSS, read/write/commit latency.
4. Add non-PQC ECDSA workload/report tooling without writing metrics to state.
5. Make placeholder JSON-RPC behavior explicit and add adapter unit tests.
6. Consider shorter binary state keys only after DB-size/write benchmarks and
   only with schema migration support.

## 13. Things that should deliberately NOT be implemented

- Ethereum Merkle Patricia Trie solely for compatibility; a simple complete
  deterministic commitment is enough for this research baseline.
- Full Ethereum RPC, archive-node behavior, full fee market, staking, governance,
  tokenomics, explorer, or complex wallet.
- A second copy of blocks/transactions/receipts in application consensus state.
- PostgreSQL, Redis, LevelDB, another blockchain framework, microservices,
  Kubernetes, or a different storage engine.
- PQC format/size/algorithm/key-registry/sign-byte/anti-downgrade changes.
- State key shortening without measured benefit and a versioned migration.

## Review decisions governing later phases

1. **SELFDESTRUCT:** preserve the semantics implied by Pluto's current fork
   schedule (Homestead through London active, Cancun unset). Do not activate
   Cancun/EIP-6780 as part of cleanup.
2. **Database compatibility:** an incompatible baseline schema may require node
   database reinitialization. Add an explicit schema version and fail fast on an
   incompatible database instead of building a complex legacy migration system.
3. **Transaction index:** keep it outside application consensus state. Benchmark
   protocol-level results without optional transaction-index overhead and report
   indexed mode separately when useful.
4. **Ethereum transaction lookup:** do not copy blockchain history into
   application state. Any persistent lookup must be optional, rebuildable,
   non-consensus, and store only minimal location metadata.

## Baseline test status and coverage gaps

Executed on the audited worktree:

| Command | Result | Elapsed |
| --- | --- | ---: |
| `go test ./... -count=1` | PASS | 26.58 s |
| `go test -race ./... -count=1` | PASS | 60.53 s |
| `go vet ./...` | PASS | 5.54 s |
| `go build ./...` | PASS | 9.23 s |

The normal suite runs the existing real-node E2E and two-validator tests. The E2E
helpers deliberately set Comet's tx indexer to `null`, so default KV-index behavior
and disk use are not covered. There are no direct tests for
`internal/platform/storage` or `modules/ethereumrpc`.

Missing tests directly related to this audit:

- dirty-only versus loaded-clean account persistence/write counts;
- zero-slot physical deletion after restart;
- empty code and self-destruct account/code/storage cleanup;
- failed deployment rollback and correct create address/nonce;
- storage original-value semantics across multiple transactions in one block;
- AppHash changes for storage-only/code-only/deletion changes and identical full
  state producing identical hash across insertion orders/restarts;
- crash boundary between `FinalizeBlock` and `Commit`;
- schema compatibility/version rejection;
- receipt/transaction lookup after RPC process restart.

## Corrected phase order

1. **Phase 1A — Blockchain Execution Correctness:** EVM transaction lifecycle,
   gas, transaction boundaries, nonce/address, original-storage semantics,
   `SetCode`, DB read errors, block gas, and atomic commit.
2. **Phase 1B — Persistent State Correctness and State Bloat:** dirty writes,
   physical deletion, empty/dead state, StorageRoot, complete AppHash, schema
   version, and storage measurements.
3. **Phase 2 — Client / Server Separation.**
4. **Phase 3 — Configuration and ECDSA Baseline Benchmark.**
5. **Phase 4 — Final Validation.**

No Phase 1A or later implementation is included in this audit correction.
