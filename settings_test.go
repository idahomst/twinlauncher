package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// settingsJSON returns the built-in settings with the first realmlist replaced.
func settingsJSON(t *testing.T, realm string) []byte {
	t.Helper()
	c, err := parseAppConfig(defaultAppSettings)
	if err != nil {
		t.Fatal(err)
	}
	c.AppSettings.Realmlists = []string{realm}
	b, _ := json.Marshal(c)
	return b
}

// launcherZip builds a package like the official latest.zip: a large binary
// plus appsettings.json (and a deeper decoy).
func launcherZip(t *testing.T, settings []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	big := make([]byte, 3<<20)
	rand.Read(big)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"TwinStar Launcher.dll", big},
		{"runtimes/win/lib/appsettings.json", []byte("{decoy")},
		{"appsettings.json", settings},
	} {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate})
		w.Write(f.data)
	}
	zw.Close()
	return buf.Bytes()
}

// zipServer serves data with Range support (http.ServeContent) and counts
// the body bytes sent.
func zipServer(t *testing.T, data []byte, ranges bool) (*httptest.Server, *atomic.Int64) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ranges {
			r.Header.Del("Range")
			w.Header().Set("Accept-Ranges", "none")
		}
		cw := &countingWriter{ResponseWriter: w, n: &sent}
		http.ServeContent(cw, r, "latest.zip", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.ResponseWriter.Write(p)
}

func TestFetchFileFromZipReadsOnlyWhatItNeeds(t *testing.T) {
	want := settingsJSON(t, "login.example")
	data := launcherZip(t, want)
	srv, sent := zipServer(t, data, true)

	got, err := fetchFileFromZip(context.Background(), srv.URL+"/latest.zip", "appsettings.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if n := sent.Load(); n > 256<<10 {
		t.Fatalf("downloaded %d of %d bytes, expected only a few blocks", n, len(data))
	}
}

func TestFetchFileFromZipNeedsRanges(t *testing.T) {
	srv, _ := zipServer(t, launcherZip(t, settingsJSON(t, "x")), false)
	if _, err := fetchFileFromZip(context.Background(), srv.URL+"/latest.zip", "appsettings.json"); err == nil {
		t.Fatal("expected an error without Range support")
	}
}

func TestParseAppConfigRejectsIncomplete(t *testing.T) {
	if _, err := parseAppConfig([]byte(`{"AppSettings":{"Realmlists":["a"]}}`)); err == nil {
		t.Fatal("settings without download URLs accepted")
	}
	if _, err := parseAppConfig(defaultAppSettings); err != nil {
		t.Fatalf("built-in settings rejected: %v", err)
	}
}

func useExeDir(t *testing.T) string {
	t.Helper()
	orig, origCfg, origSrc := exeDir, appCfg.Load(), settingsSource
	exeDir = t.TempDir()
	settingsSource = ""
	t.Cleanup(func() { exeDir, settingsSource = orig, origSrc; appCfg.Store(origCfg) })
	return exeDir
}

func TestLoadAppSettingsPriority(t *testing.T) {
	dir := useExeDir(t)

	loadAppSettings()
	if settingsSource != "built-in defaults" {
		t.Fatalf("source %q", settingsSource)
	}

	writeFile(t, filepath.Join(dir, officialSettingsFile), settingsJSON(t, "official.example"))
	loadAppSettings()
	if settings().Realmlists[0] != "official.example" {
		t.Fatalf("appsettings.json ignored: %v", settings().Realmlists)
	}

	writeFile(t, filepath.Join(dir, settingsCacheFile), settingsJSON(t, "cached.example"))
	loadAppSettings()
	if settings().Realmlists[0] != "cached.example" {
		t.Fatalf("cache ignored: %v", settings().Realmlists)
	}

	writeFile(t, filepath.Join(dir, settingsCacheFile), []byte("{broken"))
	loadAppSettings()
	if settings().Realmlists[0] != "official.example" {
		t.Fatalf("broken cache not skipped: %v", settings().Realmlists)
	}
}

func TestRefreshSettings(t *testing.T) {
	dir := useExeDir(t)
	loadAppSettings()

	fresh := settingsJSON(t, "fresh.example")
	srv, _ := zipServer(t, launcherZip(t, fresh), true)
	withSettings(t, func(c *AppConfig) {
		c.AppSettings.LauncherUpdaterSettings.LauncherDownloadUrl = srv.URL + "/latest.zip"
	})

	refreshSettings(context.Background())
	if settings().Realmlists[0] != "fresh.example" {
		t.Fatalf("not refreshed: %v", settings().Realmlists)
	}
	if cached, _ := os.ReadFile(filepath.Join(dir, settingsCacheFile)); !bytes.Equal(cached, fresh) {
		t.Fatal("refreshed settings not cached")
	}

	// A broken package keeps the current settings.
	bad, _ := zipServer(t, launcherZip(t, []byte(`{"AppSettings":{}}`)), true)
	withSettings(t, func(c *AppConfig) {
		c.AppSettings.Realmlists = []string{"current.example"}
		c.AppSettings.LauncherUpdaterSettings.LauncherDownloadUrl = bad.URL + "/latest.zip"
	})
	refreshSettings(context.Background())
	if !strings.HasPrefix(settings().Realmlists[0], "current.example") {
		t.Fatalf("broken package replaced settings: %v", settings().Realmlists)
	}
}
