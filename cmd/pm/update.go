package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	latestReleaseURL = "https://api.github.com/repos/CoolBanHub/pm/releases/latest"
	maximumMetadata  = 2 << 20
	maximumChecksums = 1 << 20
	maximumBinary    = 128 << 20
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type selfUpdater struct {
	client         *http.Client
	latestURL      string
	currentVersion string
	goos           string
	goarch         string
	executable     string
	output         io.Writer
	validate       func(path, expectedVersion string) error
}

func updateCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("update does not accept arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	updater := selfUpdater{
		client:         &http.Client{Timeout: 10 * time.Minute},
		latestURL:      latestReleaseURL,
		currentVersion: version,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		executable:     executable,
		output:         os.Stdout,
		validate:       validateDownloadedBinary,
	}
	_, err = updater.run(ctx)
	return err
}

func (u selfUpdater) run(ctx context.Context) (bool, error) {
	if u.client == nil || u.latestURL == "" || u.executable == "" {
		return false, errors.New("update configuration is incomplete")
	}
	if u.output == nil {
		u.output = io.Discard
	}
	releaseData, err := u.fetch(ctx, u.latestURL, maximumMetadata)
	if err != nil {
		return false, fmt.Errorf("query latest release: %w", err)
	}
	var release githubRelease
	if err := json.Unmarshal(releaseData, &release); err != nil {
		return false, fmt.Errorf("decode latest release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return false, errors.New("latest release does not contain a tag")
	}
	if u.currentVersion == release.TagName {
		fmt.Fprintf(u.output, "pm %s is already up to date\n", u.currentVersion)
		return false, nil
	}

	assetName := fmt.Sprintf("pm-%s-%s", u.goos, u.goarch)
	binaryAsset, found := findReleaseAsset(release.Assets, assetName)
	if !found {
		return false, fmt.Errorf("release %s does not provide %s", release.TagName, assetName)
	}
	expectedChecksum, hasDigest, err := checksumFromDigest(binaryAsset.Digest)
	if err != nil {
		return false, fmt.Errorf("release %s has an invalid digest for %s: %w", release.TagName, assetName, err)
	}
	if !hasDigest {
		checksumAsset, found := findReleaseAsset(release.Assets, "SHA256SUMS")
		if !found {
			return false, fmt.Errorf("release %s does not provide a digest or SHA256SUMS for %s", release.TagName, assetName)
		}
		checksumData, err := u.fetch(ctx, checksumAsset.BrowserDownloadURL, maximumChecksums)
		if err != nil {
			return false, fmt.Errorf("download SHA256SUMS: %w", err)
		}
		expectedChecksum, err = checksumForAsset(checksumData, assetName)
		if err != nil {
			return false, err
		}
	}
	if err := u.replaceExecutable(ctx, binaryAsset.BrowserDownloadURL, binaryAsset.Size, expectedChecksum, release.TagName); err != nil {
		return false, err
	}
	fmt.Fprintf(u.output, "updated pm from %s to %s\n", u.currentVersion, release.TagName)
	return true, nil
}

func checksumFromDigest(digest string) ([sha256.Size]byte, bool, error) {
	var checksum [sha256.Size]byte
	if strings.TrimSpace(digest) == "" {
		return checksum, false, nil
	}
	algorithm, encoded, found := strings.Cut(digest, ":")
	if !found || algorithm != "sha256" {
		return checksum, false, errors.New("digest must use sha256")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return checksum, false, errors.New("digest contains an invalid sha256 value")
	}
	copy(checksum[:], decoded)
	return checksum, true, nil
}

func findReleaseAsset(assets []releaseAsset, name string) (releaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name && asset.BrowserDownloadURL != "" {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

func checksumForAsset(data []byte, assetName string) ([sha256.Size]byte, error) {
	var checksum [sha256.Size]byte
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		if found {
			return checksum, fmt.Errorf("SHA256SUMS contains duplicate entries for %s", assetName)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return checksum, fmt.Errorf("SHA256SUMS contains an invalid checksum for %s", assetName)
		}
		copy(checksum[:], decoded)
		found = true
	}
	if err := scanner.Err(); err != nil {
		return checksum, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if !found {
		return checksum, fmt.Errorf("SHA256SUMS does not contain %s", assetName)
	}
	return checksum, nil
}

func (u selfUpdater) fetch(ctx context.Context, url string, maximum int64) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		data, err := u.fetchOnce(ctx, url, maximum)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !shouldRetryDownload(ctx, err) || attempt == 3 {
			break
		}
		fmt.Fprintf(u.output, "download interrupted; retrying (%d/3)\n", attempt+1)
		if err := waitForRetry(ctx, time.Duration(attempt)*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (u selfUpdater) fetchOnce(ctx context.Context, url string, maximum int64) ([]byte, error) {
	response, err := u.request(ctx, url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, httpStatusError(response)
	}
	if response.ContentLength > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}

func (u selfUpdater) replaceExecutable(ctx context.Context, url string, expectedSize int64, expected [sha256.Size]byte, expectedVersion string) error {
	info, err := os.Stat(u.executable)
	if err != nil {
		return fmt.Errorf("inspect current executable: %w", err)
	}
	directory := filepath.Dir(u.executable)
	temporary, err := os.CreateTemp(directory, ".pm-update-*")
	if err != nil {
		return fmt.Errorf("create update beside %s: %w; rerun with sufficient permissions (for example sudo pm update)", u.executable, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	actual, err := u.downloadBinary(ctx, temporary, url, expectedSize)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("download update: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync update: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close update: %w", err)
	}
	if !equalChecksum(actual, expected[:]) {
		return errors.New("downloaded binary checksum does not match SHA256SUMS")
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		mode = 0o755
	}
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return fmt.Errorf("make update executable: %w", err)
	}
	validate := u.validate
	if validate == nil {
		validate = validateDownloadedBinary
	}
	if err := validate(temporaryPath, expectedVersion); err != nil {
		return fmt.Errorf("validate update: %w", err)
	}
	if err := os.Rename(temporaryPath, u.executable); err != nil {
		return fmt.Errorf("replace %s: %w; rerun with sufficient permissions (for example sudo pm update)", u.executable, err)
	}
	if handle, openErr := os.Open(directory); openErr == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

func (u selfUpdater) downloadBinary(ctx context.Context, destination *os.File, url string, expectedSize int64) ([]byte, error) {
	if expectedSize < 0 {
		return nil, errors.New("release asset has a negative size")
	}
	if expectedSize > maximumBinary {
		return nil, fmt.Errorf("update binary exceeds %d bytes", maximumBinary)
	}
	hash := sha256.New()
	offset := int64(0)
	total := expectedSize
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		response, err := u.requestRange(ctx, url, offset)
		if err != nil {
			lastErr = err
		} else {
			if offset > 0 && response.StatusCode == http.StatusOK {
				if resetErr := resetUpdateDownload(destination, hash); resetErr != nil {
					response.Body.Close()
					return nil, resetErr
				}
				offset = 0
			}
			validStatus := response.StatusCode == http.StatusOK || response.StatusCode == http.StatusPartialContent
			if !validStatus {
				lastErr = httpStatusError(response)
				response.Body.Close()
			} else {
				if response.StatusCode == http.StatusPartialContent {
					start, responseTotal, rangeErr := parseContentRange(response.Header.Get("Content-Range"))
					if rangeErr != nil || start != offset {
						response.Body.Close()
						return nil, fmt.Errorf("invalid resumed response: %s", response.Header.Get("Content-Range"))
					}
					if total == 0 {
						total = responseTotal
					} else if responseTotal > 0 && total != responseTotal {
						response.Body.Close()
						return nil, fmt.Errorf("release asset size changed from %d to %d", total, responseTotal)
					}
				} else if total == 0 && response.ContentLength >= 0 {
					total = response.ContentLength
				}
				if total > maximumBinary {
					response.Body.Close()
					return nil, fmt.Errorf("update binary exceeds %d bytes", maximumBinary)
				}
				progress := newDownloadProgress(u.output, total, offset)
				remainingLimit := maximumBinary - offset + 1
				written, copyErr := io.Copy(io.MultiWriter(destination, hash, progress), io.LimitReader(response.Body, remainingLimit))
				progress.finish()
				response.Body.Close()
				offset += written
				if offset > maximumBinary {
					return nil, fmt.Errorf("update binary exceeds %d bytes", maximumBinary)
				}
				if copyErr == nil && total > 0 && offset < total {
					copyErr = io.ErrUnexpectedEOF
				}
				if copyErr == nil {
					if total > 0 && offset != total {
						return nil, fmt.Errorf("downloaded %d bytes, expected %d", offset, total)
					}
					return hash.Sum(nil), nil
				}
				lastErr = copyErr
			}
		}
		if !shouldRetryDownload(ctx, lastErr) || attempt == 5 {
			break
		}
		action := "resuming"
		if offset == 0 {
			action = "retrying"
		}
		fmt.Fprintf(u.output, "download interrupted at %d bytes; %s (%d/5)\n", offset, action, attempt+1)
		if err := waitForRetry(ctx, time.Duration(attempt)*time.Second); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func resetUpdateDownload(destination *os.File, hash io.Writer) error {
	resetter, ok := hash.(interface{ Reset() })
	if !ok {
		return errors.New("cannot reset update checksum")
	}
	if err := destination.Truncate(0); err != nil {
		return err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return err
	}
	resetter.Reset()
	return nil
}

func parseContentRange(value string) (start, total int64, err error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "bytes ")
	rangePart, totalPart, found := strings.Cut(value, "/")
	if !found {
		return 0, 0, errors.New("missing total")
	}
	startPart, _, found := strings.Cut(rangePart, "-")
	if !found {
		return 0, 0, errors.New("missing range")
	}
	start, err = strconv.ParseInt(startPart, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if totalPart != "*" {
		total, err = strconv.ParseInt(totalPart, 10, 64)
	}
	return start, total, err
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type httpResponseError struct {
	statusCode int
	message    string
}

func (e *httpResponseError) Error() string { return e.message }

func shouldRetryDownload(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var responseErr *httpResponseError
	if errors.As(err, &responseErr) {
		return responseErr.statusCode == http.StatusTooManyRequests || responseErr.statusCode >= 500
	}
	return true
}

type downloadProgress struct {
	output      io.Writer
	total       int64
	written     int64
	lastPercent int
	started     bool
}

func newDownloadProgress(output io.Writer, total, alreadyWritten int64) *downloadProgress {
	lastPercent := -10
	if total > 0 && alreadyWritten > 0 {
		lastPercent = int(alreadyWritten*100/total)/10*10 - 10
	}
	progress := &downloadProgress{output: output, total: total, written: alreadyWritten, lastPercent: lastPercent}
	if total <= 0 {
		fmt.Fprintln(output, "downloading update...")
		progress.started = true
	}
	return progress
}

func (p *downloadProgress) Write(data []byte) (int, error) {
	p.written += int64(len(data))
	if p.total > 0 {
		percent := int(p.written * 100 / p.total)
		if percent > 100 {
			percent = 100
		}
		if percent >= p.lastPercent+10 || percent == 100 {
			fmt.Fprintf(p.output, "\rdownloading update: %3d%%", percent)
			p.lastPercent = percent
			p.started = true
		}
	}
	return len(data), nil
}

func (p *downloadProgress) finish() {
	if p.started {
		fmt.Fprintln(p.output)
	}
}

func (u selfUpdater) request(ctx context.Context, url string) (*http.Response, error) {
	return u.requestRange(ctx, url, 0)
}

func (u selfUpdater) requestRange(ctx context.Context, url string, offset int64) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pm/"+u.currentVersion)
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return u.client.Do(request)
}

func httpStatusError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	detail := strings.TrimSpace(string(message))
	if detail == "" {
		return &httpResponseError{statusCode: response.StatusCode, message: fmt.Sprintf("HTTP %s", response.Status)}
	}
	return &httpResponseError{statusCode: response.StatusCode, message: fmt.Sprintf("HTTP %s: %s", response.Status, detail)}
}

func equalChecksum(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}

func validateDownloadedBinary(path, expectedVersion string) error {
	command := exec.Command(path, "version")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run downloaded binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	actualVersion := strings.TrimSpace(string(output))
	if actualVersion != expectedVersion {
		return fmt.Errorf("downloaded binary reports version %q, expected %q", actualVersion, expectedVersion)
	}
	return nil
}
