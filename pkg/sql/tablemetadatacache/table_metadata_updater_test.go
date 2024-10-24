// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tablemetadatacache

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/datadriven"
)

// TestDataDrivenTableMetadataCacheUpdater tests the operations performed by
// tableMetadataCacheUpdater. It reads data written to system.table_metadata
// by the cache updater to ensure the udpates are valid.
func TestDataDrivenTableMetadataCacheUpdater(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	queryConn := s.ApplicationLayer().SQLConn(t)
	s.ApplicationLayer().DB()

	datadriven.Walk(t, datapathutils.TestDataPath(t, ""), func(t *testing.T, path string) {
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "query":
				rows, err := queryConn.Query(d.Input)
				if err != nil {
					return err.Error()
				}
				res, err := sqlutils.RowsToDataDrivenOutput(rows)
				if err != nil {
					return err.Error()
				}
				return res
			case "update-cache":
				updater := newTableMetadataUpdater(s.InternalExecutor().(isql.Executor))
				updated, err := updater.updateCache(ctx)
				if err != nil {
					return err.Error()
				}
				return fmt.Sprintf("updated %d table(s)", updated)
			case "prune-cache":
				updater := newTableMetadataUpdater(s.InternalExecutor().(isql.Executor))
				pruned, err := updater.pruneCache(ctx)
				if err != nil {
					return err.Error()
				}
				return fmt.Sprintf("pruned %d table(s)", pruned)
			case "flush-stores":
				// Flush store so that bytes return non-zero from span stats.
				s := s.StorageLayer()
				err := s.GetStores().(*kvserver.Stores).VisitStores(func(store *kvserver.Store) error {
					return store.TODOEngine().Flush()
				})
				if err != nil {
					return err.Error()
				}
				return "success"
			default:
				return "unknown command"
			}
		})
	})

}
