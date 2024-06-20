// Copyright 2022 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/ui"
	_ "github.com/cockroachdb/cockroach/pkg/ui/distoss" // web UI init hooks
	crdbhttputil "github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/spf13/cobra"
)

var (
	// Flags.
	addr         string
	insecure     bool
	featureFlags string

	uiConfigPath     = regexp.MustCompile("^/uiconfig$")
	statusServerPath = "/_status/"
	adminServerPath  = "/_admin/"
)

// indexTemplate takes arguments about the current session and returns HTML
// which includes the UI JavaScript bundles, plus a script tag which sets the
// currently logged in user so that the UI JavaScript can decide whether to show
// a login page.
var indexHTML = []byte(`<!DOCTYPE html>
<html>
	<head>
		<title>Cockroach Console</title>
		<meta charset="UTF-8">
		<link href="favicon.ico" rel="shortcut icon">
	</head>
	<body>
		<div id="react-layout"></div>
		<script src="bundle.js" type="text/javascript"></script>
	</body>
</html>
`)

type uiFeatureFlags struct {
	is_observability_service        bool
	can_view_kv_metric_dashboards   bool
	disable_kv_level_advanced_debug bool
}

// UIConsole 'dataFromServer' fetched via /uiconfig.
type config struct {
	// Insecure is true if the server is running in insecure mode.
	Insecure bool

	LoggedInUser     *string
	Tag              string
	Version          string
	NodeID           string
	OIDCAutoLogin    bool
	OIDCLoginEnabled bool
	OIDCButtonText   string
	FeatureFlags     uiFeatureFlags

	OIDCGenerateJWTAuthTokenEnabled bool

	LicenseType               string
	SecondsUntilLicenseExpiry int64
	IsManaged                 bool
}

// UIHandler is the repurposed Handler fn from the pkg/ui/ui.go pkg.
func UIHandler() http.Handler {
	// etags is used to provide a unique per-file checksum for each served file,
	// which enables client-side caching using Cache-Control and ETag headers.
	etags := make(map[string]string)

	fileHandlerChain := crdbhttputil.EtagHandler(
		etags,
		http.FileServer(
			http.FS(ui.Assets),
		),
	)

	// Note: This can be used to retrieve latest js bundle.
	// Alternatives: parse version.txt directly.
	buildInfo := build.GetInfo()
	major, minor := build.BranchReleaseSeries()
	cfg := config{
		Insecure:         insecure,
		LoggedInUser:     nil,
		Tag:              buildInfo.Tag,
		Version:          fmt.Sprintf("v%d.%d", major, minor),
		NodeID:           "1", // TODO discoverability API.
		OIDCAutoLogin:    false,
		OIDCLoginEnabled: false,
		OIDCButtonText:   "blah blah",
		FeatureFlags:     uiFeatureFlags{},

		OIDCGenerateJWTAuthTokenEnabled: false,

		LicenseType:               "OSS",
		SecondsUntilLicenseExpiry: 0,
		IsManaged:                 false,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uiConfigPath.MatchString(r.URL.Path) {
			argBytes, err := json.Marshal(cfg)
			if err != nil {
				fmt.Printf("unable to deserialize ui config args: %v\n", err)
				http.Error(w, err.Error(), 500)
				return
			}
			_, err = w.Write(argBytes)
			if err != nil {
				fmt.Printf("unable to write ui config args: %v\n", err)
				http.Error(w, err.Error(), 500)
				return
			}
			return
		}

		if r.Header.Get("Crdb-Development") != "" {
			http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
			return
		}

		if r.URL.Path != "/" {
			fileHandlerChain.ServeHTTP(w, r)
			return
		}

		http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
	})
}

// NewProxy creates a reverse proxy to the given target URL.
func newProxy(target string) (*httputil.ReverseProxy, error) {
	url, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return httputil.NewSingleHostReverseProxy(url), nil
}

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "dbconsole",
	Short: "An observability service for CockroachDB",
	Long: `The Observability Service ingests monitoring and observability data 
from one or more CockroachDB clusters.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Hello World")
		router := http.NewServeMux()
		proxy, err := newProxy("http://localhost:8080")

		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create reverse proxy: %v\n", err)
		}

		router.Handle(statusServerPath, proxy)
		router.Handle(adminServerPath, proxy)

		// Handle all other routes.
		assetHandler := UIHandler()
		router.Handle("/", assetHandler)

		fmt.Printf("Starting server on %s\n", addr)
		if err := http.ListenAndServe(addr, router); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start server: %v\n", err)
			return err
		}

		return nil
	},
}

func main() {
	// Add all the flags registered with the standard "flag" package. Useful for
	// --vmodule, for example.
	RootCmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)

	RootCmd.PersistentFlags().StringVar(
		&addr,
		"http-addr",
		"localhost:8081",
		"The address on which to listen for HTTP requests.")

	// TODO
	RootCmd.PersistentFlags().StringVar(
		&featureFlags,
		"feature-flags",
		"",
		"TODO")

	// TODO
	RootCmd.PersistentFlags().BoolVar(
		&insecure,
		"insecure",
		true,
		"TODO")

	if err := RootCmd.Execute(); err != nil {
		// Get error code
		fmt.Print(context.Background(), "Error: %s", err.Error())
	}
}
