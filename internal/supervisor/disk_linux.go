//go:build linux

package supervisor

import "syscall"

// diskTotalBytes sums the total capacity of the filesystems backing the given
// paths, deduplicated by device id so a single disk shared by several paths is
// counted once.
func diskTotalBytes(paths []string) int64 {
	var total int64
	seen := make(map[uint64]struct{})
	for _, p := range paths {
		var stat syscall.Stat_t
		if syscall.Stat(p, &stat) != nil {
			continue
		}
		if _, ok := seen[stat.Dev]; ok {
			continue
		}
		seen[stat.Dev] = struct{}{}
		var fs syscall.Statfs_t
		if syscall.Statfs(p, &fs) != nil {
			continue
		}
		total += int64(fs.Blocks) * int64(fs.Bsize)
	}
	return total
}
