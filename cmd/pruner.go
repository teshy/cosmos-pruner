package cmd

import (
	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"
	"encoding/binary"
	"fmt"
	"github.com/bharvest-devops/cosmos-pruner/internal/rootmulti"
	cometdb "github.com/cometbft/cometbft-db"
	cmtstore "github.com/cometbft/cometbft/proto/tendermint/store"
	"github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	dbm "github.com/cosmos/cosmos-db"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// to figuring out the height to prune tx_index
var txIdxHeight int64 = 0

// load dbm
// load app store and prune
// if immutable tree is not deletable we should import and export current state

func pruneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune [path_to_home]",
		Short: "prune data from the application store and block store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {

			//ctx := cmd.Context()
			//errs, _ := errgroup.WithContext(ctx)
			var err error
			if tendermint {
				if err = pruneTMData(args[0]); err != nil {
					fmt.Println(err.Error())
				}
			}

			if cosmosSdk {
				err = pruneAppState(args[0])
				if err != nil {
					fmt.Println(err.Error())
				}
			}

			if tx_idx {
				err = pruneTxIndex(args[0])
				if err != nil {
					fmt.Println(err.Error())
				}
			}

			return nil
		},
	}
	return cmd
}

func pruneTxIndex(home string) error {
	fmt.Println("pruning tx_index")
	txIdxDB, err := openCosmosDB("tx_index", home)
	if err != nil {
		return err
	}

	defer func() {
		errClose := txIdxDB.Close()
		if errClose != nil {
			fmt.Println(errClose.Error())
		}
	}()

	pruneHeight := txIdxHeight - int64(blocks) - 10
	if pruneHeight <= 0 {
		fmt.Printf("No need to prune (pruneHeight=%d)\n", pruneHeight)
		return nil
	}

	pruneBlockIndex(txIdxDB, pruneHeight)
	pruneTxIndexTxs(txIdxDB, pruneHeight)

	fmt.Println("finished pruning tx_index")

	if compact {
		fmt.Println("compacting tx_index")
		if err := compactCosmosDB(txIdxDB); err != nil {
			fmt.Println(err.Error())
		}
	}

	return nil
}

func pruneTxIndexTxs(db dbm.DB, pruneHeight int64) {
	itr, itrErr := db.Iterator(nil, nil)
	if itrErr != nil {
		panic(itrErr)
	}

	defer itr.Close()

	///////////////////////////////////////////////////
	// delete index by hash and index by height
	for ; itr.Valid(); itr.Next() {
		key := itr.Key()
		value := itr.Value()

		strKey := string(key)

		if strings.HasPrefix(strKey, "tx.height") { // index by height
			strs := strings.Split(strKey, "/")
			intHeight, _ := strconv.ParseInt(strs[2], 10, 64)

			if intHeight < pruneHeight {
				db.Delete(value)
				db.Delete(key)
			}
		} else {
			if len(value) == 32 { // maybe index tx by events
				strs := strings.Split(strKey, "/")
				if len(strs) == 4 { // index tx by events
					intHeight, _ := strconv.ParseInt(strs[2], 10, 64)
					if intHeight < pruneHeight {
						db.Delete(key)
					}
				}
			}
		}
	}
}

func pruneBlockIndex(db dbm.DB, pruneHeight int64) {
	itr, itrErr := db.Iterator(nil, nil)
	if itrErr != nil {
		panic(itrErr)
	}

	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		key := itr.Key()
		value := itr.Value()

		strKey := string(key)

		if strings.HasPrefix(strKey, "block.height") /* index block primary key*/ || strings.HasPrefix(strKey, "block_events") /* BeginBlock & EndBlock */ {
			intHeight := int64FromBytes(value)
			//fmt.Printf("intHeight: %d\n", intHeight)

			if intHeight < pruneHeight {
				db.Delete(key)
			}
		}
	}
}

func pruneAppState(home string) error {
	appDB, errDB := openCosmosDB("application", home)
	if errDB != nil {
		return errDB
	}

	defer appDB.Close()

	var err error

	//TODO: need to get all versions in the store, setting randomly is too slow
	fmt.Println("pruning application state")

	//// only mount keys from core sdk
	//// todo allow for other keys to be mounted
	//keys := types.NewKVStoreKeys(
	//	authtypes.StoreKey, banktypes.StoreKey, stakingtypes.StoreKey,
	//	minttypes.StoreKey, distrtypes.StoreKey, slashingtypes.StoreKey,
	//	govtypes.StoreKey, paramstypes.StoreKey, ibchost.StoreKey, upgradetypes.StoreKey,
	//	evidencetypes.StoreKey, ibctransfertypes.StoreKey, capabilitytypes.StoreKey,
	//)

	keys := getStoreKeys(appDB)

	// TODO: cleanup app state
	appStore := rootmulti.NewStore(appDB, log.NewNopLogger())

	if txIdxHeight <= 0 {
		txIdxHeight = appStore.LastCommitID().Version
		fmt.Printf("[pruneAppState] set txIdxHeight=%d\n", txIdxHeight)
	}

	for _, value := range keys {
		appStore.MountStoreWithDB(storetypes.NewKVStoreKey(value), storetypes.StoreTypeIAVL, nil)
	}

	err = appStore.LoadLatestVersion()
	if err != nil {
		return err
	}

	allVersions := appStore.GetAllVersions()

	v64 := make([]int64, len(allVersions))
	for i := 0; i < len(allVersions); i++ {
		v64[i] = int64(allVersions[i])
	}

	versionsToPrune := int64(len(v64)) - int64(versions)
	fmt.Printf("[pruneAppState] versionsToPrune=%d\n", versionsToPrune)
	if versionsToPrune <= 0 {
		fmt.Printf("[pruneAppState] No need to prune (%d)\n", versionsToPrune)
	} else {
		var (
			pruningHeight int64
			i             int
		)
		for {
			pruningHeight = appStore.GetPruningHeight(versionsToPrune)
			if i > 10000 {
				panic("Could not found pruning height!! you should check if your storage is healthy")
			}
			if pruningHeight == 0 {
				versionsToPrune--
				i++
				continue
			}
			break
		}

		err = appStore.PruneStores(pruningHeight)
		if err != nil {
			fmt.Println(err.Error())
		}
	}

	if compact {
		fmt.Println("compacting application state")
		if err := compactCosmosDB(appDB); err != nil {
			fmt.Println(err.Error())
		}
	}

	return nil
}

// pruneTMData prunes the tendermint blocks and state based on the amount of blocks to keep
func pruneTMData(home string) error {
	blockStoreDB, errDBBlock := openCometBFTDB("blockstore", home)
	if errDBBlock != nil {
		return errDBBlock
	}

	// Get StateStore
	stateDB, errDBBState := openCometBFTDB("state", home)
	if errDBBState != nil {
		blockStoreDB.Close()
		return errDBBState
	}

	var err error

	stateStore := state.NewStore(stateDB, state.StoreOptions{})
	defer stateStore.Close()

	// Load committed state BEFORE creating the blockStore wrapper.
	// The blockstore can sit one block ahead of the committed state at shutdown — the normal
	// CometBFT transient when the engine stages the next block before committing it. We must
	// trim any trailing uncommitted blocks BEFORE creating store.NewBlockStore so the wrapper
	// reads the correct height from disk. If we trim after NewBlockStore, the wrapper's
	// in-memory height cache (loaded at construction) overwrites our trimmed BlockStoreState
	// when PruneBlocks() writes its own update — leaving storeHeight > committed again.
	loadedState, err := stateStore.Load()
	if err != nil {
		blockStoreDB.Close()
		return fmt.Errorf("failed to load state: %w", err)
	}

	// Trim phase: open a temporary blockStore to check and remove any trailing blocks,
	// then close and reopen the raw DB so the pruning blockStore sees the corrected height.
	{
		tempBS := store.NewBlockStore(blockStoreDB)
		currentHeight := tempBS.Height()
		if loadedState.LastBlockHeight > 0 && loadedState.LastBlockHeight < currentHeight {
			fmt.Printf("[pruneTMData] trimming %d trailing uncommitted block(s): %d→%d\n",
				currentHeight-loadedState.LastBlockHeight, currentHeight, loadedState.LastBlockHeight)
			if err := trimTrailingBlocks(blockStoreDB, tempBS, loadedState.LastBlockHeight); err != nil {
				tempBS.Close()
				blockStoreDB.Close()
				return fmt.Errorf("failed to trim trailing blocks: %w", err)
			}
		}
		tempBS.Close()
	}

	// Close and reopen blockStoreDB so the subsequent NewBlockStore reads the (trimmed) height
	// from disk rather than any stale in-memory state.
	blockStoreDB.Close()
	blockStoreDB, errDBBlock = openCometBFTDB("blockstore", home)
	if errDBBlock != nil {
		return errDBBlock
	}

	blockStore := store.NewBlockStore(blockStoreDB)
	defer blockStore.Close()

	base := blockStore.Base()
	effectiveHeight := blockStore.Height() // correct after trim

	pruneHeight := effectiveHeight - int64(blocks)
	fmt.Printf("[pruneTMData] pruneHeight=%d\n", pruneHeight)
	if pruneHeight <= 0 {
		fmt.Println("[pruneTMData] No need to prune")
		return nil
	}

	if txIdxHeight <= 0 {
		txIdxHeight = effectiveHeight
		fmt.Printf("[pruneTMData] set txIdxHeight=%d\n", txIdxHeight)
	}

	fmt.Println("pruning block/state store")

	var (
		prunedBlocksCount uint64
		endHeight         int64 = base
	)

	// prune block store
	// prune one by one instead of range to avoid `panic: pebble: batch too large: >= 4.0 G` issue
	// (see https://github.com/notional-labs/cosmprund/issues/11)
	for pruneStateFrom := base; pruneStateFrom < pruneHeight-1; pruneStateFrom += rootmulti.PRUNE_BATCH_SIZE {
		err = nil
		height := pruneStateFrom
		if height >= pruneHeight-1 {
			height = pruneHeight - 1
		}

		prunedBlocks, evidenceRetainBlocks, _ := blockStore.PruneBlocks(height, loadedState)
		if err != nil {
			//return err
			fmt.Println(err.Error())
		}
		prunedBlocksCount += prunedBlocks

		endHeight += rootmulti.PRUNE_BATCH_SIZE
		if endHeight >= pruneHeight-1 {
			endHeight = pruneHeight - 1
		}

		_, err = stateStore.LoadConsensusParams(endHeight)
		if err != nil {
			continue
		}
		_, err = stateStore.LoadValidators(endHeight)
		if err != nil {
			continue
		}
		_, err = stateStore.LoadFinalizeBlockResponse(endHeight)
		if err != nil {
			continue
		}
		_, err = stateStore.LoadLastFinalizeBlockResponse(endHeight)
		if err != nil {
			continue
		}

		err = stateStore.PruneStates(pruneStateFrom, endHeight, evidenceRetainBlocks)
		if err != nil {
			fmt.Printf("failed to prune state store: %s", err)
		}
	}

	fmt.Printf("Pruned blocks count: %d\n", prunedBlocksCount)

	if compact {
		fmt.Println("compacting block store")
		if err := compactCometBFTDB(blockStoreDB); err != nil {
			fmt.Println(err.Error())
		}
	}

	if compact {
		fmt.Println("compacting state store")
		if err := compactCometBFTDB(stateDB); err != nil {
			fmt.Println(err.Error())
		}
	}

	return nil
}

// Utils
func openCosmosDB(dbname string, home string) (dbm.DB, error) {
	dbType := dbm.BackendType(backend)
	dbDir := rootify(dataDir, home)

	var db1 dbm.DB

	if dbType == dbm.GoLevelDBBackend {
		o := opt.Options{
			DisableSeeksCompaction: true,
		}

		lvlDB, err := dbm.NewGoLevelDBWithOpts(dbname, dbDir, &o)
		if err != nil {
			return nil, err
		}

		db1 = lvlDB
	} else {
		var err error
		db1, err = dbm.NewDB(dbname, dbType, dbDir)
		if err != nil {
			return nil, err
		}
	}

	return db1, nil
}

// Utils
func openCometBFTDB(dbname string, home string) (cometdb.DB, error) {
	dbType := cometdb.BackendType(backend)
	dbDir := rootify(dataDir, home)

	var db1 cometdb.DB

	if dbType == cometdb.GoLevelDBBackend {
		o := opt.Options{
			DisableSeeksCompaction: true,
		}

		lvlDB, err := cometdb.NewGoLevelDBWithOpts(dbname, dbDir, &o)
		if err != nil {
			return nil, err
		}

		db1 = lvlDB
	} else {
		var err error
		db1, err = cometdb.NewDB(dbname, dbType, dbDir)
		if err != nil {
			return nil, err
		}
	}

	return db1, nil
}

func compactCosmosDB(vdb dbm.DB) error {
	dbType := dbm.BackendType(backend)

	if dbType == dbm.GoLevelDBBackend {
		vdbLevel := vdb.(*dbm.GoLevelDB)
		if err := vdbLevel.ForceCompact(nil, nil); err != nil {
			return err
		}
	} else if dbType == dbm.PebbleDBBackend {
		if err := compactCosmosDBPebble(vdb); err != nil {
			return err
		}
	}

	return nil
}

func compactCometBFTDB(vdb cometdb.DB) error {
	dbType := cometdb.BackendType(backend)

	if dbType == cometdb.GoLevelDBBackend {
		vdbLevel := vdb.(*cometdb.GoLevelDB)
		if err := vdbLevel.Compact(nil, nil); err != nil {
			return err
		}
	} else if dbType == cometdb.PebbleDBBackend {
		if err := compactCometBFTDBPebble(vdb); err != nil {
			return err
		}
	}

	return nil
}

func getStoreKeys(db dbm.DB) (storeKeys []string) {
	latestVer := rootmulti.GetLatestVersion(db)
	latestCommitInfo, err := getCommitInfo(db, latestVer)
	if err != nil {
		panic(err)
	}

	for _, storeInfo := range latestCommitInfo.StoreInfos {
		storeKeys = append(storeKeys, storeInfo.Name)
	}
	return
}

func getCommitInfo(db dbm.DB, ver int64) (*storetypes.CommitInfo, error) {
	const commitInfoKeyFmt = "s/%d" // s/<version>
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, ver)

	bz, err := db.Get([]byte(cInfoKey))
	if err != nil {
		return nil, fmt.Errorf("failed to get commit info: %s", err)
	} else if bz == nil {
		return nil, fmt.Errorf("no commit info found")
	}

	cInfo := &storetypes.CommitInfo{}
	if err = cInfo.Unmarshal(bz); err != nil {
		return nil, fmt.Errorf("failed unmarshal commit info: %s", err)
	}

	return cInfo, nil
}

func cp(bz []byte) (ret []byte) {
	ret = make([]byte, len(bz))
	copy(ret, bz)
	return ret
}

func rootify(path, root string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func int64FromBytes(bz []byte) int64 {
	v, _ := binary.Varint(bz)
	return v
}

// trimTrailingBlocks deletes any block data above committedHeight from the blockstore and writes
// a fresh BlockStoreState so blockStore.Height() == committedHeight on next open.
//
// Why this is needed: the blockstore can sit one block ahead of the committed app/state height at
// shutdown — CometBFT stages the next block before the previous one is fully committed. If
// cosmos-pruner runs against data in that transient state, PruneBlocks() only removes old blocks
// from the bottom and leaves the uncommitted trailing block at the top. On restart the node sees
// storeHeight > appHeight, attempts to replay the trailing block, and chains whose BeginBlocker
// validates against pruned state (e.g. validator set records) panic. Chains that do not panic
// are still left with store > state — a latent inconsistency.
//
// This function explicitly removes the trailing block keys (H:, P:*, SC:, BH:) and rewrites the
// BlockStoreState meta so the store reports exactly the committed height.
func trimTrailingBlocks(db cometdb.DB, bs *store.BlockStore, committedHeight int64) error {
	for h := bs.Height(); h > committedHeight; h-- {
		batch := db.NewBatch()

		// Delete block header
		batch.Delete([]byte(fmt.Sprintf("H:%d", h)))
		// Delete seenCommit
		batch.Delete([]byte(fmt.Sprintf("SC:%d", h)))
		// Delete extended commit (vote extensions, ABCI 2.0 / CometBFT 0.38+). Mirrors
		// CometBFT's own PruneBlocks, which deletes EC:<h> for every pruned height; without
		// this, trimming a block that had SaveBlockWithExtendedCommit called on it leaves an
		// orphaned EC:<h> key behind.
		batch.Delete([]byte(fmt.Sprintf("EC:%d", h)))

		// Delete block parts and BH:<hash> using the block meta if available.
		// For a staged-but-uncommitted block the meta may or may not be present.
		if meta := bs.LoadBlockMeta(h); meta != nil {
			for i := 0; i < int(meta.BlockID.PartSetHeader.Total); i++ {
				batch.Delete([]byte(fmt.Sprintf("P:%d:%d", h, i)))
			}
			hash := strings.ToLower(meta.BlockID.Hash.String())
			batch.Delete([]byte(fmt.Sprintf("BH:%s", hash)))
		} else {
			// Meta absent — delete at least the most common single-part key
			batch.Delete([]byte(fmt.Sprintf("P:%d:0", h)))
		}

		// Rewrite BlockStoreState so Height reflects the trimmed top.
		// Do this on every iteration so the state is consistent even if we crash mid-trim.
		newHeight := h - 1
		bss := cmtstore.BlockStoreState{Base: bs.Base(), Height: newHeight}
		bssBytes, err := bss.Marshal()
		if err != nil {
			batch.Close()
			return fmt.Errorf("marshal BlockStoreState: %w", err)
		}
		batch.Set([]byte("blockStore"), bssBytes)

		if err := batch.WriteSync(); err != nil {
			batch.Close()
			return fmt.Errorf("write trim batch at height %d: %w", h, err)
		}
		batch.Close()
		fmt.Printf("[trimTrailingBlocks] removed block %d, blockstore now at %d\n", h, newHeight)
	}
	return nil
}
