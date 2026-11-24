package main

// settings.go — TwinStar download settings (appsettings.json): built-in
// defaults, cached copy, and refresh from the official launcher package.

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type AppConfig struct {
	AppSettings AppSettings `json:"AppSettings"`
}

type AppSettings struct {
	DownloadSettings           DownloadSettings           `json:"DownloadSettings"`
	LauncherUpdaterSettings    LauncherUpdaterSettings    `json:"LauncherUpdaterSettings"`
	Realmlists                 []string                   `json:"Realmlists"`
	ExpansionSelectionSettings ExpansionSelectionSettings `json:"ExpansionSelectionSettings"`
}

type DownloadSettings struct {
	CataManifest         string            `json:"CataManifest"`
	MopManifest          string            `json:"MopManifest"`
	CDNCata              string            `json:"CDNCata"`
	CDNMop               string            `json:"CDNMop"`
	ClientCataAddress    string            `json:"ClientCataAddress"`
	ClientMopAddress     string            `json:"ClientMopAddress"`
	ClientVanillaAddress string            `json:"ClientVanillaAddress"`
	MopPatchUrls         []string          `json:"MopPatchUrls"`
	CataDefaultLocale    string            `json:"CataDefaultLocale"`
	MopDefaultLocale     string            `json:"MopDefaultLocale"`
	DNSFailoverSettings  map[string]string `json:"DNSFailoverSettings"`
}

type LauncherUpdaterSettings struct {
	LauncherDownloadUrl string `json:"LauncherDownloadUrl"`
}

type ExpansionSelectionSettings struct {
	AvailableExpansions []string `json:"AvailableExpansions"`
}

// defaultAppSettings is a copy of appsettings.json from the official TwinStar
// Launcher, so twinlauncher works without it.
//
//go:embed appsettings.default.json
var defaultAppSettings []byte

const (
	// Written by refreshSettings; never edited by hand.
	settingsCacheFile = "twinlauncher_appsettings.json"
	// From the official launcher, used when no cache exists (old setups).
	officialSettingsFile = "appsettings.json"
	// Fallback if the settings do not name the launcher package.
	officialLauncherZip = "https://twinstar-wow.com/launcher/latest.zip"
)

var (
	appCfg         atomic.Pointer[AppConfig]
	settingsSource string // where the startup settings came from (set before the GUI starts)
	exeDir         string
)

// settings returns the active settings. Safe from any goroutine; callers get
// a consistent snapshot even if a refresh swaps the config meanwhile.
func settings() *AppSettings { return &appCfg.Load().AppSettings }

func parseAppConfig(data []byte) (*AppConfig, error) {
	var c AppConfig
	if err := json.Unmarshal(stripBOM(data), &c); err != nil {
		return nil, err
	}
	ds := c.AppSettings.DownloadSettings
	for name, v := range map[string]string{
		"CDNCata": ds.CDNCata, "CDNMop": ds.CDNMop, "CataManifest": ds.CataManifest,
		"MopManifest": ds.MopManifest, "ClientCataAddress": ds.ClientCataAddress,
		"ClientMopAddress": ds.ClientMopAddress, "ClientVanillaAddress": ds.ClientVanillaAddress,
	} {
		if v == "" {
			return nil, fmt.Errorf("DownloadSettings.%s is missing", name)
		}
	}
	if len(c.AppSettings.Realmlists) == 0 {
		return nil, errors.New("Realmlists is empty")
	}
	return &c, nil
}

// loadAppSettings activates the first usable settings of: the cached copy
// from the last refresh, appsettings.json next to the exe, built-in defaults.
func loadAppSettings() {
	for _, f := range []string{settingsCacheFile, officialSettingsFile} {
		p := filepath.Join(exeDir, f)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		c, err := parseAppConfig(data)
		if err != nil {
			settingsSource = fmt.Sprintf("built-in defaults (%s is invalid: %v)", f, err)
			continue
		}
		appCfg.Store(c)
		settingsSource = f
		return
	}
	c, err := parseAppConfig(defaultAppSettings)
	if err != nil {
		panic("embedded appsettings.default.json: " + err.Error())
	}
	appCfg.Store(c)
	if settingsSource == "" {
		settingsSource = "built-in defaults"
	}
}

var refreshMu sync.Mutex

// refreshSettings fetches appsettings.json from the official launcher package
// and activates it when it differs from the current settings. Failures are
// logged and leave the current settings in place.
func refreshSettings(ctx context.Context) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	zipURL := firstNonEmpty(settings().LauncherUpdaterSettings.LauncherDownloadUrl, officialLauncherZip)
	data, err := fetchFileFromZip(ctx, zipURL, officialSettingsFile)
	if err != nil {
		logLine("WARN: could not refresh settings from the TwinStar Launcher package: " + err.Error())
		return
	}
	c, err := parseAppConfig(data)
	if err != nil {
		logLine("WARN: TwinStar Launcher package has unusable settings, keeping current ones: " + err.Error())
		return
	}

	cached, _ := os.ReadFile(filepath.Join(exeDir, settingsCacheFile))
	if !bytes.Equal(cached, data) {
		if err := os.WriteFile(filepath.Join(exeDir, settingsCacheFile), data, 0644); err != nil {
			logLine("WARN: cannot cache settings: " + err.Error())
		}
	}
	if sameSettings(appCfg.Load(), c) {
		logLine("Download settings are up to date with the official TwinStar Launcher.")
		return
	}
	appCfg.Store(c)
	logLine("Download settings updated from the official TwinStar Launcher.")
}

func sameSettings(a, b *AppConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// ── Reading one file out of a remote ZIP with HTTP range requests ───────────

// fetchFileFromZip returns the shortest-path entry whose base name is name
// (case-insensitive) from the ZIP at zipURL, downloading only the ZIP's
// directory and that entry instead of the whole package.
func fetchFileFromZip(ctx context.Context, zipURL, name string) ([]byte, error) {
	size, err := httpContentLength(ctx, zipURL)
	if err != nil {
		return nil, err
	}
	ra := &httpReaderAt{ctx: ctx, url: zipURL, size: size, blocks: map[int64][]byte{}}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("read package: %w", err)
	}
	var found *zip.File
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), name) && (found == nil || len(f.Name) < len(found.Name)) {
			found = f
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%s not found in %s", name, zipURL)
	}
	rc, err := found.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 1<<20))
}

const rangeBlockSize = 64 * 1024

// httpReaderAt reads a remote file in 64 KiB blocks fetched with Range
// requests and kept in memory.
type httpReaderAt struct {
	ctx    context.Context
	url    string
	size   int64
	blocks map[int64][]byte
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		idx := pos / rangeBlockSize
		b, err := r.block(idx)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], b[pos-idx*rangeBlockSize:])
	}
	return n, nil
}

func (r *httpReaderAt) block(idx int64) ([]byte, error) {
	if b, ok := r.blocks[idx]; ok {
		return b, nil
	}
	start := idx * rangeBlockSize
	end := min(start+rangeBlockSize, r.size) - 1
	req, err := newRequest(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := doChecked(httpClient, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("server does not support partial downloads (HTTP %d)", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, rangeBlockSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != end-start+1 {
		return nil, fmt.Errorf("short range response: %d bytes", len(b))
	}
	r.blocks[idx] = b
	return b, nil
}
