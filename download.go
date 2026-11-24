package main

// download.go — HTTP helpers, update flows, manifest parsing, resumable downloads

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	userAgent       = "TwinStar Launcher"
	dlBufSize       = 64 * 1024
	stallTimeout    = 60 * time.Second // abort a download when no data arrives for this long
	smallGetTimeout = 2 * time.Minute  // whole-request limit for manifests, file lists, patch XML
	maxRedirects    = 8
)

// ── HTTP ─────────────────────────────────────────────────────────────────────

var (
	httpClient     *http.Client // follows redirects
	httpNoRedirect *http.Client // redirects followed manually so Range survives CDN hops
)

func initHTTP() {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	httpClient = &http.Client{Transport: tr}
	httpNoRedirect = &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type httpStatusError struct {
	Code int
	URL  string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d for %s", e.Code, e.URL) }

func isHTTP404(err error) bool {
	var se *httpStatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

func newRequest(ctx context.Context, method, u string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

func doChecked(c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, &httpStatusError{resp.StatusCode, req.URL.String()}
	}
	return resp, nil
}

// failoverURL rewrites the host of rawURL to its DNSFailoverSettings IP (plain
// http), like DownloaderService.ExecuteWithFailover.
func failoverURL(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	ip, ok := settings().DownloadSettings.DNSFailoverSettings[strings.ToLower(u.Hostname())]
	if !ok || ip == "" {
		return "", false
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(ip, port)
	} else {
		u.Host = ip
	}
	u.Scheme = "http"
	return u.String(), true
}

// withFailover runs fn on rawURL and, if that fails and the host has a DNS
// failover entry, once more on the failover URL.
func withFailover[T any](ctx context.Context, rawURL string, fn func(u string) (T, error)) (T, error) {
	res, err := fn(rawURL)
	if err == nil || ctx.Err() != nil {
		return res, err
	}
	alt, ok := failoverURL(rawURL)
	if !ok {
		return res, err
	}
	logLine(fmt.Sprintf("  %v — retrying via %s", err, alt))
	return fn(alt)
}

// httpGetBytes fetches a small resource completely.
func httpGetBytes(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, smallGetTimeout)
	defer cancel()
	return withFailover(ctx, rawURL, func(u string) ([]byte, error) {
		req, err := newRequest(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := doChecked(httpClient, req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	})
}

func httpPostBytes(ctx context.Context, rawURL string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, smallGetTimeout)
	defer cancel()
	return withFailover(ctx, rawURL, func(u string) ([]byte, error) {
		req, err := newRequest(ctx, http.MethodPost, u, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/html")
		resp, err := doChecked(httpClient, req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	})
}

// httpContentLength returns the size reported by a HEAD request.
func httpContentLength(ctx context.Context, rawURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, smallGetTimeout)
	defer cancel()
	return withFailover(ctx, rawURL, func(u string) (int64, error) {
		req, err := newRequest(ctx, http.MethodHead, u, nil)
		if err != nil {
			return 0, err
		}
		resp, err := doChecked(httpClient, req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		if resp.ContentLength <= 0 {
			return 0, fmt.Errorf("server did not report a size for %s", u)
		}
		return resp.ContentLength, nil
	})
}

// httpGetRange GETs rawURL from offset, following redirects by hand so the
// Range header is sent on every hop (and on the failover request).
func httpGetRange(ctx context.Context, rawURL string, offset int64) (*http.Response, error) {
	return withFailover(ctx, rawURL, func(u string) (*http.Response, error) {
		cur := u
		for i := 0; i < maxRedirects; i++ {
			req, err := newRequest(ctx, http.MethodGet, cur, nil)
			if err != nil {
				return nil, err
			}
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}
			resp, err := httpNoRedirect.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				loc, lerr := resp.Location()
				resp.Body.Close()
				if lerr != nil {
					return nil, fmt.Errorf("HTTP %d without Location for %s", resp.StatusCode, cur)
				}
				cur = loc.String()
				continue
			}
			if resp.StatusCode >= 400 {
				resp.Body.Close()
				return nil, &httpStatusError{resp.StatusCode, cur}
			}
			return resp, nil
		}
		return nil, fmt.Errorf("too many redirects for %s", u)
	})
}

// ── Progress ─────────────────────────────────────────────────────────────────

type progress struct {
	total, done int64 // bytes, whole job (total <= 0: unknown)
	label       string
	fileSize    int64
	fileDone    int64
	lastReport  time.Time
	sinceLast   int64
}

func newProgress(total, done int64) *progress {
	return &progress{total: total, done: done, lastReport: time.Now()}
}

func (p *progress) startFile(label string, size, already int64) {
	p.label, p.fileSize, p.fileDone = label, size, already
}

// add records n downloaded bytes (negative after a restart) and refreshes
// the UI at most once per second.
func (p *progress) add(n int64) {
	p.done += n
	p.fileDone += n
	p.sinceLast += n
	now := time.Now()
	elapsed := now.Sub(p.lastReport)
	if elapsed < time.Second {
		return
	}
	speed := int64(float64(max(p.sinceLast, 0)) / elapsed.Seconds())
	status := p.label + "  " + fmtBytes(p.fileDone)
	if p.fileSize > 0 {
		status += " / " + fmtBytes(p.fileSize)
	}
	if p.total > 0 {
		pct := int(p.done * 100 / p.total)
		setPercent(pct)
		status += fmt.Sprintf("  |  total %d%%", pct)
	}
	setStatus(status + "  |  " + fmtBytes(speed) + "/s")
	p.lastReport, p.sinceLast = now, 0
}

// ── Resumable single-file download ───────────────────────────────────────────

// downloadFile writes rawURL to dest, resuming at offset (dest must then hold
// exactly offset bytes). size is the expected final size, or <= 0 if unknown.
func downloadFile(ctx context.Context, rawURL, dest string, offset, size int64, prog *progress) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := httpGetRange(reqCtx, rawURL, offset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
			return fmt.Errorf("unexpected Content-Range %q, wanted offset %d", cr, offset)
		}
		flags |= os.O_APPEND
	case offset > 0:
		// The server ignored Range and sent the whole file: start over
		// instead of appending it to the partial copy.
		logLine("  server ignored the resume request, downloading the whole file again")
		prog.add(-offset)
		offset = 0
		flags |= os.O_TRUNC
	default:
		flags |= os.O_TRUNC
	}
	if size <= 0 && resp.ContentLength > 0 {
		size = offset + resp.ContentLength
	}

	clearReadOnly(dest)
	f, err := os.OpenFile(dest, flags, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	buf := make([]byte, dlBufSize)
	written := offset
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			prog.add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if stalled.Load() && ctx.Err() == nil {
				return fmt.Errorf("download stalled (no data for %s)", stallTimeout)
			}
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if size > 0 && written != size {
		return fmt.Errorf("incomplete download: got %s of %s", fmtBytes(written), fmtBytes(size))
	}
	return nil
}

// ── Existing / pre-allocated files ───────────────────────────────────────────

const (
	fileAttributeSparse = 0x200 // FILE_ATTRIBUTE_SPARSE_FILE

	// Layout of a pre-allocated download (Blizzard downloader, TwinStar
	// Launcher): <size bytes of data><1 bitmap byte per 128 KiB chunk><24 bytes>.
	// Each bitmap bit marks one finished 16 KiB sub-chunk.
	sparseChunkSize    = 128 * 1024
	sparseSubChunkSize = 16 * 1024
	sparseFooterSize   = 24
)

func isSparse(fi os.FileInfo) bool {
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return a.FileAttributes&fileAttributeSparse != 0
	}
	return false
}

func clearReadOnly(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0200 == 0 {
		os.Chmod(path, fi.Mode().Perm()|0200)
	}
}

// existingBytes returns how many bytes of path can be kept for a file of the
// given size. Mirrors GameUpdater.HandleExistingFilesAndUpdateRecords: files
// that are larger than expected, or sparse with the wrong length, are
// pre-allocated downloads and go through handleSparseFile.
func existingBytes(path string, size int64) (int64, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s is a directory", path)
	}
	if (isSparse(fi) && fi.Size() != size) || fi.Size() > size {
		return handleSparseFile(path, fi.Size(), size)
	}
	return fi.Size(), nil
}

// handleSparseFile mirrors GameUpdater.HandleSparseFile: if the file carries a
// chunk bitmap, it is cut back to the first unfinished sub-chunk; otherwise it
// is unusable and deleted. Unlike the original, a file whose bitmap says
// everything is done is truncated to its real size (dropping the trailer).
func handleSparseFile(path string, length, size int64) (int64, error) {
	clearReadOnly(path)
	chunks := (size + sparseChunkSize - 1) / sparseChunkSize
	var keep int64
	if length == size+chunks+sparseFooterSize {
		var err error
		if keep, err = offsetFromChunkBitmap(path, size, chunks); err != nil {
			return 0, err
		}
	}
	if keep <= 0 {
		logLine("  discarding unusable partial file " + filepath.Base(path))
		if err := os.Remove(path); err != nil {
			return 0, err
		}
		return 0, nil
	}
	keep = min(keep, size)
	if err := os.Truncate(path, keep); err != nil {
		return 0, err
	}
	return keep, nil
}

func offsetFromChunkBitmap(path string, size, chunks int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	bitmap := make([]byte, chunks)
	if _, err := f.ReadAt(bitmap, size); err != nil && err != io.EOF {
		return 0, err
	}
	for i, b := range bitmap {
		if b == 0xFF {
			continue
		}
		off := int64(i) * sparseChunkSize
		for b&1 != 0 {
			off += sparseSubChunkSize
			b >>= 1
		}
		return off, nil
	}
	return size, nil
}

// localPath joins a server-supplied relative path onto base, refusing
// absolute paths, ".." escapes and reserved device names.
func localPath(base, rel string) (string, error) {
	p := filepath.FromSlash(rel)
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("refusing unsafe path %q", rel)
	}
	return filepath.Join(base, p), nil
}

// ── Update entry points ───────────────────────────────────────────────────────

// runUpdate checks and downloads everything for one expansion. mopLocale is
// the selected MoP game language (ignored for other expansions).
func runUpdate(ctx context.Context, expansion, gameDir, mopLocale string) error {
	if err := checkClientVersion(gameDir, expansion); err != nil {
		return err
	}
	switch expansion {
	case "Cata":
		return updateCata(ctx, gameDir)
	case "Mop":
		return updateMop(ctx, gameDir, mopLocale)
	case "Vanilla":
		return updateVanilla(ctx, gameDir)
	}
	return fmt.Errorf("unknown expansion %q", expansion)
}

func updateCata(ctx context.Context, gameDir string) error {
	ds := settings().DownloadSettings

	logLine("[1/2] Checking client files…")
	clientErr := updateClientFiles(ctx, gameDir, ds.ClientCataAddress)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Read after the client files: a fresh install gets Config.wtf from them.
	locale := localeFromConfig(gameDir)
	if locale == "" {
		locale = firstNonEmpty(ds.CataDefaultLocale, "enUS")
		logLine("No locale in Config.wtf, using default: " + locale)
	} else {
		logLine("Detected locale: " + locale)
	}

	logLine("[2/2] Fetching CDN manifest: " + ds.CataManifest)
	records, err := fetchManifest(ctx, ds.CDNCata, ds.CataManifest, locale)
	if err != nil {
		return err
	}
	if err := downloadCDNFiles(ctx, gameDir, ds.CDNCata, records); err != nil {
		return err
	}
	return clientErr
}

func updateMop(ctx context.Context, gameDir, locale string) error {
	ds := settings().DownloadSettings

	logLine("[1/3] Checking client files…")
	clientErr := updateClientFiles(ctx, gameDir, ds.ClientMopAddress)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if locale == "" {
		locale = firstNonEmpty(localeFromConfig(gameDir), ds.MopDefaultLocale, "enUS")
	}
	logLine("Game language: " + locale)
	// Keep Config.wtf in line with the data we are about to download.
	if err := setupConfigAndRealmlist(gameDir, "Mop", "", locale); err != nil {
		logLine("WARN: config setup: " + err.Error())
	}

	logLine("[2/3] Contacting MoP patch server for manifest…")
	cdnBase, manifestName := getMopManifest(ctx, locale, ds.MopPatchUrls)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cdnBase == "" {
		cdnBase = ds.CDNMop
	}
	if manifestName == "" {
		manifestName = ds.MopManifest
	}
	logLine("  Manifest: " + manifestName)
	logLine("  CDN:      " + cdnBase)

	logLine("[3/3] Fetching manifest and downloading files…")
	records, err := fetchManifest(ctx, cdnBase, manifestName, locale)
	if err != nil {
		return err
	}
	if err := downloadCDNFiles(ctx, gameDir, cdnBase, records); err != nil {
		return err
	}
	return clientErr
}

func updateVanilla(ctx context.Context, gameDir string) error {
	if _, err := os.Stat(filepath.Join(gameDir, "WoW.exe")); err == nil {
		logLine("WoW.exe already present, skipping download.")
		return nil
	}
	zipURL := settings().DownloadSettings.ClientVanillaAddress
	// Same temp file as the original launcher, so either can resume it.
	tmpPath := filepath.Join(os.TempDir(), "TwinStar_vanilla_client.tmp")

	total, err := httpContentLength(ctx, zipURL)
	if err != nil {
		return err
	}
	var offset int64
	if fi, err := os.Stat(tmpPath); err == nil {
		offset = fi.Size()
		if offset > total {
			logLine("  Temp file is larger than the server copy, starting over.")
			if err := os.Remove(tmpPath); err != nil {
				return err
			}
			offset = 0
		}
	}

	if offset < total {
		if offset > 0 {
			logLine(fmt.Sprintf("Resuming Vanilla client ZIP at %s / %s", fmtBytes(offset), fmtBytes(total)))
		} else {
			logLine("Downloading Vanilla client ZIP (" + fmtBytes(total) + ")…")
		}
		prog := newProgress(total, offset)
		prog.startFile("Vanilla client", total, offset)
		if err := downloadFile(ctx, zipURL, tmpPath, offset, total, prog); err != nil {
			return err
		}
	} else {
		logLine("  Download already complete.")
	}

	logLine("Extracting ZIP…")
	if err := extractZip(tmpPath, gameDir); err != nil {
		return err
	}
	os.Remove(tmpPath)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── MoP patch server negotiation ─────────────────────────────────────────────

// getMopManifest asks the patch servers for the current CDN and manifest,
// like GameUpdater.DownloadPatchContent. Empty results mean "use defaults".
func getMopManifest(ctx context.Context, locale string, patchURLs []string) (cdnBase, manifestName string) {
	var body bytes.Buffer
	body.WriteString(`<version program="WoW"><record program="Bnet" component="Win" version ="1" /><record program="WoW" component="`)
	xml.EscapeText(&body, []byte(locale))
	body.WriteString(`" version="4" /></version>`)

	for _, patchURL := range patchURLs {
		cdn, manifest, err := tryMopPatchURL(ctx, patchURL, locale, body.Bytes())
		if err == nil {
			return cdn, manifest
		}
		if ctx.Err() != nil {
			return "", ""
		}
		logLine("  WARN patch URL " + patchURL + ": " + err.Error())
	}
	logLine("  WARN: no patch server answered, using defaults from appsettings.json")
	return "", ""
}

func tryMopPatchURL(ctx context.Context, patchURL, locale string, body []byte) (cdnBase, manifestName string, err error) {
	resp, err := httpPostBytes(ctx, patchURL, body)
	if err != nil {
		return "", "", err
	}
	value, found, err := findXMLElement(resp, "record", func(attrs map[string]string) bool {
		return strings.EqualFold(attrs["program"], "WoW") && strings.EqualFold(attrs["component"], locale)
	})
	if err != nil {
		return "", "", fmt.Errorf("patch response: %w", err)
	}
	if !found {
		logLine("  WARN: no WoW/" + locale + " record in patch response")
		return "", "", nil
	}

	// Value: "<configURL>;<hash1>;<hash2>;<build>"
	parts := strings.Split(strings.TrimSpace(value.text), ";")
	if len(parts) < 4 {
		logLine("  WARN: unexpected patch record format")
		return "", "", nil
	}
	manifestName = fmt.Sprintf("wow-%s-%s.mfil", parts[3], parts[2])
	configURL := parts[0]
	if strings.HasPrefix(patchURL, "https:") {
		configURL = ensureHTTPS(configURL)
	}

	cfg, err := httpGetBytes(ctx, configURL)
	if err != nil {
		return "", "", err
	}
	server, found, err := findXMLElement(cfg, "server", func(attrs map[string]string) bool {
		return strings.EqualFold(attrs["id"], "twinwind")
	})
	if err != nil {
		return "", "", fmt.Errorf("CDN config: %w", err)
	}
	if found {
		cdnBase = server.attrs["url"]
	}
	return cdnBase, manifestName, nil
}

type xmlElement struct {
	attrs map[string]string
	text  string
}

// findXMLElement returns the first element named name (at any depth) whose
// attributes satisfy match — the equivalent of LINQ Descendants().First().
func findXMLElement(data []byte, name string, match func(map[string]string) bool) (xmlElement, bool, error) {
	d := xml.NewDecoder(bytes.NewReader(stripBOM(data)))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return xmlElement{}, false, nil
		}
		if err != nil {
			return xmlElement{}, false, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || !strings.EqualFold(se.Name.Local, name) {
			continue
		}
		attrs := make(map[string]string, len(se.Attr))
		for _, a := range se.Attr {
			attrs[strings.ToLower(a.Name.Local)] = a.Value
		}
		if !match(attrs) {
			continue
		}
		var v struct {
			Text string `xml:",chardata"`
		}
		if err := d.DecodeElement(&v, &se); err != nil {
			return xmlElement{}, false, err
		}
		return xmlElement{attrs: attrs, text: v.Text}, true, nil
	}
}

func ensureHTTPS(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "https://" + u[7:]
	}
	if !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

// ── .mfil manifest ───────────────────────────────────────────────────────────

var reLocaleInPath = regexp.MustCompile(`(?i)^Data/([^/]+)/`)

type ManifestRecord struct {
	Name       string
	Size       int64
	Path       string // local destination
	Downloaded int64  // bytes already present
}

func fetchManifest(ctx context.Context, cdnBase, manifestName, locale string) ([]ManifestRecord, error) {
	data, err := httpGetBytes(ctx, cdnBase+manifestName)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	records, err := parseMfil(bytes.NewReader(data), locale)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	logLine(fmt.Sprintf("  Manifest: %d files for locale %s", len(records), locale))
	return records, nil
}

// parseMfil mirrors GameUpdater.ParseManifestFileForLocale: records with a
// positive size, filtered to the given locale when they carry one.
func parseMfil(r io.Reader, locale string) ([]ManifestRecord, error) {
	var (
		records               []ManifestRecord
		name                  string
		size                  int64
		fileLocale, pathLocal string
	)
	flush := func() {
		if name == "" || size <= 0 {
			return
		}
		if (fileLocale != "" || pathLocal != "") &&
			!strings.EqualFold(locale, fileLocale) && !strings.EqualFold(locale, pathLocal) {
			return
		}
		records = append(records, ManifestRecord{Name: name, Size: size})
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "file="):
			flush()
			name = line[len("file="):]
			// Quirk kept from the original: "Updates/" entries inherit the
			// size and locale state of the previous entry.
			if !strings.HasPrefix(name, "Updates/") {
				size, pathLocal = 0, ""
				fileLocale = ""
				if m := reLocaleInPath.FindStringSubmatch(name); m != nil {
					fileLocale = m[1]
				}
			}
		case strings.HasPrefix(line, "\tsize="):
			v, err := strconv.ParseInt(line[len("\tsize="):], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad size for %s: %w", name, err)
			}
			size = v
		case strings.HasPrefix(line, "\tpath=locale_"):
			pathLocal = line[len("\tpath=locale_"):]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return records, nil
}

// ── CDN bulk download ────────────────────────────────────────────────────────

func downloadCDNFiles(ctx context.Context, gameDir, cdnBase string, records []ManifestRecord) error {
	var totalSize, totalDone int64
	for i := range records {
		r := &records[i]
		dest, err := localPath(gameDir, r.Name)
		if err != nil {
			return err
		}
		r.Path = dest
		if r.Downloaded, err = existingBytes(dest, r.Size); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		totalSize += r.Size
		totalDone += r.Downloaded
	}

	var todo []ManifestRecord
	for _, r := range records {
		if r.Downloaded < r.Size {
			todo = append(todo, r)
		}
	}
	logLine(fmt.Sprintf("Files total: %d  (%s)", len(records), fmtBytes(totalSize)))
	logLine(fmt.Sprintf("To download: %d  already done: %s", len(todo), fmtBytes(totalDone)))

	prog := newProgress(totalSize, totalDone)
	skipped := 0
	for i, rec := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		logLine(fmt.Sprintf("[%d/%d] %s (%s)", i+1, len(todo), rec.Name, fmtBytes(rec.Size)))

		lockPath := rec.Path + ".lock"
		if _, err := os.Stat(lockPath); err == nil {
			return fmt.Errorf("the game has a lock on %s — close WoW or delete %s first",
				rec.Name, filepath.Base(lockPath))
		}
		if err := os.MkdirAll(filepath.Dir(rec.Path), 0755); err != nil {
			return err
		}

		prog.startFile(fmt.Sprintf("File %d/%d", i+1, len(todo)), rec.Size, rec.Downloaded)
		err := downloadFile(ctx, cdnBase+rec.Name, rec.Path, rec.Downloaded, rec.Size, prog)
		if isHTTP404(err) {
			logLine("  SKIP (404 Not Found)")
			skipped++
			continue
		}
		if err != nil {
			return fmt.Errorf("downloading %s: %w", rec.Name, err)
		}
	}
	if skipped > 0 {
		logLine(fmt.Sprintf("WARN: %d file(s) were missing on the CDN and were skipped", skipped))
	}
	return nil
}

// ── Client files (filelist.json) ─────────────────────────────────────────────

type FileListEntry struct {
	FileName string `json:"fileName"`
	Md5      string `json:"md5"`
}

// updateClientFiles mirrors GetClientFilesToUpdateAsync/UpdateClientFilesAsync:
// .wtf files are only created when missing, everything else is replaced when
// its MD5 differs. Files are downloaded to a temp name and then swapped in.
func updateClientFiles(ctx context.Context, gameDir, baseURL string) error {
	data, err := httpGetBytes(ctx, baseURL+"filelist.json")
	if err != nil {
		return fmt.Errorf("file list: %w", err)
	}
	var entries []FileListEntry
	if err := json.Unmarshal(stripBOM(data), &entries); err != nil {
		return fmt.Errorf("file list: %w", err)
	}

	type item struct {
		FileListEntry
		dest string
	}
	var toUpdate []item
	for _, e := range entries {
		if strings.EqualFold(e.FileName, "jsonGen.ps1") {
			continue
		}
		dest, err := localPath(gameDir, e.FileName)
		if err != nil {
			logLine("  WARN: " + err.Error())
			continue
		}
		_, statErr := os.Stat(dest)
		if strings.Contains(strings.ToLower(e.FileName), ".wtf") {
			if os.IsNotExist(statErr) {
				toUpdate = append(toUpdate, item{e, dest})
			}
			continue
		}
		if statErr == nil {
			if ok, _ := md5Match(dest, e.Md5); ok {
				continue
			}
		}
		toUpdate = append(toUpdate, item{e, dest})
	}

	logLine(fmt.Sprintf("  %d client file(s) need updating", len(toUpdate)))
	failed := 0
	for i, e := range toUpdate {
		if err := ctx.Err(); err != nil {
			return err
		}
		logLine("  Downloading: " + e.FileName)
		prog := newProgress(0, 0)
		prog.startFile(fmt.Sprintf("Client file %d/%d", i+1, len(toUpdate)), 0, 0)
		if err := replaceFile(ctx, baseURL+e.FileName, e.dest, e.Md5, prog); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logLine("  WARN: " + e.FileName + ": " + err.Error())
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d client file(s) could not be updated (is WoW running?)", failed)
	}
	return nil
}

// replaceFile downloads to dest+".part", checks the MD5 and renames it over
// dest, so an interrupted download never leaves a broken Wow.exe behind.
func replaceFile(ctx context.Context, rawURL, dest, wantMd5 string, prog *progress) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	tmp := dest + ".part"
	defer os.Remove(tmp)
	if err := downloadFile(ctx, rawURL, tmp, 0, 0, prog); err != nil {
		return err
	}
	if wantMd5 != "" {
		if ok, err := md5Match(tmp, wantMd5); err != nil {
			return err
		} else if !ok {
			logLine("  WARN: MD5 of " + filepath.Base(dest) + " does not match filelist.json")
		}
	}
	clearReadOnly(dest)
	return os.Rename(tmp, dest)
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

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	// A single top-level folder is stripped (same logic as the original).
	topDirs := map[string]struct{}{}
	for _, f := range r.File {
		if top, _, _ := strings.Cut(f.Name, "/"); top != "" {
			topDirs[top] = struct{}{}
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
		rel := strings.TrimPrefix(f.Name, stripPrefix)
		if rel == "" {
			continue
		}
		dest, err := localPath(destDir, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		logLine("  Extracting: " + rel)
		if err := extractZipFile(f, dest); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil
}

func extractZipFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	clearReadOnly(dest)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
