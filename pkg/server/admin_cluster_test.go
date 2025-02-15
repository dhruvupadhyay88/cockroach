// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness/livenesspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminAPIDatabaseDetails(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const numServers = 3
	tc := testcluster.StartTestCluster(t, numServers, base.TestClusterArgs{})
	defer tc.Stopper().Stop(context.Background())

	db := tc.ServerConn(0)

	_, err := db.Exec("CREATE DATABASE test")
	require.NoError(t, err)

	_, err = db.Exec("CREATE TABLE test.foo (id INT PRIMARY KEY, val STRING)")
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		_, err := db.Exec("INSERT INTO test.foo VALUES($1, $2)", i, "test")
		require.NoError(t, err)
	}

	// Flush all stores here so that we can read the ApproximateDiskBytes field without waiting for a flush.
	for i := 0; i < numServers; i++ {
		s := tc.Server(i)
		err = s.GetStores().(*kvserver.Stores).VisitStores(func(store *kvserver.Store) error {
			return store.TODOEngine().Flush()
		})
		require.NoError(t, err)
	}

	s := tc.Server(0)

	var resp serverpb.DatabaseDetailsResponse
	require.NoError(t, serverutils.GetJSONProto(s, "/_admin/v1/databases/test", &resp))
	assert.Nil(t, resp.Stats, "No Stats unless we ask for them explicitly.")

	nodeIDs := tc.NodeIDs()
	testutils.SucceedsSoon(t, func() error {
		var resp serverpb.DatabaseDetailsResponse
		require.NoError(t, serverutils.GetJSONProto(s, "/_admin/v1/databases/test?include_stats=true", &resp))

		if resp.Stats.RangeCount != int64(1) {
			return errors.Newf("expected range-count=1, got %d", resp.Stats.RangeCount)
		}
		if len(resp.Stats.NodeIDs) != len(nodeIDs) {
			return errors.Newf("expected node-ids=%s, got %s", nodeIDs, resp.Stats.NodeIDs)
		}
		assert.Equal(t, nodeIDs, resp.Stats.NodeIDs, "NodeIDs")

		// We've flushed data so this estimation should be non-zero.
		assert.Positive(t, resp.Stats.ApproximateDiskBytes, "ApproximateDiskBytes")

		return nil
	})
}

func TestAdminAPITableStats(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const nodeCount = 3
	tc := testcluster.StartTestCluster(t, nodeCount, base.TestClusterArgs{
		ReplicationMode: base.ReplicationAuto,
		ServerArgs: base.TestServerArgs{
			ScanInterval:    time.Millisecond,
			ScanMinIdleTime: time.Millisecond,
			ScanMaxIdleTime: time.Millisecond,
		},
	})
	defer tc.Stopper().Stop(context.Background())
	server0 := tc.Server(0)

	// Create clients (SQL, HTTP) connected to server 0.
	db := tc.ServerConn(0)

	client, err := server0.GetAdminHTTPClient()
	if err != nil {
		t.Fatal(err)
	}

	client.Timeout = time.Hour // basically no timeout

	// Make a single table and insert some data. The database and test have
	// names which require escaping, in order to verify that database and
	// table names are being handled correctly.
	if _, err := db.Exec(`CREATE DATABASE "test test"`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE "test test"."foo foo" (
			id INT PRIMARY KEY,
			val STRING
		)`,
	); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := db.Exec(`
			INSERT INTO "test test"."foo foo" VALUES(
				$1, $2
			)`, i, "test",
		); err != nil {
			t.Fatal(err)
		}
	}

	url := server0.AdminURL().String() + "/_admin/v1/databases/test test/tables/foo foo/stats"
	var tsResponse serverpb.TableStatsResponse

	// The new SQL table may not yet have split into its own range. Wait for
	// this to occur, and for full replication.
	testutils.SucceedsSoon(t, func() error {
		if err := httputil.GetJSON(client, url, &tsResponse); err != nil {
			t.Fatal(err)
		}
		if len(tsResponse.MissingNodes) != 0 {
			return errors.Errorf("missing nodes: %+v", tsResponse.MissingNodes)
		}
		if tsResponse.RangeCount != 1 {
			return errors.Errorf("Table range not yet separated.")
		}
		if tsResponse.NodeCount != nodeCount {
			return errors.Errorf("Table range not yet replicated to %d nodes.", 3)
		}
		if a, e := tsResponse.ReplicaCount, int64(nodeCount); a != e {
			return errors.Errorf("expected %d replicas, found %d", e, a)
		}
		if a, e := tsResponse.Stats.KeyCount, int64(30); a < e {
			return errors.Errorf("expected at least %d total keys, found %d", e, a)
		}
		return nil
	})

	if len(tsResponse.MissingNodes) > 0 {
		t.Fatalf("expected no missing nodes, found %v", tsResponse.MissingNodes)
	}

	// Kill a node, ensure it shows up in MissingNodes and that ReplicaCount is
	// lower.
	tc.StopServer(1)

	if err := httputil.GetJSON(client, url, &tsResponse); err != nil {
		t.Fatal(err)
	}
	if a, e := tsResponse.NodeCount, int64(nodeCount); a != e {
		t.Errorf("expected %d nodes, found %d", e, a)
	}
	if a, e := tsResponse.RangeCount, int64(1); a != e {
		t.Errorf("expected %d ranges, found %d", e, a)
	}
	if a, e := tsResponse.ReplicaCount, int64((nodeCount/2)+1); a != e {
		t.Errorf("expected %d replicas, found %d", e, a)
	}
	if a, e := tsResponse.Stats.KeyCount, int64(10); a < e {
		t.Errorf("expected at least 10 total keys, found %d", a)
	}
	if len(tsResponse.MissingNodes) != 1 {
		t.Errorf("expected one missing node, found %v", tsResponse.MissingNodes)
	}
	if len(tsResponse.NodeIDs) == 0 {
		t.Error("expected at least one node in NodeIds list")
	}

	// Call TableStats with a very low timeout. This tests that fan-out queries
	// do not leak goroutines if the calling context is abandoned.
	// Interestingly, the call can actually sometimes succeed, despite the small
	// timeout; however, in aggregate (or in stress tests) this will suffice for
	// detecting leaks.
	client.Timeout = 1 * time.Nanosecond
	_ = httputil.GetJSON(client, url, &tsResponse)
}

func TestLivenessAPI(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{})
	defer tc.Stopper().Stop(context.Background())

	startTime := tc.Server(0).Clock().PhysicalNow()

	// We need to retry because the gossiping of liveness status is an
	// asynchronous process.
	testutils.SucceedsSoon(t, func() error {
		var resp serverpb.LivenessResponse
		if err := serverutils.GetJSONProto(tc.Server(0), "/_admin/v1/liveness", &resp); err != nil {
			return err
		}
		if a, e := len(resp.Livenesses), tc.NumServers(); a != e {
			return errors.Errorf("found %d liveness records, wanted %d", a, e)
		}
		livenessMap := make(map[roachpb.NodeID]livenesspb.Liveness)
		for _, l := range resp.Livenesses {
			livenessMap[l.NodeID] = l
		}
		for i := 0; i < tc.NumServers(); i++ {
			s := tc.Server(i)
			sl, ok := livenessMap[s.NodeID()]
			if !ok {
				return errors.Errorf("found no liveness record for node %d", s.NodeID())
			}
			if sl.Expiration.WallTime < startTime {
				return errors.Errorf(
					"expected node %d liveness to expire in future (after %d), expiration was %d",
					s.NodeID(),
					startTime,
					sl.Expiration,
				)
			}
			status, ok := resp.Statuses[s.NodeID()]
			if !ok {
				return errors.Errorf("found no liveness status for node %d", s.NodeID())
			}
			if a, e := status, livenesspb.NodeLivenessStatus_LIVE; a != e {
				return errors.Errorf(
					"liveness status for node %s was %s, wanted %s", s.NodeID(), a, e,
				)
			}
		}
		return nil
	})
}
