// +build integration

// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package integration

import (
	"testing"
	"time"

	"github.com/m3db/m3/src/dbnode/integration/generate"
	"github.com/m3db/m3/src/dbnode/namespace"
	persistfs "github.com/m3db/m3/src/dbnode/persist/fs"
	"github.com/m3db/m3/src/dbnode/retention"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/bootstrapper"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/bootstrapper/fs"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/index"
	"github.com/m3db/m3/src/dbnode/storage/index/compaction"
	"github.com/m3db/m3/src/m3ninx/idx"
	"github.com/m3db/m3/src/m3ninx/index/segment/fst"
	"github.com/m3db/m3/src/x/ident"

	"github.com/stretchr/testify/require"
)

func TestFilesystemBootstrapIndexWithIndexingEnabled(t *testing.T) {
	if testing.Short() {
		t.SkipNow() // Just skip if we're doing a short run
	}

	var (
		blockSize = 2 * time.Hour
		rOpts     = retention.NewOptions().SetRetentionPeriod(2 * blockSize).SetBlockSize(blockSize)
		idxOpts   = namespace.NewIndexOptions().SetEnabled(true).SetBlockSize(2 * blockSize)
		nOpts     = namespace.NewOptions().SetRetentionOptions(rOpts).SetIndexOptions(idxOpts)
	)
	ns1, err := namespace.NewMetadata(testNamespaces[0], nOpts)
	require.NoError(t, err)
	ns2, err := namespace.NewMetadata(testNamespaces[1], nOpts)
	require.NoError(t, err)

	opts := newTestOptions(t).
		SetNamespaces([]namespace.Metadata{ns1, ns2})

	// Test setup
	setup, err := newTestSetup(t, opts, nil)
	require.NoError(t, err)
	defer setup.close()

	fsOpts := setup.storageOpts.CommitLogOptions().FilesystemOptions()

	persistMgr, err := persistfs.NewPersistManager(fsOpts)
	require.NoError(t, err)

	storageIdxOpts := setup.storageOpts.IndexOptions()
	compactor, err := compaction.NewCompactor(storageIdxOpts.DocumentArrayPool(),
		index.DocumentArrayPoolCapacity,
		storageIdxOpts.SegmentBuilderOptions(),
		storageIdxOpts.FSTSegmentOptions(),
		compaction.CompactorOptions{
			FSTWriterOptions: &fst.WriterOptions{
				// DisableRegistry is set to true to trade a larger FST size
				// for a faster FST compaction since we want to reduce the end
				// to end latency for time to first index a metric.
				DisableRegistry: true,
			},
		})
	require.NoError(t, err)

	noOpAll := bootstrapper.NewNoOpAllBootstrapperProvider()
	bsOpts := result.NewOptions().
		SetSeriesCachePolicy(setup.storageOpts.SeriesCachePolicy())
	bfsOpts := fs.NewOptions().
		SetResultOptions(bsOpts).
		SetFilesystemOptions(fsOpts).
		SetIndexOptions(storageIdxOpts).
		SetDatabaseBlockRetrieverManager(setup.storageOpts.DatabaseBlockRetrieverManager()).
		SetPersistManager(persistMgr).
		SetCompactor(compactor)
	bs, err := fs.NewFileSystemBootstrapperProvider(bfsOpts, noOpAll)
	require.NoError(t, err)
	processOpts := bootstrap.NewProcessOptions().
		SetTopologyMapProvider(setup).
		SetOrigin(setup.origin)
	processProvider, err := bootstrap.NewProcessProvider(bs, processOpts, bsOpts)
	require.NoError(t, err)

	setup.storageOpts = setup.storageOpts.
		SetBootstrapProcessProvider(processProvider)

	// Write test data
	now := setup.getNowFn()

	fooSeries := generate.Series{
		ID:   ident.StringID("foo"),
		Tags: ident.NewTags(ident.StringTag("city", "new_york"), ident.StringTag("foo", "foo")),
	}

	barSeries := generate.Series{
		ID:   ident.StringID("bar"),
		Tags: ident.NewTags(ident.StringTag("city", "new_jersey")),
	}

	bazSeries := generate.Series{
		ID:   ident.StringID("baz"),
		Tags: ident.NewTags(ident.StringTag("city", "seattle")),
	}

	seriesMaps := generate.BlocksByStart([]generate.BlockConfig{
		{
			IDs:       []string{fooSeries.ID.String()},
			Tags:      fooSeries.Tags,
			NumPoints: 100,
			Start:     now.Add(-blockSize),
		},
		{
			IDs:       []string{barSeries.ID.String()},
			Tags:      barSeries.Tags,
			NumPoints: 100,
			Start:     now.Add(-blockSize),
		},
		{
			IDs:       []string{fooSeries.ID.String()},
			Tags:      fooSeries.Tags,
			NumPoints: 50,
			Start:     now,
		},
		{
			IDs:       []string{bazSeries.ID.String()},
			Tags:      bazSeries.Tags,
			NumPoints: 50,
			Start:     now,
		},
	})

	require.NoError(t, writeTestDataToDisk(ns1, setup, seriesMaps, 0))
	require.NoError(t, writeTestDataToDisk(ns2, setup, nil, 0))

	// Start the server with filesystem bootstrapper
	log := setup.storageOpts.InstrumentOptions().Logger()
	log.Debug("filesystem bootstrap test")
	require.NoError(t, setup.startServer())
	log.Debug("server is now up")

	// Stop the server
	defer func() {
		require.NoError(t, setup.stopServer())
		log.Debug("server is now down")
	}()

	// Verify data matches what we expect
	verifySeriesMaps(t, setup, testNamespaces[0], seriesMaps)
	verifySeriesMaps(t, setup, testNamespaces[1], nil)

	// Issue some index queries
	session, err := setup.m3dbClient.DefaultSession()
	require.NoError(t, err)

	start := now.Add(-rOpts.RetentionPeriod())
	end := now.Add(blockSize)
	queryOpts := index.QueryOptions{StartInclusive: start, EndExclusive: end}

	// Match all new_*r*
	regexpQuery, err := idx.NewRegexpQuery([]byte("city"), []byte("new_.*r.*"))
	require.NoError(t, err)
	iter, exhaustive, err := session.FetchTaggedIDs(ns1.ID(),
		index.Query{Query: regexpQuery}, queryOpts)
	require.NoError(t, err)
	defer iter.Finalize()

	verifyQueryMetadataResults(t, iter, exhaustive, verifyQueryMetadataResultsOptions{
		namespace:  ns1.ID(),
		exhaustive: true,
		expected:   []generate.Series{fooSeries, barSeries},
	})

	// Match all *e*e*
	regexpQuery, err = idx.NewRegexpQuery([]byte("city"), []byte(".*e.*e.*"))
	require.NoError(t, err)
	iter, exhaustive, err = session.FetchTaggedIDs(ns1.ID(),
		index.Query{Query: regexpQuery}, queryOpts)
	require.NoError(t, err)
	defer iter.Finalize()

	verifyQueryMetadataResults(t, iter, exhaustive, verifyQueryMetadataResultsOptions{
		namespace:  ns1.ID(),
		exhaustive: true,
		expected:   []generate.Series{barSeries, bazSeries},
	})
}
