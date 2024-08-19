// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package ui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"sync"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/assetbundle"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

//go:embed upgradesPage.html
var upgradesTemplateFS embed.FS

const (
	downloadedUIAssets = "ui-assets/"
	// Version of the UI assets that are shipped with the binary.
	defaultUIVersion = "v24.3+ui.0"
)

var DBConsoleVersion = settings.RegisterStringSetting(
	settings.ApplicationLevel,
	"ui.version",
	"currently just the path to the UI assets",
	defaultUIVersion,
)

type DynamicUIServer struct {
	cfg Config
	mu  struct {
		sync.RWMutex
		uiAssets fs.FS
		etags    map[string]string
	}
	updateChan  chan string
	upgradeTmpl *template.Template
	stopper     *stop.Stopper
}

func NewDynamicUIServer(cfg Config, stopper *stop.Stopper) *DynamicUIServer {
	upgradesTemplate, err := template.ParseFS(upgradesTemplateFS, "upgradesPage.html")
	if err != nil {
		log.Error(context.Background(), "Failed to parse upgrades template: "+err.Error())
	}

	s := &DynamicUIServer{
		cfg:         cfg,
		updateChan:  make(chan string, 1), // Buffered channel to avoid blocking
		upgradeTmpl: upgradesTemplate,
		stopper:     stopper,
	}
	s.mu.etags = make(map[string]string)

	return s
}

func (s *DynamicUIServer) onUIVersionChange(ctx context.Context) {
	uiAssetsVersion := DBConsoleVersion.Get(&s.cfg.Settings.SV)
	select {
	case s.updateChan <- uiAssetsVersion:
	default:
		log.Warningf(ctx, "UI asset update already pending, skipping update to %s", uiAssetsVersion)
	}
}

func (s *DynamicUIServer) assetUpdater(ctx context.Context) {
	// TODO: download version from cloud.
	for {
		select {
		case path := <-s.updateChan:
			if err := s.loadUIAssets(path); err != nil {
				log.Errorf(ctx, "Failed to load UI assets from %s: %s", path, err.Error())
			}
		case <-ctx.Done():
			return
		case <-s.stopper.ShouldQuiesce():
			return
		}
	}
}

func (s *DynamicUIServer) loadUIAssets(fileName string) error {
	log.Infof(context.Background(), "Loading UI assets from %s", fileName)
	if fileName == defaultUIVersion {
		s.setUIAssets(Assets)
		return nil
	}

	// Check if the directory exists.
	if _, err := os.Stat(downloadedUIAssets); os.IsNotExist(err) {
		if err := os.Mkdir(downloadedUIAssets, os.ModePerm); err != nil {
			return fmt.Errorf("failed to create directory for UI assets: " + err.Error())
		}
	}
	// Check if the bundle exists in the filesystem.
	expectedFile := downloadedUIAssets + fileName + ".tar.zst"
	if _, err := os.Stat(expectedFile); err != nil {
		// TODO
		err := DownloadUIAsset(fileName)
		if err != nil {
			return fmt.Errorf("failed to download UI assets: " + err.Error())
		}
	}
	assetBytes, err := os.ReadFile(downloadedUIAssets + fileName + ".tar.zst")
	if err != nil {
		return fmt.Errorf("failed to read UI assets from %s: %s", fileName, err.Error())
	}
	assets, err := assetbundle.AsFS(bytes.NewReader(assetBytes))
	if err != nil {
		return fmt.Errorf("failed to create asset filesystem: " + err.Error())
	}

	s.setUIAssets(assets)
	return nil
}

// HandlerV2 returns an http.Handler that serves the UI.
func HandlerV2(cfg Config, stopper *stop.Stopper) http.Handler {
	s := NewDynamicUIServer(cfg, stopper)

	DBConsoleVersion.SetOnChange(&cfg.Settings.SV, s.onUIVersionChange)
	// Start the background asset update goroutine.
	ctx, _ := stopper.WithCancelOnQuiesce(context.Background())
	err := stopper.RunAsyncTask(ctx, "ui-asset-updater", s.assetUpdater)
	if err != nil {
		log.Fatalf(context.Background(), "Failed to start ui asset updater: %v", err)
	}

	// Load initial UI assets.
	path := DBConsoleVersion.Get(&cfg.Settings.SV)
	s.updateChan <- path

	return s
}
func (s *DynamicUIServer) setUIAssets(assets fs.FS) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mu.uiAssets = assets
	s.mu.etags = make(map[string]string)
	if err := httputil.ComputeEtags(assets, s.mu.etags); err != nil {
		log.Error(context.Background(), "Unable to compute asset hashes: "+err.Error())
	}
}

func (s *DynamicUIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	assets := s.mu.uiAssets
	if s.mu.uiAssets == nil {
		log.Error(r.Context(), "Remote UI assets not loaded, falling back to UI shipped with binary.")
		assets = Assets
	}

	fileHandlerChain := httputil.EtagHandler(
		s.mu.etags,
		http.FileServer(http.FS(assets)),
	)

	if uiConfigPath.MatchString(r.URL.Path) {
		s.serveUIConfig(w, r)
		return
	}

	if r.URL.Path == "/upgrades" {
		s.serveUpgradesPage(w, r)
		return
	}

	if r.URL.Path == "/upgrade-ui" && r.Method == "POST" {
		s.handleUpgrade(w, r)
		return
	}

	buildInfo := build.GetInfo()

	if r.Header.Get("Crdb-Development") != "" {
		http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
		return
	}

	if r.URL.Path != "/" {
		fileHandlerChain.ServeHTTP(w, r)
		return
	}

	http.ServeContent(w, r, "index.html", buildInfo.GoTime(), bytes.NewReader(indexHTML))
}

func (s *DynamicUIServer) serveUIConfig(w http.ResponseWriter, r *http.Request) {
	buildInfo := build.GetInfo()
	licenseType, err := base.LicenseType(s.cfg.Settings)
	if err != nil {
		log.Error(r.Context(), "unable to get license type: "+err.Error())
	}

	licenseTTL := base.GetLicenseTTL(r.Context(), s.cfg.Settings, timeutil.DefaultTimeSource{})
	oidcConf := s.cfg.OIDC.GetOIDCConf()
	major, minor := build.BranchReleaseSeries()
	args := indexHTMLArgs{
		Insecure:                        s.cfg.Insecure,
		LoggedInUser:                    s.cfg.GetUser(r.Context()),
		Tag:                             buildInfo.Tag,
		Version:                         fmt.Sprintf("v%d.%d", major, minor),
		OIDCAutoLogin:                   oidcConf.AutoLogin,
		OIDCLoginEnabled:                oidcConf.Enabled,
		OIDCButtonText:                  oidcConf.ButtonText,
		FeatureFlags:                    s.cfg.Flags,
		OIDCGenerateJWTAuthTokenEnabled: oidcConf.GenerateJWTAuthTokenEnabled,
		LicenseType:                     licenseType,
		SecondsUntilLicenseExpiry:       licenseTTL,
		IsManaged:                       log.RedactionPolicyManaged,
	}
	if s.cfg.NodeID != nil {
		args.NodeID = s.cfg.NodeID.String()
	}

	argBytes, err := json.Marshal(args)
	if err != nil {
		log.Error(r.Context(), "unable to serialize ui config args: "+err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := w.Write(argBytes); err != nil {
		log.Error(r.Context(), "unable to write ui config args: "+err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *DynamicUIServer) serveUpgradesPage(w http.ResponseWriter, r *http.Request) {
	major, minor := build.BranchReleaseSeries()
	versions, err := ListUIAssets(major, minor)
	if err != nil {
		log.Error(r.Context(), "Error listing UI assets: "+err.Error())
	}
	currentVersion := DBConsoleVersion.Get(&s.cfg.Settings.SV)
	data := struct {
		CurrentVersion    string
		AvailableVersions []string
	}{
		CurrentVersion:    currentVersion,
		AvailableVersions: versions,
	}

	if s.upgradeTmpl == nil {
		http.Error(w, "Upgrades page not found", http.StatusInternalServerError)
		return
	}

	if err := s.upgradeTmpl.Execute(w, data); err != nil {
		log.Error(r.Context(), "Error executing upgrade template: "+err.Error())
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (s *DynamicUIServer) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	_, err := s.cfg.InternalExecutor.Exec(r.Context(), "set-ui-version", nil, "SET CLUSTER SETTING ui.version = $1", req.Version)
	if err != nil {
		log.Error(r.Context(), "Error setting UI version: "+err.Error())
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	err = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
	if err != nil {
		log.Error(r.Context(), "Error encoding response: "+err.Error())
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}
