// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// TODO: add these functions to hermitcrab pkg

package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"cloud.google.com/go/storage"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

const bucketName = "crdb-ui-test"

func formatFileName(name string) string {
	// Remove the "ui/" prefix and the ".tar.zst" suffix.
	name = name[3 : len(name)-8]
	return name
}

func ListUIAssets(major int, minor int) ([]string, error) {
	versionString := fmt.Sprintf("v%d.%d", major, minor)
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithScopes(storage.ScopeReadOnly))
	if err != nil {
		return nil, fmt.Errorf("Error creating storage client: " + err.Error())
	}

	bucket := client.Bucket(bucketName)

	log.Infof(ctx, "Listing objects in bucket %s with prefix %s", bucketName, versionString)
	// Create a query to list objects with the given prefix
	query := &storage.Query{
		Prefix:     "ui/" + versionString,
		Delimiter:  "/",
		Projection: storage.ProjectionNoACL,
	}

	it := bucket.Objects(ctx, query)

	var res []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Bucket(%q).Objects: %s", bucketName, err.Error())
		}
		res = append(res, formatFileName(attrs.Name))
	}

	return res, nil
}

func DownloadUIAsset(fileName string) error {
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithScopes(storage.ScopeReadOnly))
	if err != nil {
		return fmt.Errorf("Error creating storage client: " + err.Error())
	}

	bucket := client.Bucket(bucketName)

	obj := bucket.Object("ui/" + fileName + ".tar.zst")
	r, err := obj.NewReader(ctx)
	if err != nil {
		return fmt.Errorf("error creating reader for object %s: %s", obj.ObjectName(), err.Error())
	}
	defer r.Close()
	f, err := os.Create("ui-assets/" + fileName + ".tar.zst")
	if err != nil {
		return fmt.Errorf("error creating file %s: %s", fileName, err.Error())
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("error copying object %s to file %s: %s", obj.ObjectName(), fileName, err.Error())
	}

	return nil
}
