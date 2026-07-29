package supervisor

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // register the "sqlite" driver (pure Go, no cgo)
)

const (
	// diskUnknown marks a directory whose size has not been measured yet or that
	// could not be read. Status.DiskUsage mirrors it; the UI renders "-".
	diskUnknown int64 = -1
	// forceDeepScans forces a full re-walk every N intervals even when the
	// directory fingerprint is unchanged, so an in-place file content change
	// (which does not update a parent directory's mtime) is eventually caught.
	forceDeepScans = 6
)

var diskSchema = `
CREATE TABLE IF NOT EXISTS dir_snapshot (
  path        TEXT PRIMARY KEY,
  total_size  INTEGER NOT NULL,
  fingerprint TEXT NOT NULL,
  scanned_at  INTEGER NOT NULL
);`

type dirSnapshot struct {
	total       int64
	fingerprint string
	scannedAt   int64
}

// DiskMonitor tracks the on-disk size of each managed program's working
// directory. Sizes are recomputed by a background goroutine on a slow interval
// (default 5 minutes) and cached, so the 2-second status poll only reads an
// in-memory map. A directory mtime fingerprint lets unchanged subtrees be
// skipped entirely; snapshots are persisted to SQLite so a daemon restart does
// not force a full re-walk.
type DiskMonitor struct {
	db        *sql.DB
	mu        sync.Mutex
	total     map[string]int64 // directory -> bytes (diskUnknown = unmeasured)
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
	walkCount int64 // number of full size walks performed (for tests/observability)
}

// NewDiskMonitor opens (or creates) the snapshot database under stateDir and
// starts the background scan loop. A scan runs immediately so the first poll
// after a cold start gets real values quickly.
func NewDiskMonitor(stateDir string, interval time.Duration) (*DiskMonitor, error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if stateDir == "" {
		stateDir = "."
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "disk.db"))
	if err != nil {
		return nil, fmt.Errorf("open disk db: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite serializes writes through one connection
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set wal mode: %w", err)
	}
	if _, err := db.Exec(diskSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init disk schema: %w", err)
	}
	d := &DiskMonitor{
		db:       db,
		total:    make(map[string]int64),
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	d.loadSnapshots()
	go d.loop()
	return d, nil
}

// Stop halts the background scan loop and closes the database.
func (d *DiskMonitor) Stop() {
	select {
	case <-d.stop: // already stopped
		return
	default:
		close(d.stop)
	}
	<-d.done
	_ = d.db.Close()
}

func (d *DiskMonitor) loadSnapshots() {
	rows, err := d.db.Query("SELECT path, total_size FROM dir_snapshot")
	if err != nil {
		return
	}
	defer rows.Close()
	d.mu.Lock()
	for rows.Next() {
		var path string
		var size int64
		if rows.Scan(&path, &size) == nil {
			d.total[path] = size
		}
	}
	d.mu.Unlock()
}

func (d *DiskMonitor) loop() {
	defer close(d.done)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	d.scanAll() // initial scan so a cold start is not stuck on "-" for one interval
	for {
		select {
		case <-ticker.C:
			d.scanAll()
		case <-d.stop:
			return
		}
	}
}

// Apply registers each status's working directory and fills its DiskUsage from
// the cache. It only touches memory, so it is safe to call on every status
// poll. Unknown / not-yet-scanned directories read as diskUnknown.
//
// A newly seen directory is scanned immediately in the background; otherwise a
// fresh daemon (whose initial background sweep ran before any directory was
// registered) would show "-" until the next ticker tick minutes later.
func (d *DiskMonitor) Apply(statuses []Status) {
	d.mu.Lock()
	var fresh []string
	for i := range statuses {
		dir := statuses[i].Directory
		if dir == "" {
			statuses[i].DiskUsage = diskUnknown
			continue
		}
		if _, ok := d.total[dir]; !ok {
			d.total[dir] = diskUnknown
			fresh = append(fresh, dir)
		}
		statuses[i].DiskUsage = d.total[dir]
	}
	d.mu.Unlock()
	for _, dir := range fresh {
		dir := dir
		go d.scanOne(dir)
	}
}

func (d *DiskMonitor) scanAll() {
	d.mu.Lock()
	dirs := make([]string, 0, len(d.total))
	for dir := range d.total {
		dirs = append(dirs, dir)
	}
	d.mu.Unlock()
	for _, dir := range dirs {
		d.scanOne(dir)
	}
}

func (d *DiskMonitor) scanOne(dir string) {
	now := time.Now()
	fingerprint, err := collectDirFingerprint(dir)
	if err != nil {
		d.setAndPersist(dir, diskUnknown, "", now)
		return
	}
	snap := d.readSnapshot(dir)
	force := snap == nil || now.Sub(time.Unix(snap.scannedAt, 0)) >= forceDeepScans*d.interval
	// Prune: the set of files/dirs is unchanged and we are not due for a forced
	// deep scan, so the cached total is still valid.
	if snap != nil && snap.fingerprint == fingerprint && !force {
		d.setTotal(dir, snap.total)
		return
	}
	atomic.AddInt64(&d.walkCount, 1)
	total, err := walkTotalSize(dir)
	if err != nil {
		d.setAndPersist(dir, diskUnknown, "", now)
		return
	}
	d.setAndPersist(dir, total, fingerprint, now)
}

func (d *DiskMonitor) setTotal(dir string, size int64) {
	d.mu.Lock()
	d.total[dir] = size
	d.mu.Unlock()
}

func (d *DiskMonitor) setAndPersist(dir string, size int64, fingerprint string, at time.Time) {
	d.setTotal(dir, size)
	_, _ = d.db.Exec(
		"INSERT INTO dir_snapshot(path, total_size, fingerprint, scanned_at) VALUES(?,?,?,?) "+
			"ON CONFLICT(path) DO UPDATE SET total_size=excluded.total_size, fingerprint=excluded.fingerprint, scanned_at=excluded.scanned_at",
		dir, size, fingerprint, at.Unix(),
	)
}

func (d *DiskMonitor) readSnapshot(dir string) *dirSnapshot {
	var snap dirSnapshot
	err := d.db.QueryRow("SELECT total_size, fingerprint, scanned_at FROM dir_snapshot WHERE path=?", dir).
		Scan(&snap.total, &snap.fingerprint, &snap.scannedAt)
	if err != nil {
		return nil
	}
	return &snap
}

// DiskTotal returns the total capacity of the filesystem(s) backing the tracked
// working directories. Filesystems are deduplicated by device id so a single
// disk shared by several directories is counted once.
func (d *DiskMonitor) DiskTotal() int64 {
	d.mu.Lock()
	dirs := make([]string, 0, len(d.total))
	for dir := range d.total {
		dirs = append(dirs, dir)
	}
	d.mu.Unlock()
	return diskTotalBytes(dirs)
}

// collectDirFingerprint walks the tree collecting every directory's modification
// time into a stable string. It only stat()s directories, so it is far cheaper
// than a full size scan. A directory's mtime changes when entries are added or
// removed beneath it (e.g. a binary is replaced on deploy); an unchanged
// fingerprint means the file set is unchanged.
func collectDirFingerprint(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", root)
	}
	type node struct {
		rel   string
		mtime int64
	}
	var nodes []node
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable subtrees
		}
		if !entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		fi, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		nodes = append(nodes, node{rel: rel, mtime: fi.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].rel < nodes[j].rel })
	var sb strings.Builder
	for _, n := range nodes {
		sb.WriteString(n.rel)
		sb.WriteByte('|')
		sb.WriteString(strconv.FormatInt(n.mtime, 10))
		sb.WriteByte(';')
	}
	return sb.String(), nil
}

// walkTotalSize sums the sizes of all regular files under root. Symlinks and
// other non-regular files are ignored.
func walkTotalSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
