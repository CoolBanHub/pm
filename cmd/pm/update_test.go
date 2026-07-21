package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfUpdaterSkipsCurrentVersion(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/latest" {
			t.Fatalf("unexpected asset request: %s", request.URL.Path)
		}
		_ = json.NewEncoder(writer).Encode(githubRelease{TagName: "v1.2.3"})
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "pm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	updated, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.2.3",
		goos: "linux", goarch: "amd64", executable: target, output: &output,
	}).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if updated || requests != 1 || !strings.Contains(output.String(), "already up to date") {
		t.Fatalf("updated=%v requests=%d output=%q", updated, requests, output.String())
	}
}

func TestUpdateCommandRejectsArguments(t *testing.T) {
	if err := updateCommand([]string{"unexpected"}); err == nil {
		t.Fatal("expected update argument error")
	}
}

func TestSelfUpdaterDownloadsVerifiesAndReplaces(t *testing.T) {
	binary := []byte("new-pm-binary")
	server := updateTestServer(t, "v2.0.0", binary, checksumLine(binary, "pm-darwin-arm64"))
	defer server.Close()
	directory := t.TempDir()
	target := filepath.Join(directory, "pm")
	if err := os.WriteFile(target, []byte("old-pm-binary"), 0o751); err != nil {
		t.Fatal(err)
	}
	validated := false
	var output bytes.Buffer
	updated, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.0.0",
		goos: "darwin", goarch: "arm64", executable: target, output: &output,
		validate: func(path, expectedVersion string) error {
			validated = true
			if expectedVersion != "v2.0.0" {
				return fmt.Errorf("version = %s", expectedVersion)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(data, binary) {
				return errors.New("unexpected binary content")
			}
			return nil
		},
	}).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !updated || !validated || !bytes.Equal(data, binary) || info.Mode().Perm() != 0o751 {
		t.Fatalf("updated=%v validated=%v data=%q mode=%o", updated, validated, data, info.Mode().Perm())
	}
	if !strings.Contains(output.String(), "updated pm from v1.0.0 to v2.0.0") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestSelfUpdaterPrefersExecutableArchive(t *testing.T) {
	binary := []byte("archived-pm-binary")
	archive := updateArchive(t, "pm", 0o755, binary)
	digest := sha256.Sum256(archive)
	archiveRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_ = json.NewEncoder(writer).Encode(githubRelease{TagName: "v2.0.0", Assets: []releaseAsset{
				{Name: "pm-darwin-arm64.tar.gz", BrowserDownloadURL: server.URL + "/archive", Size: int64(len(archive)), Digest: "sha256:" + hex.EncodeToString(digest[:])},
				{Name: "pm-darwin-arm64", BrowserDownloadURL: server.URL + "/legacy", Size: int64(len(binary))},
			}})
		case "/archive":
			archiveRequests++
			_, _ = writer.Write(archive)
		case "/legacy":
			t.Error("legacy binary was downloaded despite archive availability")
			http.Error(writer, "unexpected legacy request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "pm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updated, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.0.0",
		goos: "darwin", goarch: "arm64", executable: target, output: io.Discard,
		validate: func(path, expectedVersion string) error {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(data, binary) || expectedVersion != "v2.0.0" {
				return errors.New("unexpected extracted update")
			}
			return nil
		},
	}).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if !updated || archiveRequests != 1 || !bytes.Equal(data, binary) {
		t.Fatalf("updated=%v archive requests=%d data=%q", updated, archiveRequests, data)
	}
}

func TestExtractUpdateArchiveRequiresSafeExecutable(t *testing.T) {
	for name, archive := range map[string][]byte{
		"path traversal": updateArchive(t, "../pm", 0o755, []byte("binary")),
		"not executable": updateArchive(t, "pm", 0o644, []byte("binary")),
	} {
		t.Run(name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "pm.tar.gz")
			if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			destination, err := os.CreateTemp(t.TempDir(), "pm")
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			if err := extractUpdateArchive(archivePath, destination); err == nil {
				t.Fatal("expected unsafe archive rejection")
			}
		})
	}
}

func TestSelfUpdaterLeavesExecutableOnChecksumFailure(t *testing.T) {
	binary := []byte("new-pm-binary")
	wrong := []byte("different-binary")
	server := updateTestServer(t, "v2.0.0", binary, checksumLine(wrong, "pm-linux-amd64"))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "pm")
	original := []byte("old-pm-binary")
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.0.0",
		goos: "linux", goarch: "amd64", executable: target, output: io.Discard,
		validate: func(string, string) error { return nil },
	}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %v", err)
	}
	data, _ := os.ReadFile(target)
	if !bytes.Equal(data, original) {
		t.Fatalf("executable changed after checksum failure: %q", data)
	}
}

func TestSelfUpdaterLeavesExecutableOnValidationFailure(t *testing.T) {
	binary := []byte("new-pm-binary")
	server := updateTestServer(t, "v2.0.0", binary, checksumLine(binary, "pm-linux-arm64"))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "pm")
	original := []byte("old-pm-binary")
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.0.0",
		goos: "linux", goarch: "arm64", executable: target, output: io.Discard,
		validate: func(string, string) error { return errors.New("wrong version") },
	}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "validate update") {
		t.Fatalf("error = %v", err)
	}
	data, _ := os.ReadFile(target)
	if !bytes.Equal(data, original) {
		t.Fatalf("executable changed after validation failure: %q", data)
	}
}

func TestSelfUpdaterResumesInterruptedBinaryDownload(t *testing.T) {
	binary := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	half := len(binary) / 2
	binaryRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			_ = json.NewEncoder(writer).Encode(githubRelease{TagName: "v2.0.0", Assets: []releaseAsset{
				{Name: "pm-linux-amd64", BrowserDownloadURL: server.URL + "/binary", Size: int64(len(binary))},
				{Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/checksums"},
			}})
		case "/checksums":
			_, _ = writer.Write([]byte(checksumLine(binary, "pm-linux-amd64")))
		case "/binary":
			binaryRequests++
			if binaryRequests == 1 {
				writer.Header().Set("Content-Length", fmt.Sprint(len(binary)))
				_, _ = writer.Write(binary[:half])
				return
			}
			if request.Header.Get("Range") != fmt.Sprintf("bytes=%d-", half) {
				t.Errorf("Range = %q", request.Header.Get("Range"))
			}
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(binary)-1, len(binary)))
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = writer.Write(binary[half:])
		}
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "pm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updated, err := (selfUpdater{
		client: server.Client(), latestURL: server.URL + "/latest", currentVersion: "v1.0.0",
		goos: "linux", goarch: "amd64", executable: target, output: io.Discard,
		validate: func(string, string) error { return nil },
	}).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if !updated || binaryRequests != 2 || !bytes.Equal(data, binary) {
		t.Fatalf("updated=%v requests=%d data=%q", updated, binaryRequests, data)
	}
}

func TestValidateDownloadedBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'v3.1.4\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateDownloadedBinary(path, "v3.1.4"); err != nil {
		t.Fatal(err)
	}
	if err := validateDownloadedBinary(path, "v3.1.5"); err == nil {
		t.Fatal("expected version mismatch")
	}
}

func TestChecksumForAssetRejectsDuplicates(t *testing.T) {
	line := checksumLine([]byte("binary"), "pm-linux-amd64")
	if _, err := checksumForAsset([]byte(line+line), "pm-linux-amd64"); err == nil {
		t.Fatal("expected duplicate checksum error")
	}
}

func TestChecksumFromDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("binary"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	parsed, found, err := checksumFromDigest(digest)
	if err != nil || !found || parsed != sum {
		t.Fatalf("parsed=%x found=%v err=%v", parsed, found, err)
	}
	if _, found, err := checksumFromDigest(""); err != nil || found {
		t.Fatalf("empty digest found=%v err=%v", found, err)
	}
	if _, _, err := checksumFromDigest("md5:abcd"); err == nil {
		t.Fatal("expected unsupported digest error")
	}
}

func updateTestServer(t *testing.T, tag string, binary []byte, checksums string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest":
			assets := []releaseAsset{
				{Name: "pm-darwin-arm64", BrowserDownloadURL: server.URL + "/binary", Size: int64(len(binary))},
				{Name: "pm-linux-amd64", BrowserDownloadURL: server.URL + "/binary", Size: int64(len(binary))},
				{Name: "pm-linux-arm64", BrowserDownloadURL: server.URL + "/binary", Size: int64(len(binary))},
				{Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/checksums"},
			}
			_ = json.NewEncoder(writer).Encode(githubRelease{TagName: tag, Assets: assets})
		case "/binary":
			_, _ = writer.Write(binary)
		case "/checksums":
			_, _ = writer.Write([]byte(checksums))
		default:
			http.NotFound(writer, request)
		}
	}))
	return server
}

func checksumLine(data []byte, name string) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func updateArchive(t *testing.T, name string, mode int64, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
