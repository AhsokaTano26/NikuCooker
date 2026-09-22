package models

import (
	"fmt"

	"github.com/shirou/gopsutil/v4/disk"
)

// freeSpace reports the bytes available on the filesystem holding path.
//
// gopsutil rather than a platform-specific call: the three implementations it
// wraps are each easy to get subtly wrong — `statfs` field units differ between
// Linux and macOS, and Windows needs GetDiskFreeSpaceEx — and a wrong answer
// here is a disk-full failure during a three-gigabyte download.
func freeSpace(path string) (uint64, error) {
	usage, err := disk.Usage(path)
	if err != nil {
		return 0, fmt.Errorf("models: check free space on %s: %w", path, err)
	}
	return usage.Free, nil
}
