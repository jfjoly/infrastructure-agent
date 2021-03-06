// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package agent

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/newrelic/infrastructure-agent/internal/agent/delta"
	"github.com/newrelic/infrastructure-agent/internal/testhelpers"
	"github.com/newrelic/infrastructure-agent/pkg/backend/http"
	"github.com/newrelic/infrastructure-agent/pkg/backend/identityapi"
	"github.com/newrelic/infrastructure-agent/pkg/backend/inventoryapi"
	"github.com/newrelic/infrastructure-agent/pkg/config"
	"github.com/newrelic/infrastructure-agent/pkg/entity"
	"github.com/stretchr/testify/assert"
)

var (
	agentIdn         = entity.Identity{ID: 13}
	registerEntities = []identityapi.RegisterEntity{
		identityapi.NewRegisterEntity("my-entity-1"),
	}
)

func TempDeltaStoreDir() (string, error) {
	return ioutil.TempDir("", "deltastore")
}

func FailingPostDelta(_ []string, _ bool, _ ...*inventoryapi.RawDelta) (*inventoryapi.PostDeltaResponse, error) {
	return nil, fmt.Errorf("catapun!")
}

func FakePostDelta(_ []string, _ bool, _ ...*inventoryapi.RawDelta) (*inventoryapi.PostDeltaResponse, error) {
	return &inventoryapi.PostDeltaResponse{}, nil
}

func ResetPostDelta(_ []string, _ bool, _ ...*inventoryapi.RawDelta) (*inventoryapi.PostDeltaResponse, error) {
	return &inventoryapi.PostDeltaResponse{
		Reset: inventoryapi.ResetAll,
	}, nil
}

func TestNewPatchSender(t *testing.T) {
	agentContext := &context{cfg: &config.Config{}}
	ps, err := newPatchSender("", agentContext, &delta.Store{}, "", emptyIdnProvide, http.NullHttpClient)
	assert.NotNil(t, ps)
	assert.NoError(t, err)
}

func cachePluginData(t *testing.T, store *delta.Store, entityKey string) {
	plugin := &delta.PluginInfo{
		Source:       "metadata/plugin",
		Plugin:       "metadata",
		FileName:     "plugin.json",
		MostRecentID: int64(0),
		LastSentID:   int64(0),
	}
	srcFile := store.SourceFilePath(plugin, entityKey)
	err := os.MkdirAll(filepath.Dir(srcFile), 0755)
	assert.NoError(t, err)
	diff1 := []byte(`{"hostname":{"alias":"aaa-opsmatic","entityKey":"hostname"}}`)
	err = ioutil.WriteFile(srcFile, diff1, 0644)
	assert.NoError(t, err)
	err = store.UpdatePluginsInventoryCache(entityKey)
	assert.NoError(t, err)
	err = store.SaveState()
	assert.NoError(t, err)
	fileInfo, err := os.Stat(filepath.Join(store.CacheDir, "metadata", entityKey))
	assert.NoError(t, err)
	assert.True(t, fileInfo.IsDir())
}

func TestPatchSender_Process_LongTermOffline(t *testing.T) {
	// Given a delta Store
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "default", maxInventoryDataSize)

	// With some cached plugin data
	cachePluginData(t, store, "entityKey")

	// And a patch sender that has been disconnected for more than 24 hours
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 10, 12, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       FailingPostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}
	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// When the patch sender tries to process the deltas
	err = ps.Process()

	// It returns an error since they are not sent, but just cleaned up
	assert.Error(t, err)

	// And the delta cache has been cleaned up
	_, err = os.Stat(filepath.Join(store.CacheDir, "metadata", "entityKey"))
	assert.True(t, os.IsNotExist(err))
}

func TestPatchSender_Process_LongTermOffline_ReconnectPlugins(t *testing.T) {
	// Given a delta Store
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "default", maxInventoryDataSize)

	// With some cached plugin data
	cachePluginData(t, store, "entityKey")

	// And a patch sender that has been disconnected for more than 24 hours, but doesn't need to reset deltas
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 10, 12, 12, 12, 12, &time.Location{})
	lastDeltaRemoval := time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey: "entityKey",
		store:     store,
		context: &context{
			agentKey:     "agentIdentifier",
			reconnecting: new(sync.Map),
		},
		postDeltas:       FakePostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastDeltaRemoval,
		resetIfOffline:   resetTime,
	}
	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// With a reconnectable plugin
	wg := &sync.WaitGroup{}
	plugin := reconnectingPlugin{context: ps.context, invocations: 0, wg: wg}
	ps.context.AddReconnecting(&plugin)
	wg.Add(1)

	// When the patch sender tries to process the deltas
	assert.NoError(t, ps.Process())

	// The registered plugin has been invoked to run again
	assert.NoError(t, wait(3*time.Second, wg))
	assert.Equal(t, 1, plugin.invocations)
}

func TestPatchSender_Process_LongTermOffline_NoDeltasToPost_UpdateLastConnection(t *testing.T) {
	// Given a delta Store
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "default", maxInventoryDataSize)

	// And a patch sender that has been disconnected for less than 24 hours
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       FailingPostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}

	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// When the patch sender tries to process the deltas
	err = ps.Process()

	// The lastConnection time has been updated
	assert.True(t, lastConnection.Before(ps.lastConnection))
}

func TestPatchSender_Process_LongTermOffline_AlreadyRemoved(t *testing.T) {
	// Given a delta Store
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "default", maxInventoryDataSize)

	// With some cached plugin data
	cachePluginData(t, store, "entityKey")

	// And a patch sender that has been disconnected for more than 24 hours
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 10, 12, 12, 12, 12, &time.Location{})
	// But the deltas were already cleaned up less than 24 hours ago
	lastRemoval := time.Date(2018, 12, 12, 10, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       FailingPostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastRemoval,
		resetIfOffline:   resetTime,
	}
	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// When the patch sender tries to process the deltas
	err = ps.Process()

	// It returns an error since they are not sent
	assert.Error(t, err)

	// But the current delta cache is not cleaned up since it is less than 24 hours old
	fileInfo, err := os.Stat(filepath.Join(store.CacheDir, "metadata", "entityKey"))
	assert.NoError(t, err)
	assert.True(t, fileInfo.IsDir())
}

func TestPatchSender_Process_ShortTermOffline(t *testing.T) {
	// Given a delta Store
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "default", maxInventoryDataSize)

	// With some cached plugin data
	cachePluginData(t, store, "entityKey")

	// And a patch sender that has been disconnected for less than 24 hours
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       FailingPostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}

	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// When the patch sender fails at processing deltas
	err = ps.Process()
	assert.Error(t, err)

	// The delta cache has NOT been cleaned up
	fileInfo, err := os.Stat(filepath.Join(store.CacheDir, "metadata", "entityKey"))
	assert.NoError(t, err)
	assert.True(t, fileInfo.IsDir())
}

func TestPatchSender_Process_DividedDeltas(t *testing.T) {
	const entityKey = "entityKey"

	// Given a patch sender
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "localhost", maxInventoryDataSize)
	pdt := testhelpers.NewPostDeltaTracer(maxInventoryDataSize)
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       pdt.PostDeltas,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}
	timeNow = func() time.Time {
		return time.Date(2018, 12, 12, 12, 12, 12, 12, &time.Location{})
	}

	// And a set of normal-sized deltas from different plugins
	testhelpers.PopulateDeltas(dataDir, entityKey, []testhelpers.FakeDeltaEntry{
		{Source: "plugin1/plugin1", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 10000},
		{Source: "plugin2/plugin2", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 1000},
		{Source: "plugin3/plugin3", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 1000},
	})

	// When the patch sender processes them
	assert.NoError(t, ps.Process())

	// They are divided in chunks and submitted in multiple invocations
	assert.Len(t, pdt.Errors, 0)
	assert.Len(t, pdt.Sources, 2)

	// Two plugins deltas in the first invocation
	assert.Contains(t, pdt.Sources[0], "plugin1/plugin1")
	assert.Contains(t, pdt.Sources[0], "plugin2/plugin2")

	// The later plugin in the second invocation
	assert.Contains(t, pdt.Sources[1], "plugin3/plugin3")
}

func TestPatchSender_Process_DisabledDeltaSplit(t *testing.T) {
	const entityKey = "entityKey"

	// Given a patch sender with disabled delta split
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "localhost", delta.DisableInventorySplit)
	pdt := testhelpers.NewPostDeltaTracer(math.MaxInt32)
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       pdt.PostDeltas,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}

	// And a set of normal-sized deltas from different plugins
	testhelpers.PopulateDeltas(dataDir, entityKey, []testhelpers.FakeDeltaEntry{
		{Source: "plugin1/plugin1", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 10000},
		{Source: "plugin2/plugin2", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 1000},
		{Source: "plugin3/plugin3", DeltasSize: maxInventoryDataSize/3 + 100, BodySize: 1000},
	})

	// When the patch sender processes them
	assert.NoError(t, ps.Process())

	// They are not divided in chunks even if they are larger than the maximum inventory data size
	assert.Len(t, pdt.Errors, 0)
	assert.Len(t, pdt.Sources, 1)

	// And all the deltas are sent in the same invocation
	assert.Contains(t, pdt.Sources[0], "plugin1/plugin1")
	assert.Contains(t, pdt.Sources[0], "plugin2/plugin2")
	assert.Contains(t, pdt.Sources[0], "plugin3/plugin3")
}

func TestPatchSender_Process_SingleRequestDeltas(t *testing.T) {
	const entityKey = "entityKey"

	// Given a patch sender
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "localhost", maxInventoryDataSize)
	pdt := testhelpers.NewPostDeltaTracer(maxInventoryDataSize)
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       pdt.PostDeltas,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
	}

	// And a set of deltas from different plugins, whose total size is smaller than the max inventory data size
	testhelpers.PopulateDeltas(dataDir, entityKey, []testhelpers.FakeDeltaEntry{
		{Source: "plugin1/plugin1", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin2/plugin2", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin3/plugin3", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin4/plugin4", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
	})

	// When the patch sender processes them
	assert.NoError(t, ps.Process())

	// They are sent in a single request
	assert.Len(t, pdt.Errors, 0)
	assert.Len(t, pdt.Sources, 1)

	// All in the first invocation
	assert.Contains(t, pdt.Sources[0], "plugin1/plugin1")
	assert.Contains(t, pdt.Sources[0], "plugin2/plugin2")
	assert.Contains(t, pdt.Sources[0], "plugin3/plugin3")
	assert.Contains(t, pdt.Sources[0], "plugin4/plugin4")
}

func TestPatchSender_Process_CompactEnabled(t *testing.T) {
	const entityKey = "entityKey"

	// Given a patch sender with compaction enabled
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "localhost", maxInventoryDataSize)
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey:        "entityKey",
		store:            store,
		context:          &context{agentKey: "agentIdentifier"},
		postDeltas:       FakePostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
		compactEnabled:   true,
	}

	// And a set of stored deltas that occupy a given size in disk
	testhelpers.PopulateDeltas(dataDir, entityKey, []testhelpers.FakeDeltaEntry{
		{Source: "plugin1/plugin1", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin2/plugin2", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin3/plugin3", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin4/plugin4", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
	})
	storageSize, err := store.StorageSize(dataDir)
	assert.NoError(t, err)

	// When the patch sender processes them
	assert.NoError(t, ps.Process())

	// The data is compacted
	compactedSize, err := store.StorageSize(dataDir)
	assert.NoError(t, err)
	assert.True(t, compactedSize < storageSize, "%v not smaller than %v", compactedSize, storageSize)
}

func TestPatchSender_Process_Reset(t *testing.T) {
	const entityKey = "entityKey"

	// Given a patch sender
	dataDir, err := TempDeltaStoreDir()
	assert.NoError(t, err)
	store := delta.NewStore(dataDir, "localhost", maxInventoryDataSize)
	resetTime, _ := time.ParseDuration("24h")
	lastConnection := time.Date(2018, 12, 12, 0, 12, 12, 12, &time.Location{})
	ps := patchSenderIngest{
		entityKey: "entityKey",
		store:     store,
		context:   &context{agentKey: "agentIdentifier", reconnecting: new(sync.Map)},
		// And a backend service that returns ResetAll after being invoked
		postDeltas:       ResetPostDelta,
		cfg:              &config.Config{},
		lastConnection:   lastConnection,
		lastDeltaRemoval: lastConnection,
		resetIfOffline:   resetTime,
		compactEnabled:   true,
	}

	// And a set of stored deltas that occupy a given size in disk
	testhelpers.PopulateDeltas(dataDir, entityKey, []testhelpers.FakeDeltaEntry{
		{Source: "plugin1/plugin1", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin2/plugin2", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin3/plugin3", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
		{Source: "plugin4/plugin4", DeltasSize: maxInventoryDataSize / 10, BodySize: 100},
	})

	// When the patch sender processes them
	assert.NoError(t, ps.Process())

	// The deltas are removed
	storageSize, err := store.StorageSize(dataDir)
	assert.NoError(t, err)

	// few bytes remain (the almost-empty .delta_repo/delta_id_file.json file) + few directories
	assert.True(t, storageSize < 10, "%v not smaller than 10", storageSize)
}

var emptyIdnProvide = func() entity.Identity {
	return entity.EmptyIdentity
}
