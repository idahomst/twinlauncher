package main

// main.go — entry point, settings, Play, Config.wtf / realmlist.wtf handling

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lxn/walk"
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
	Expansion   string `json:"expansion"`
	Realmlist   string `json:"realmlist"`
	CataPath    string `json:"cata_path"`
	MopPath     string `json:"mop_path"`
	VanillaPath string `json:"vanilla_path"`
	// GameLocale is the MoP client language (TwinStar Launcher 1.0.3 "Game language").
	GameLocale string `json:"game_locale"`
	ClearCache bool   `json:"clear_cache"`
	// SkipRealmlistSetup: stored inverted so the JSON zero-value (false)
	// means "do set realmlist" — the correct default for new installs.
	SkipRealmlistSetup bool `json:"skip_realmlist_setup"`
}

// mopLocales mirrors GameLocaleHelper.MopLocaleCodes of TwinStar Launcher 1.0.3.
// Names are plain ASCII so they render with any font under Wine.
var mopLocales = []struct{ Code, Name string }{
	{"enUS", "English (US)"},
	{"enGB", "English (UK)"},
	{"deDE", "German"},
	{"frFR", "French"},
	{"esES", "Spanish (Spain)"},
	{"esMX", "Spanish (Mexico)"},
	{"ptBR", "Portuguese (Brazil)"},
	{"itIT", "Italian"},
	{"ruRU", "Russian"},
	{"koKR", "Korean"},
	{"zhCN", "Chinese (Simplified)"},
	{"zhTW", "Chinese (Traditional)"},
	{"enTW", "English (Taiwan)"},
}

// ── Globals ──────────────────────────────────────────────────────────────────

var (
	appCfg           AppConfig
	userCfg          UserSettings
	userSettingsPath string
)

const userSettingsFile = "twinlauncher_settings.json"

// ── Entry point ──────────────────────────────────────────────────────────────

func main() {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	appSettingsPath := filepath.Join(exeDir, "appsettings.json")
	userSettingsPath = filepath.Join(exeDir, userSettingsFile)

	initHTTP()

	if err := loadAppSettings(appSettingsPath); err != nil {
		msgBox("Fatal Error",
			"Cannot load appsettings.json:\n"+err.Error()+
				"\n\nMake sure twinlauncher.exe is in the same folder as appsettings.json.",
			walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}
	loadUserSettings()

	if userCfg.Expansion == "" {
		userCfg.Expansion = "Mop" // same default as TwinStar Launcher 1.0.3
	}
	if userCfg.Realmlist == "" && len(appCfg.AppSettings.Realmlists) > 0 {
		userCfg.Realmlist = appCfg.AppSettings.Realmlists[0]
	}
	if userCfg.GameLocale == "" {
		userCfg.GameLocale = appCfg.AppSettings.DownloadSettings.MopDefaultLocale
		if userCfg.GameLocale == "" {
			userCfg.GameLocale = "enUS"
		}
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
	return json.Unmarshal(stripBOM(data), &appCfg)
}

func loadUserSettings() {
	data, err := os.ReadFile(userSettingsPath)
	if err != nil {
		return
	}
	json.Unmarshal(stripBOM(data), &userCfg)
}

func saveUserSettings() {
	data, _ := json.MarshalIndent(userCfg, "", "  ")
	if err := os.WriteFile(userSettingsPath, data, 0644); err != nil {
		logLine("WARN: cannot save settings: " + err.Error())
	}
}

// ── Play ─────────────────────────────────────────────────────────────────────

func doPlay() {
	gp := gamePath()
	if gp == "" {
		msgBox("Error", "Game path is not set.", walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}

	if userCfg.ClearCache {
		logLine("Clearing cache…")
		if err := os.RemoveAll(filepath.Join(gp, "Cache")); err != nil {
			logLine("WARN: clear cache: " + err.Error())
		}
	}

	// Like TwinStar Launcher 1.0.3: the locale is only forced for MoP.
	realmlist, locale := "", ""
	if !userCfg.SkipRealmlistSetup {
		realmlist = userCfg.Realmlist
	} else {
		logLine("Skipping realmlist setup (disabled in settings).")
	}
	if userCfg.Expansion == "Mop" {
		locale = userCfg.GameLocale
	}
	if err := setupConfigAndRealmlist(gp, userCfg.Expansion, realmlist, locale); err != nil {
		logLine("WARN: config setup: " + err.Error())
	}

	exeName := findClientExe(gp)
	if exeName == "" {
		msgBox("Error",
			"No WoW executable found in:\n"+gp+
				"\n\nRun 'Check && Update' first.",
			walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}

	cmd := exec.Command(filepath.Join(gp, exeName))
	cmd.Dir = gp
	if err := cmd.Start(); err != nil {
		msgBox("Error", "Failed to launch WoW:\n"+err.Error(), walk.MsgBoxIconError|walk.MsgBoxOK)
		return
	}
	logLine(fmt.Sprintf("Launched %s (PID %d)", exeName, cmd.Process.Pid))
	cmd.Process.Release()
}

func findClientExe(gameDir string) string {
	for _, name := range []string{"Wow-64.exe", "Wow.exe", "WoW.exe"} {
		if _, err := os.Stat(filepath.Join(gameDir, name)); err == nil {
			return name
		}
	}
	return ""
}

// ── Config.wtf / realmlist.wtf ───────────────────────────────────────────────

var (
	reRealmLine  = regexp.MustCompile(`(?i)^SET realmlist`)
	reLocaleLine = regexp.MustCompile(`(?i)^SET locale`)
	reLocaleVal  = regexp.MustCompile(`(?i)^SET locale "([^"]+)"`)
)

// setupConfigAndRealmlist mirrors GameUpdater.SetupConfigAndRealmlist: every
// Config.wtf below gameDir gets the realmlist and locale (each skipped when
// empty), every realmlist.wtf is rewritten when a realmlist is given.
func setupConfigAndRealmlist(gameDir, expansion, realmlist, locale string) error {
	if realmlist == "" && locale == "" {
		return nil
	}
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

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			keep(err)
			continue
		}
		keep(os.WriteFile(cfgPath, []byte(patchConfig(string(data), realmlist, locale)), 0644))
	}

	if realmlist != "" {
		for _, rltPath := range rltFiles {
			content := "set realmlist " + realmlist + "\r\n"
			if expansion == "Cata" {
				content += "set patchlist localhost\r\n"
			}
			keep(os.WriteFile(rltPath, []byte(content), 0644))
		}
	}
	return firstErr
}

// patchConfig replaces (or appends) the realmlist and locale lines of a
// Config.wtf. Empty values are left untouched. Output uses CRLF like the
// original launcher.
func patchConfig(content, realmlist, locale string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSuffix(content, "\n")
	var lines []string
	if content != "" {
		lines = strings.Split(content, "\n")
	}
	foundRealm, foundLocale := false, false
	for i, l := range lines {
		if !foundLocale && locale != "" && reLocaleLine.MatchString(l) {
			lines[i] = `SET locale "` + locale + `"`
			foundLocale = true
		} else if !foundRealm && realmlist != "" && reRealmLine.MatchString(l) {
			lines[i] = `SET realmlist "` + realmlist + `"`
			foundRealm = true
		}
	}
	if !foundLocale && locale != "" {
		lines = append(lines, `SET locale "`+locale+`"`)
	}
	if !foundRealm && realmlist != "" {
		lines = append(lines, `SET realmlist "`+realmlist+`"`)
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// localeFromConfig returns the last `SET locale "xx"` value of WTF/Config.wtf.
func localeFromConfig(gameDir string) string {
	f, err := os.Open(filepath.Join(gameDir, "WTF", "Config.wtf"))
	if err != nil {
		return ""
	}
	defer f.Close()
	var locale string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := reLocaleVal.FindStringSubmatch(sc.Text()); m != nil {
			locale = m[1]
		}
	}
	return locale
}

// ── Misc ─────────────────────────────────────────────────────────────────────

func stripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
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
