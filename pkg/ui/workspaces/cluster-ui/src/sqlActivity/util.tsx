// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { Filters, getTimeValueInSeconds } from "../queryFilter";
import { AggregateStatistics } from "../statementsTable";
import {
  CollectedStatementStatistics,
  containAny,
  FixFingerprintHexValue,
  flattenStatementStats,
  unset,
} from "../util";
import { filterBySearchQuery } from "../statementsPage";
import { createSelector } from "@reduxjs/toolkit";

export function filteredStatementsData(
  filters: Filters,
  search: string,
  statements: AggregateStatistics[],
  nodeRegions: { [key: string]: string },
  isTenant: boolean,
): AggregateStatistics[] {
  const timeValue = getTimeValueInSeconds(filters);
  const sqlTypes =
    filters.sqlType?.length > 0
      ? filters.sqlType.split(",").map(function (sqlType: string) {
          // Adding "Type" to match the value on the Statement
          // Possible values: TypeDDL, TypeDML, TypeDCL and TypeTCL
          return "Type" + sqlType;
        })
      : [];
  const databases =
    filters.database?.length > 0 ? filters.database.split(",") : [];
  if (databases.includes(unset)) {
    databases.push("");
  }
  const regions = filters.regions?.length > 0 ? filters.regions.split(",") : [];
  const nodes = filters.nodes?.length > 0 ? filters.nodes.split(",") : [];
  const appNames = filters.app
    ?.split(",")
    .map(app => app.trim())
    .filter(appName => !!appName);

  // Return statements filtered by the values selected on the filter and
  // the search text. A statement must match all selected filters to be
  // displayed on the table.
  // Current filters: search text, database, fullScan, service latency,
  // SQL Type, nodes and regions.
  return statements
    .filter(
      statement =>
        !appNames?.length || appNames.includes(statement.applicationName),
    )
    .filter(
      statement =>
        databases.length == 0 || databases.includes(statement.database),
    )
    .filter(statement => (filters.fullScan ? statement.fullScan : true))
    .filter(
      statement =>
        statement.stats.service_lat.mean >= timeValue || timeValue === "empty",
    )
    .filter(
      statement =>
        sqlTypes.length == 0 || sqlTypes.includes(statement.stats.sql_type),
    )
    .filter(
      // The statement must contain at least one value from the selected regions
      // list if the list is not empty.
      statement =>
        regions.length == 0 ||
        statement.stats.regions?.some(region => regions.includes(region)),
    )
    .filter(
      // The statement must contain at least one value from the selected nodes
      // list if the list is not empty.
      // If the cluster is a tenant cluster we don't care
      // about nodes.
      statement =>
        isTenant ||
        nodes.length == 0 ||
        (statement.stats.nodes &&
          containAny(
            statement.stats.nodes.map(node => "n" + node),
            nodes,
          )),
    )
    .filter(statement =>
      search ? filterBySearchQuery(statement, search) : true,
    );
}

export const aggregateRawStmtsByStmtFingerprintID = (
  rawStmts: CollectedStatementStatistics[],
): AggregateStatistics[] => {
  if (!rawStmts?.length) return [];

  console.log("yeehaw");
  const statements = flattenStatementStats(rawStmts);

  return statements.map(stmt => {
    return {
      aggregatedFingerprintID: stmt.statement_fingerprint_id?.toString(),
      aggregatedFingerprintHexID: FixFingerprintHexValue(
        stmt.statement_fingerprint_id?.toString(16),
      ),
      label: stmt.statement,
      summary: stmt.statement_summary,
      aggregatedTs: stmt.aggregated_ts,
      implicitTxn: stmt.implicit_txn,
      fullScan: stmt.full_scan,
      database: stmt.database,
      applicationName: stmt.app,
      stats: stmt.stats,
    };
  });
};

export const aggregateStmtsMemoized = createSelector(
  (stmts: CollectedStatementStatistics[]) => stmts,
  stmts => aggregateRawStmtsByStmtFingerprintID(stmts),
);
