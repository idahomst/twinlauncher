package main

// main.go — entry point, config structs, download logic

import (
	"archive/zip"
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ── appsettings.json ─────────────────────────────────────────────────────────

type AppConfig struct {
	AppSettings AppSettings `json:"AppSettings"`
}

type AppSettings struct {
	DownloadSettings           DownloadSettings           `json:"DownloadSettings"`
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

type ExpansionSelectionSettings struct {
	AvailableExpansions []string `json:"AvailableExpansions"`
}

// ── User settings (twinlauncher_settings.json) ───────────────────────────────

type UserSettings struct {
	Expansion          string `json:"expansion"`
	Realmlist          string `json:"realmlist"`
	CataPath           string `json:"cata_path"`
	MopPath            string `json:"mop_path"`
	VanillaPath        string `json:"vanilla_path"`
	ClearCache         bool   `json:"clear_cache"`
	// SkipRealmlistSetup: stored inverted so the JSON zero-value (false)
	// means "do set realmlist" — the correct default for new installs.
	SkipRealmlistSetup bool   `json:"skip_realmlist_setup"`
}

// ── DTOs ─────────────────────────────────────────────────────────────────────

type FileListEntry struct {
	FileName string `json:"fileName"`
	Md5      string `json:"md5"`
}

type ManifestRecord struct {
	Name       string
	Size       int64
	Downloaded int64
}

// ── Globals ──────────────────────────────────────────────────────────────────

var (
	appCfg          AppConfig
	userCfg         UserSettings
	userSettingsPath string
	httpClient      *http.Client
)

const (
	userSettingsFile = "twinlauncher_settings.json"
	dlBufSize        = 65536
)

// ── Entry point ──────────────────────────────────────────────────────────────

func main() {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	appSettingsPath := filepath.Join(exeDir, "appsettings.json")
	userSettingsPath = filepath.Join(exeDir, userSettingsFile)

	// no timeout; manual redirect loop preserves Range header across CDN hops
	httpClient = &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	if err := loadAppSettings(appSettingsPath); err != nil {
		msgBox(0, "Fatal Error",
			"Cannot load appsettings.json:\n"+err.Error()+
				"\n\nMake sure twinlauncher.exe is in the same folder as appsettings.json.",
			MB_OK|MB_ICONERROR)
		return
	}
	loadUserSettings()

	if userCfg.Expansion == "" {
		userCfg.Expansion = "Cata"
	}
	if userCfg.Realmlist == "" && len(appCfg.AppSettings.Realmlists) > 0 {
		userCfg.Realmlist = appCfg.AppSettings.Realmlists[0]
	}

	runGUI()
}

// ── Path helpers ─────────────────────────────────────────────────────────────

func gamePath() string {
	switch userCfg.Expansion {
	case "Cata":
		return userCfg.CataPath
	case "Mop":
		return userCfg.MopPath
	case "Vanilla":
		return userCfg.VanillaPath
	}
	return ""
}

func setGamePath(p string) {
	switch userCfg.Expansion {
	case "Cata":
		userCfg.CataPath = p
	case "Mop":
		userCfg.MopPath = p
	case "Vanilla":
		userCfg.VanillaPath = p
	}
}

// ── Config I/O ───────────────────────────────────────────────────────────────

func loadAppSettings(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &appCfg)
}

func loadUserSettings() {
	data, err := os.ReadFile(userSettingsPath)
	if err != nil {
		return
	}
	json.Unmarshal(data, &userCfg)
}

func saveUserSettings() {
	data, _ := json.MarshalIndent(userCfg, "", "  ")
	os.WriteFile(userSettingsPath, data, 0644)
}

// ── Play ─────────────────────────────────────────────────────────────────────

func doPlay() {
	gp := gamePath()
	if gp == "" {
		msgBox(0, "Error", "Game path is not set.", MB_OK|MB_ICONERROR)
		return
	}

	if !userCfg.SkipRealmlistSetup {
		locale := localeFromConfig(gp)
		if err := setupConfigAndRealmlist(gp, userCfg.Expansion, userCfg.Realmlist, locale); err != nil {
			logLine("WARN: config setup: " + err.Error())
		}
	} else {
		logLine("Skipping realmlist/config setup (disabled in settings).")
	}

	if userCfg.ClearCache {
		logLine("Clearing cache…")
		os.RemoveAll(filepath.Join(gp, "Cache"))
	}

	var exeName string
	for _, name := range []string{"Wow-64.exe", "Wow.exe", "WoW.exe"} {
		if _, err := os.Stat(filepath.Join(gp, name)); err == nil {
			exeName = name
			break
		}
	}
	if exeName == "" {
		msgBox(0, "Error",
			"No WoW executable found in:\n"+gp+
				"\n\nRun 'Check && Update' first.",
			MB_OK|MB_ICONERROR)
		return
	}

	exePath := filepath.Join(gp, exeName)
	cmd := exec.Command(exePath)
	cmd.Dir = gp
	if err := cmd.Start(); err != nil {
		msgBox(0, "Error", "Failed to launch WoW:\n"+err.Error(), MB_OK|MB_ICONERROR)
		return
	}
	logLine(fmt.Sprintf("Launched %s (PID %d)", exeName, cmd.Process.Pid))
}

// ── Update entry points ───────────────────────────────────────────────────────

func updateCata(gameDir string) error {
	ds := appCfg.AppSettings.DownloadSettings

	locale := localeFromConfig(gameDir)
	if locale == "" {
		locale = ds.CataDefaultLocale
		logLine("No locale in Config.wtf, using default: " + locale)
	} else {
		logLine("Detected locale: " + locale)
	}

	logLine("[1/2] Checking client launcher files…")
	if err := updateClientFiles(gameDir, ds.ClientCataAddress); err != nil {
		logLine("WARN: client files check failed: " + err.Error())
	}

	logLine("[2/2] Fetching CDN manifest: " + ds.CataManifest)
	records, err := parseMfil(ds.CDNCata, ds.CataManifest, locale)
	if err != nil {
		return fmt.Errorf("manifest parse failed: %w", err)
	}
	return downloadCDNFiles(gameDir, ds.CDNCata, records)
}

func updateMop(gameDir string) error {
	ds := appCfg.AppSettings.DownloadSettings

	locale := localeFromConfig(gameDir)
	if locale == "" {
		locale = ds.MopDefaultLocale
		logLine("No locale in Config.wtf, using default: " + locale)
	} else {
		logLine("Detected locale: " + locale)
	}

	logLine("[1/3] Checking client launcher files…")
	if err := updateClientFiles(gameDir, ds.ClientMopAddress); err != nil {
		logLine("WARN: client files check failed: " + err.Error())
	}

	logLine("[2/3] Contacting MoP patch server for manifest…")
	cdnBase, manifestName, err := getMopManifest(locale, ds)
	if err != nil {
		logLine("WARN: patch server failed (" + err.Error() + "), using static defaults")
		cdnBase = ds.CDNMop
		manifestName = ds.MopManifest
	}
	logLine("  Manifest: " + manifestName)
	logLine("  CDN:      " + cdnBase)

	logLine("[3/3] Fetching manifest and downloading files…")
	records, err := parseMfil(cdnBase, manifestName, locale)
	if err != nil {
		return fmt.Errorf("manifest parse failed: %w", err)
	}
	return downloadCDNFiles(gameDir, cdnBase, records)
}

func updateVanilla(gameDir string) error {
	if _, err := os.Stat(filepath.Join(gameDir, "WoW.exe")); err == nil {
		logLine("WoW.exe already present, skipping download.")
		return nil
	}
	ds := appCfg.AppSettings.DownloadSettings
	tmpPath := filepath.Join(os.TempDir(), "TwinStar_vanilla_client.tmp")
	logLine("Downloading Vanilla client ZIP (resumable)…")
	if err := downloadZipResumable(ds.ClientVanillaAddress, tmpPath); err != nil {
		return err
	}
	logLine("Extracting ZIP…")
	return extractZip(tmpPath, gameDir)
}

// ── MoP patch server negotiation ─────────────────────────────────────────────

func getMopManifest(locale string, ds DownloadSettings) (cdnBase, manifestName string, err error) {
	body := fmt.Sprintf(
		`<version program="WoW"><record program="Bnet" component="Win" version ="1" />`+
			`<record program="WoW" component="%s" version="4" /></version>`,
		xmlEscape(locale))

	var lastErr error
	for _, patchURL := range ds.MopPatchUrls {
		cdnBase, manifestName, lastErr = tryMopPatchURL(patchURL, locale, []byte(body))
		if lastErr == nil {
			return
		}
		logLine("  WARN patch URL " + patchURL + ": " + lastErr.Error())
	}
	err = lastErr
	return
}

func tryMopPatchURL(patchURL, locale string, body []byte) (cdnBase, manifestName string, err error) {
	resp, err := httpPost(patchURL, body)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	type xmlRec struct {
		Program   string `xml:"program,attr"`
		Component string `xml:"component,attr"`
		Value     string `xml:",chardata"`
	}
	type xmlPatch struct {
		Records []xmlRec `xml:"record"`
	}
	var patch xmlPatch
	if err = xml.NewDecoder(resp.Body).Decode(&patch); err != nil {
		return
	}

	var found *xmlRec
	for i := range patch.Records {
		r := &patch.Records[i]
		if strings.EqualFold(r.Program, "WoW") && strings.EqualFold(r.Component, locale) {
			found = r
			break
		}
	}
	if found == nil {
		err = fmt.Errorf("no WoW/%s record in patch response", locale)
		return
	}

	// Value: "<configURL>;<hash1>;<hash2>;<build>"
	parts := strings.Split(strings.TrimSpace(found.Value), ";")
	if len(parts) < 4 {
		err = fmt.Errorf("unexpected patch value format")
		return
	}
	manifestName = fmt.Sprintf("wow-%s-%s.mfil", parts[3], parts[2])
	configURL := parts[0]
	if strings.HasPrefix(patchURL, "https:") {
		configURL = ensureHTTPS(configURL)
	}

	cfgResp, cerr := httpGet(configURL)
	if cerr != nil {
		err = cerr
		return
	}
	defer cfgResp.Body.Close()

	cfgBytes, rerr := io.ReadAll(cfgResp.Body)
	if rerr != nil {
		err = rerr
		return
	}

	// The XML nesting is deep: config > versioninfo > version > servers > server
	// Use a regex to extract regardless of nesting — same intent as C# LINQ .Descendants()
	reA := regexp.MustCompile(`(?i)<server[^>]+id\s*=\s*"twinwind"[^>]+url\s*=\s*"([^"]+)"`)
	reB := regexp.MustCompile(`(?i)<server[^>]+url\s*=\s*"([^"]+)"[^>]+id\s*=\s*"twinwind"`)
	if m := reA.FindSubmatch(cfgBytes); m != nil {
		cdnBase = string(m[1])
	} else if m := reB.FindSubmatch(cfgBytes); m != nil {
		cdnBase = string(m[1])
	} else {
		err = fmt.Errorf("twinwind server not found in CDN config at %s", configURL)
	}
	return
}

// ── .mfil manifest parser ────────────────────────────────────────────────────

var reLocaleInPath = regexp.MustCompile(`(?i)^Data/([^/]+)/`)

func parseMfil(cdnBase, manifestName, locale string) ([]ManifestRecord, error) {
	resp, err := httpGet(cdnBase + manifestName)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	var records []ManifestRecord
	var cur *ManifestRecord
	var fileLocale, pathLocale string

	flush := func() {
		if cur == nil || cur.Size <= 0 {
			cur = nil
			return
		}
		if fileLocale != "" || pathLocale != "" {
			if !strings.EqualFold(locale, fileLocale) && !strings.EqualFold(locale, pathLocale) {
				cur = nil
				return
			}
		}
		records = append(records, *cur)
		cur = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "file="):
			flush()
			name := line[5:]
			fileLocale, pathLocale = "", ""
			cur = &ManifestRecord{Name: name}
			if m := reLocaleInPath.FindStringSubmatch(name); m != nil {
				fileLocale = m[1]
			}
		case strings.HasPrefix(line, "\tsize=") && cur != nil:
			cur.Size, _ = strconv.ParseInt(line[6:], 10, 64)
		case strings.HasPrefix(line, "\tpath=locale_") && cur != nil:
			pathLocale = line[13:]
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	logLine(fmt.Sprintf("  Manifest: %d files for locale %s", len(records), locale))
	return records, nil
}

// ── CDN bulk download ────────────────────────────────────────────────────────

func downloadCDNFiles(gameDir, cdnBase string, records []ManifestRecord) error {
	var totalSize, totalDone int64
	for i := range records {
		totalSize += records[i].Size
		fi, err := os.Stat(filepath.Join(gameDir, filepath.FromSlash(records[i].Name)))
		if err == nil {
			if fi.Size() >= records[i].Size {
				// File is complete (exact size or newer/larger version)
				records[i].Downloaded = records[i].Size
			} else {
				// Partial download — resume from where we left off
				records[i].Downloaded = fi.Size()
			}
			totalDone += records[i].Downloaded
		}
	}

	var todo []ManifestRecord
	for _, r := range records {
		if r.Downloaded < r.Size {
			todo = append(todo, r)
		}
	}

	logLine(fmt.Sprintf("Files total: %d  (%s)", len(records), fmtBytes(totalSize)))
	logLine(fmt.Sprintf("To download: %d  already done: %s", len(todo), fmtBytes(totalDone)))

	var globalDone int64 = totalDone

	for i, rec := range todo {
		if atomic.LoadInt32(&dlCancel) == 1 {
			return fmt.Errorf("cancelled by user")
		}

		logLine(fmt.Sprintf("[%d/%d] %s (%s)", i+1, len(todo), rec.Name, fmtBytes(rec.Size)))

		destPath := filepath.Join(gameDir, filepath.FromSlash(rec.Name))
		lockPath := destPath + ".lock"
		if _, err := os.Stat(lockPath); err == nil {
			return fmt.Errorf("game has a lock on %s — close WoW first", filepath.Base(lockPath))
		}
		if dir := filepath.Dir(destPath); dir != gameDir {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
		}

		err := downloadFileResume(cdnBase+rec.Name, destPath, rec.Downloaded, rec.Size,
			globalDone, totalSize)
		if err != nil {
			if isHTTP404(err) {
				logLine("  SKIP (404 Not Found)")
				continue
			}
			return fmt.Errorf("downloading %s: %w", rec.Name, err)
		}
		globalDone += rec.Size - rec.Downloaded
	}
	return nil
}

func downloadFileResume(url, destPath string, offset, fileSize, globalDone, globalTotal int64) error {
	resp, err := httpGetRange(url, offset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, dlBufSize)
	downloaded := offset
	var speedBytes int64
	lastReport := time.Now()

	for {
		if atomic.LoadInt32(&dlCancel) == 1 {
			return fmt.Errorf("cancelled")
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			downloaded += int64(n)
			speedBytes += int64(n)
		}
		now := time.Now()
		if now.Sub(lastReport) >= time.Second {
			speed := float64(speedBytes) / now.Sub(lastReport).Seconds()
			pct := 0
			if globalTotal > 0 {
				pct = int((globalDone + downloaded - offset) * 100 / globalTotal)
			}
			setPercent(pct)
			if fileSize > 0 {
				filePct := downloaded * 100 / fileSize
				setStatus(fmt.Sprintf("%d%%  %s/%s  @ %s/s",
					filePct, fmtBytes(downloaded), fmtBytes(fileSize), fmtBytes(int64(speed))))
			}
			speedBytes = 0
			lastReport = now
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// ── Client file update via filelist.json ─────────────────────────────────────

func updateClientFiles(gameDir, baseURL string) error {
	resp, err := httpGet(baseURL + "filelist.json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var entries []FileListEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return err
	}

	var toUpdate []FileListEntry
	for _, e := range entries {
		if strings.EqualFold(e.FileName, "jsonGen.ps1") {
			continue
		}
		dest := filepath.Join(gameDir, filepath.FromSlash(e.FileName))
		if strings.Contains(strings.ToLower(e.FileName), ".wtf") {
			if _, err := os.Stat(dest); os.IsNotExist(err) {
				toUpdate = append(toUpdate, e)
			}
			continue
		}
		needsUpdate := true
		if _, err := os.Stat(dest); err == nil {
			if ok, _ := md5Match(dest, e.Md5); ok {
				needsUpdate = false
			}
		}
		if needsUpdate {
			toUpdate = append(toUpdate, e)
		}
	}

	logLine(fmt.Sprintf("  %d client file(s) need updating", len(toUpdate)))
	for _, e := range toUpdate {
		if atomic.LoadInt32(&dlCancel) == 1 {
			return fmt.Errorf("cancelled")
		}
		logLine("  Downloading: " + e.FileName)
		dest := filepath.Join(gameDir, filepath.FromSlash(e.FileName))
		if dir := filepath.Dir(dest); dir != gameDir {
			os.MkdirAll(dir, 0755)
		}
		if err := downloadToFile(baseURL+e.FileName, dest); err != nil {
			logLine("  WARN: " + err.Error())
		}
	}
	return nil
}

func md5Match(path, expected string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), expected), nil
}

// ── Vanilla ZIP ──────────────────────────────────────────────────────────────

func downloadZipResumable(url, tmpPath string) error {
	resp, err := httpClient.Head(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	totalSize := resp.ContentLength

	var offset int64
	if fi, err := os.Stat(tmpPath); err == nil {
		offset = fi.Size()
		if offset >= totalSize {
			logLine("  Download already complete.")
			return nil
		}
		logLine(fmt.Sprintf("  Resuming from %s / %s", fmtBytes(offset), fmtBytes(totalSize)))
	}
	return downloadFileResume(url, tmpPath, offset, totalSize, offset, totalSize)
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	// Detect single top-level folder (same logic as the original)
	topDirs := map[string]struct{}{}
	for _, f := range r.File {
		parts := strings.SplitN(f.Name, "/", 2)
		if parts[0] != "" {
			topDirs[parts[0]] = struct{}{}
		}
	}
	stripPrefix := ""
	if len(topDirs) == 1 {
		for k := range topDirs {
			stripPrefix = k + "/"
		}
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := f.Name
		if stripPrefix != "" && strings.HasPrefix(rel, stripPrefix) {
			rel = rel[len(stripPrefix):]
		}
		if rel == "" {
			continue
		}
		dest := filepath.Join(destDir, filepath.FromSlash(rel))
		if dir := filepath.Dir(dest); dir != destDir {
			os.MkdirAll(dir, 0755)
		}
		logLine("  Extracting: " + rel)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	os.Remove(zipPath)
	return nil
}

// ── Config.wtf / realmlist.wtf ───────────────────────────────────────────────

var (
	reRealmline = regexp.MustCompile(`(?i)^SET realmlist`)
	reLocaleLine = regexp.MustCompile(`(?i)^SET locale`)
)

func setupConfigAndRealmlist(gameDir, expansion, realmlist, locale string) error {
	var configs, rltFiles []string
	filepath.Walk(gameDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		switch strings.ToLower(info.Name()) {
		case "config.wtf":
			configs = append(configs, path)
		case "realmlist.wtf":
			rltFiles = append(rltFiles, path)
		}
		return nil
	})

	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		foundRealm, foundLocale := false, false
		for i, l := range lines {
			if !foundRealm && reRealmline.MatchString(l) {
				lines[i] = `SET realmlist "` + realmlist + `"`
				foundRealm = true
			} else if !foundLocale && locale != "" && reLocaleLine.MatchString(l) {
				lines[i] = `SET locale "` + locale + `"`
				foundLocale = true
			}
		}
		if !foundRealm {
			lines = append(lines, `SET realmlist "`+realmlist+`"`)
		}
		if !foundLocale && locale != "" {
			lines = append(lines, `SET locale "`+locale+`"`)
		}
		os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0644)
	}

	for _, rltPath := range rltFiles {
		var content string
		if expansion == "Cata" {
			content = "set realmlist " + realmlist + "\nset patchlist localhost\n"
		} else {
			content = "set realmlist " + realmlist + "\n"
		}
		os.WriteFile(rltPath, []byte(content), 0644)
	}
	return nil
}

func localeFromConfig(gameDir string) string {
	cfgPath := filepath.Join(gameDir, "WTF", "Config.wtf")
	f, err := os.Open(cfgPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	re := regexp.MustCompile(`(?i)^SET locale "([^"]+)"`)
	var locale string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			locale = m[1]
		}
	}
	return locale
}

// ── HTTP helpers ─────────────────────────────────────────────────────────────

func httpGet(url string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "TwinStar Launcher")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		resp, err = dnsFailover("GET", url)
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	return resp, nil
}

// httpGetRange performs a GET with manual redirect handling so the Range header
// is preserved across CDN hops (http.Client's auto-redirect would drop it).
func httpGetRange(url string, offset int64) (*http.Response, error) {
	const maxRedirects = 8
	cur := url
	for i := 0; i < maxRedirects; i++ {
		req, _ := http.NewRequest("GET", cur, nil)
		req.Header.Set("User-Agent", "TwinStar Launcher")
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := httpClient.Do(req) // CheckRedirect → ErrUseLastResponse
		if err != nil {
			if i == 0 {
				resp, err = dnsFailover("GET", cur)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Body.Close()
			cur = loc
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, cur)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("too many redirects for %s", url)
}

func httpPost(url string, body []byte) (*http.Response, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("User-Agent", "TwinStar Launcher")
	req.Header.Set("Content-Type", "text/html")
	return c.Do(req)
}

func dnsFailover(method, origURL string) (*http.Response, error) {
	for host, ip := range appCfg.AppSettings.DownloadSettings.DNSFailoverSettings {
		if strings.Contains(origURL, host) {
			newURL := strings.Replace(origURL, host, ip, 1)
			if strings.HasPrefix(newURL, "https://") {
				newURL = "http://" + newURL[8:]
			}
			req, _ := http.NewRequest(method, newURL, nil)
			req.Header.Set("User-Agent", "TwinStar Launcher")
			return http.DefaultClient.Do(req)
		}
	}
	return nil, fmt.Errorf("no DNS failover entry for %s", origURL)
}

func downloadToFile(url, dest string) error {
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func isHTTP404(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

// ── Misc ─────────────────────────────────────────────────────────────────────

func ensureHTTPS(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "https://" + u[7:]
	}
	if !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func fmtBytes(b int64) string {
	if b < 0 {
		return "?"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
