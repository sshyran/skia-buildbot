package goldingestion

import (
	"testing"
	"time"

	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/go/ingestion"
	"go.skia.org/infra/go/sharedconfig"
	"go.skia.org/infra/go/testutils"
	tracedb "go.skia.org/infra/go/trace/db"
	"go.skia.org/infra/go/vcsinfo"
	"go.skia.org/infra/golden/go/types"
)

const (
	// name of the input file containing test data.
	TRYBOT_INGESTION_FILE = "testdata/trybot-dm.json"

	// temporary file used to store traceDB content.
	TRYBOT_TRACE_DB_FILE = "./trybot_test_trace.db"

	// URL of the code review system.
	TEST_CODE_REVIEW_URL = "https://codereview.chromium.org"
)

var (
	// trace ids and values that are contained in the test file.
	TRYBOT_TEST_ENTRIES = []struct {
		key   string
		value string
	}{
		{key: "x86_64:MSVC:pipe-8888:Debug:CPU:AVX2:ShuttleB:aaclip:Win8:gm", value: "fa3c371d201d6f88f7a47b41862e2e85"},
		{key: "x86_64:MSVC:pipe-8888:Debug:CPU:AVX2:ShuttleB:clipcubic:Win8:gm", value: "64e446d96bebba035887dd7dda6db6c4"},
	}

	// TEST_COMMITS are the commits we are considering. It needs to contain at
	// least all the commits referenced in the test file.
	TRYBOT_TEST_COMMITS = []*vcsinfo.LongCommit{
		&vcsinfo.LongCommit{
			ShortCommit: &vcsinfo.ShortCommit{
				Hash:    "02cb37309c01506e2552e931efa9c04a569ed266",
				Subject: "Really big code change",
			},
			Timestamp: now.Add(-time.Second * 10).Add(-time.Nanosecond * time.Duration(now.Nanosecond())),
			Branches:  map[string]bool{"master": true},
		},
	}
)

// Tests the processor in conjunction with the vcs.
func TestTrybotGoldProcessor(t *testing.T) {
	// Set up mock VCS and run a servcer with the given data directory.
	vcs := ingestion.MockVCS(TEST_COMMITS)
	server, serverAddr := ingestion.StartTraceDBTestServer(t, TRYBOT_TRACE_DB_FILE)
	defer server.Stop()
	defer testutils.Remove(t, TRYBOT_TRACE_DB_FILE)

	ingesterConf := &sharedconfig.IngesterConfig{
		ExtraParams: map[string]string{
			CONFIG_TRACESERVICE:    serverAddr,
			CONFIG_CODE_REVIEW_URL: TEST_CODE_REVIEW_URL,
		},
	}

	// Set up the processor.
	processor, err := newGoldTrybotProcessor(vcs, ingesterConf, nil)
	assert.Nil(t, err)

	// Load the example file and process it.
	fsResult, err := ingestion.FileSystemResult(TRYBOT_INGESTION_FILE)
	assert.Nil(t, err)
	err = processor.Process(fsResult)
	assert.Nil(t, err)

	// Steal the traceDB used by the processor to verify the results.
	traceDB := processor.(*goldTrybotProcessor).traceDB

	// The timestamp for the issue/patchset in the testfile is 1443718869.
	startTime := time.Unix(1443718868, 0)
	commitIDs, err := traceDB.List(startTime, time.Now())
	assert.Nil(t, err)

	assert.Equal(t, 1, len(FilterCommitIDs(commitIDs, TRYBOT_SRC)))
	assert.Equal(t, 0, len(FilterCommitIDs(commitIDs, VCS_SRC)))

	assert.Equal(t, 1, len(commitIDs))
	assert.Equal(t, &tracedb.CommitID{
		Timestamp: time.Unix(1443718869, 0),
		ID:        "1",
		Source:    string(TRYBOT_SRC) + TEST_CODE_REVIEW_URL + "/1381483003",
	}, commitIDs[0])

	// Get a tile and make sure we have the right number of traces.
	tile, err := traceDB.TileFromCommits(commitIDs)
	assert.Nil(t, err)

	traces := tile.Traces
	assert.Equal(t, len(TEST_ENTRIES), len(traces))

	for _, testEntry := range TEST_ENTRIES {
		found, ok := traces[testEntry.key]
		assert.True(t, ok)
		goldTrace, ok := found.(*types.GoldenTrace)
		assert.True(t, ok)
		assert.Equal(t, 1, len(goldTrace.Values))
		assert.Equal(t, testEntry.value, goldTrace.Values[0])
	}

	// Make sure the prefix is stripped correctly.
	StripSourcePrefix(commitIDs)
	assert.Equal(t, TEST_CODE_REVIEW_URL+"/1381483003", commitIDs[0].Source)

	assert.Nil(t, traceDB.Close())
}