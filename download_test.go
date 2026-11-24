package main

// Tests build for GOOS=windows only (lxn/walk). Run them under Wine:
//   GOOS=windows go test -exec wine .

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func init() { initHTTP() }

func TestFileListWithBOM(t *testing.T) {
	data := []byte("\xEF\xBB\xBF[{\"fileName\":\"Wow.exe\",\"md5\":\"abc\"}]")
	var entries []FileListEntry
	if err := json.Unmarshal(stripBOM(data), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].FileName != "Wow.exe" {
		t.Fatalf("got %+v", entries)
	}
}

func TestParseMfil(t *testing.T) {
	mfil := strings.Join([]string{
		"file=Data", "\tsize=0",
		"file=Data/enGB", "\tsize=0",
		"file=Data/art.MPQ", "\tsize=100",
		"file=Data/enGB/locale-enGB.MPQ", "\tsize=200",
		"file=Data/deDE/locale-deDE.MPQ", "\tsize=300",
		"file=Data/expansion3.MPQ", "\tsize=50", "\tpath=locale_deDE",
		"file=Data/expansion4.MPQ", "\tsize=60", "\tpath=locale_enGB",
	}, "\r\n") + "\r\n"
	recs, err := parseMfil(strings.NewReader(mfil), "enGB")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, fmt.Sprintf("%s:%d", r.Name, r.Size))
	}
	want := "Data/art.MPQ:100 Data/enGB/locale-enGB.MPQ:200 Data/expansion4.MPQ:60"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestLocalPath(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{"../evil.exe", "Data/../../evil", `C:\Windows\evil`, "/abs", "NUL"} {
		if _, err := localPath(base, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	p, err := localPath(base, "Data/enGB/realmlist.wtf")
	if err != nil || p != filepath.Join(base, "Data", "enGB", "realmlist.wtf") {
		t.Fatalf("got %q, %v", p, err)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestExistingBytes(t *testing.T) {
	dir := t.TempDir()
	const size = 3 * sparseChunkSize

	// missing
	if n, err := existingBytes(filepath.Join(dir, "none"), size); n != 0 || err != nil {
		t.Fatalf("missing: %d %v", n, err)
	}

	// partial → resume
	p := filepath.Join(dir, "partial")
	writeFile(t, p, make([]byte, 1000))
	if n, _ := existingBytes(p, size); n != 1000 {
		t.Fatalf("partial: %d", n)
	}

	// oversized without a bitmap trailer → deleted
	p = filepath.Join(dir, "oversized")
	writeFile(t, p, make([]byte, size+10))
	if n, _ := existingBytes(p, size); n != 0 {
		t.Fatalf("oversized: %d", n)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("oversized file not removed")
	}

	// pre-allocated with bitmap: chunk 0 done, chunk 1 has 2 sub-chunks
	p = filepath.Join(dir, "sparse")
	data := make([]byte, size+3+sparseFooterSize)
	copy(data[size:], []byte{0xFF, 0x03, 0x00})
	writeFile(t, p, data)
	want := int64(sparseChunkSize + 2*sparseSubChunkSize)
	if n, err := existingBytes(p, size); n != want || err != nil {
		t.Fatalf("sparse: %d %v, want %d", n, err, want)
	}
	if fi, _ := os.Stat(p); fi.Size() != want {
		t.Fatalf("sparse file not truncated: %d", fi.Size())
	}

	// pre-allocated and complete → trailer dropped
	p = filepath.Join(dir, "complete")
	data = make([]byte, size+3+sparseFooterSize)
	copy(data[size:], []byte{0xFF, 0xFF, 0xFF})
	writeFile(t, p, data)
	if n, _ := existingBytes(p, size); n != size {
		t.Fatalf("complete: %d", n)
	}
}

// fileServer serves body; ignoreRange makes it answer 200 to Range requests,
// truncate cuts the body short.
func fileServer(t *testing.T, body []byte, ignoreRange bool, truncate int) (*httptest.Server, *[]string) {
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/file", http.StatusFound)
			return
		}
		b, status, start := body, http.StatusOK, 0
		if rg := r.Header.Get("Range"); rg != "" && !ignoreRange {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			b, status = body[start:], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(status)
		if truncate > 0 {
			b = b[:truncate]
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &ranges
}

func TestDownloadResume(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)
	for _, tc := range []struct {
		name        string
		ignoreRange bool
		path        string
	}{
		{"206", false, "/file"},
		{"server ignores Range", true, "/file"},
		{"redirect keeps Range", false, "/redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ranges := fileServer(t, body, tc.ignoreRange, 0)
			dest := filepath.Join(t.TempDir(), "f")
			writeFile(t, dest, body[:4000])
			err := downloadFile(context.Background(), srv.URL+tc.path, dest, 4000, int64(len(body)), newProgress(0, 0))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(dest)
			if !bytes.Equal(got, body) {
				t.Fatalf("content mismatch: len %d", len(got))
			}
			for _, rg := range *ranges {
				if rg != "bytes=4000-" {
					t.Fatalf("Range lost on a hop: %q", *ranges)
				}
			}
		})
	}
}

func TestDownloadIncomplete(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 5000)
	srv, _ := fileServer(t, body, false, 0)
	dest := filepath.Join(t.TempDir(), "f")
	err := downloadFile(context.Background(), srv.URL+"/file", dest, 0, 6000, newProgress(0, 0))
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("want incomplete error, got %v", err)
	}
}

func TestFailoverKeepsRange(t *testing.T) {
	body := bytes.Repeat([]byte("abc"), 1000)
	srv, ranges := fileServer(t, body, false, 0)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	appCfg.AppSettings.DownloadSettings.DNSFailoverSettings = map[string]string{"cdn.invalid": "127.0.0.1"}
	t.Cleanup(func() { appCfg.AppSettings.DownloadSettings.DNSFailoverSettings = nil })

	dest := filepath.Join(t.TempDir(), "f")
	writeFile(t, dest, body[:100])
	err := downloadFile(context.Background(), "https://cdn.invalid:"+port+"/file", dest, 100, int64(len(body)), newProgress(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, body) {
		t.Fatal("content mismatch")
	}
	if len(*ranges) != 1 || (*ranges)[0] != "bytes=100-" {
		t.Fatalf("ranges: %q", *ranges)
	}
}

func TestFailoverURL(t *testing.T) {
	appCfg.AppSettings.DownloadSettings.DNSFailoverSettings = map[string]string{"cdn.twinstar-wow.com": "176.9.18.199"}
	t.Cleanup(func() { appCfg.AppSettings.DownloadSettings.DNSFailoverSettings = nil })
	got, ok := failoverURL("https://cdn.twinstar-wow.com/15050.direct/Data/a.MPQ")
	if !ok || got != "http://176.9.18.199/15050.direct/Data/a.MPQ" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := failoverURL("https://evilcdn.twinstar-wow.com.example/x"); ok {
		t.Fatal("substring host matched")
	}
}

func TestPatchConfig(t *testing.T) {
	in := "SET readTOS \"1\"\r\nSET realmlist \"old\"\r\nSET locale \"enUS\"\r\n"
	got := patchConfig(in, "login.example", "deDE")
	want := "SET readTOS \"1\"\r\nSET realmlist \"login.example\"\r\nSET locale \"deDE\"\r\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
	got = patchConfig("SET gxApi \"d3d9\"\n", "", "frFR")
	if got != "SET gxApi \"d3d9\"\r\nSET locale \"frFR\"\r\n" {
		t.Fatalf("append: %q", got)
	}
}

func TestFindXMLElement(t *testing.T) {
	cfg := []byte(`<?xml version="1.0"?><config><versioninfo><version><servers>
		<server id="other" url="http://a/"/><server url="http://cdn/x/" id="TwinWind"/>
		</servers></version></versioninfo></config>`)
	el, ok, err := findXMLElement(cfg, "server", func(a map[string]string) bool { return strings.EqualFold(a["id"], "twinwind") })
	if err != nil || !ok || el.attrs["url"] != "http://cdn/x/" {
		t.Fatalf("got %+v %v %v", el, ok, err)
	}
	patch := []byte(`<patch><record program="WoW" component="enUS">http://cfg/;h1;h2;18414</record></patch>`)
	el, ok, _ = findXMLElement(patch, "record", func(a map[string]string) bool { return a["component"] == "enUS" })
	if !ok || el.text != "http://cfg/;h1;h2;18414" {
		t.Fatalf("got %+v", el)
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "z.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	f.Close()
	return p
}

func TestExtractZip(t *testing.T) {
	dest := t.TempDir()
	z := makeZip(t, map[string]string{"WoW_Vanilla/WoW.exe": "exe", "WoW_Vanilla/Data/a.MPQ": "mpq"})
	if err := extractZip(z, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "Data", "a.MPQ")); string(b) != "mpq" {
		t.Fatal("top-level folder not stripped")
	}

	z = makeZip(t, map[string]string{"../evil.exe": "x", "ok.txt": "y"})
	if err := extractZip(z, t.TempDir()); err == nil {
		t.Fatal("zip slip not rejected")
	}
}

func TestCheckClientVersion(t *testing.T) {
	// Any system executable with a version resource stands in for Wow.exe
	// (Wine's notepad.exe has none, its winver.exe/explorer.exe do).
	var src, ver string
	for _, name := range []string{"notepad.exe", "winver.exe", "explorer.exe", "regsvr32.exe"} {
		p := filepath.Join(os.Getenv("WINDIR"), "System32", name)
		if v, err := fileVersion(p); err == nil {
			src, ver = p, v
			break
		}
	}
	if src == "" {
		t.Skip("no system exe with version info found")
	}
	t.Logf("using %s (version %s)", src, ver)
	dir := t.TempDir()
	if err := checkClientVersion(dir, "Mop"); err != nil {
		t.Fatalf("empty folder rejected: %v", err)
	}
	data, _ := os.ReadFile(src)
	writeFile(t, filepath.Join(dir, "Wow.exe"), data)

	orig := expectedClientVersion["Mop"]
	t.Cleanup(func() { expectedClientVersion["Mop"] = orig })

	expectedClientVersion["Mop"] = ver
	if err := checkClientVersion(dir, "Mop"); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
	expectedClientVersion["Mop"] = "5.4.8"
	if err := checkClientVersion(dir, "Mop"); err == nil || !strings.Contains(err.Error(), ver) {
		t.Fatalf("wrong version accepted (exe is %s): %v", ver, err)
	}

	// An exe without a version resource is rejected, like the original.
	self, _ := os.Executable()
	data, _ = os.ReadFile(self)
	writeFile(t, filepath.Join(dir, "Wow.exe"), data)
	if err := checkClientVersion(dir, "Mop"); err == nil {
		t.Fatal("exe without version info accepted")
	}
}
