// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

/*
Package server implements the Cockroach storage node. A node
corresponds to a single instance of the cockroach binary, running on a
single physical machine, which exports the "Node" Go RPC service. Each
node multiplexes RPC requests to one or more stores, associated with
physical storage devices.

Package server also provides access to administrative tools via the
command line and also through a REST API.
*/
package server
