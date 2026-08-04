//go:build !pebbledb

package cmd

import (
	cometdb "github.com/cometbft/cometbft-db"
	dbm "github.com/cosmos/cosmos-db"
)

// compactCosmosDBPebble and compactCometBFTDBPebble are no-ops when built without the pebbledb
// tag. To enable pebble support, build with: go build -tags pebbledb
func compactCosmosDBPebble(_ dbm.DB) error    { return nil }
func compactCometBFTDBPebble(_ cometdb.DB) error { return nil }
