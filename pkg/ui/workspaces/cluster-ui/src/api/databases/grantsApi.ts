// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import useSWRImmutable from "swr/immutable";

import { fetchDataJSON } from "../fetchData";
import {
  APIV2ResponseWithPaginationState,
  SimplePaginationState,
} from "../types";

export type DatabaseGrant = {
  grantee: string;
  privilege: string;
};

export enum GrantsSortOptions {
  GRANTEE = "grantee",
  PRIVILEGE = "privilege",
}

type DatabaseGrantsRequest = {
  dbId: number;
  sortBy?: GrantsSortOptions;
  sortOrder?: "asc" | "desc";
  pagination?: SimplePaginationState;
};

export type DatabaseGrantsResponse = APIV2ResponseWithPaginationState<
  DatabaseGrant[]
>;

const createDbGrantsPath = (req: DatabaseGrantsRequest): string => {
  const { dbId, pagination, sortBy, sortOrder } = req;
  const urlParams = new URLSearchParams();
  if (pagination?.pageNum) {
    urlParams.append("pageNum", pagination.pageNum.toString());
  }
  if (pagination?.pageSize) {
    urlParams.append("pageSize", pagination.pageSize.toString());
  }
  if (sortBy) {
    urlParams.append("sortBy", sortBy);
  }
  if (sortOrder) {
    urlParams.append("sortOrder", sortOrder);
  }
  return `api/v2/grants/databases/${dbId}/?` + urlParams.toString();
};

const fetchDbGrants = (
  req: DatabaseGrantsRequest,
): Promise<DatabaseGrantsResponse> => {
  const path = createDbGrantsPath(req);
  return fetchDataJSON(path);
};

export const useDatabaseGrantsImmutable = (req: DatabaseGrantsRequest) => {
  const { data, isLoading, error } = useSWRImmutable(
    createDbGrantsPath(req),
    () => fetchDbGrants(req),
  );

  return {
    databaseGrants: data?.results,
    pagination: data?.pagination_info,
    isLoading,
    error: error,
  };
};
