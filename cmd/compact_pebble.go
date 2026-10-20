//go:build pebbledb

package cmd

import (
	cometdb "github.com/cometbft/cometbft-db"
	dbm "github.com/cosmos/cosmos-db"
)

func compactCosmosDBPebble(vdb dbm.DB) error {
	raw := vdb.(*dbm.PebbleDB).DB()
	iter, _ := raw.NewIter(nil)
	var start, end []byte
	if iter.First() {
		start = cp(iter.Key())
	}
	if iter.Last() {
		end = cp(iter.Key())
	}
	iter.Close()
	return raw.Compact(start, end, false)
}

func compactCometBFTDBPebble(vdb cometdb.DB) error {
	return vdb.(*cometdb.PebbleDB).Compact(nil, nil)
}
