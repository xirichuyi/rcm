package mirror

import "golang.org/x/sys/unix"

func prepareWatchLimits() {
	var limit unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &limit) != nil {
		return
	}
	target := uint64(65536)
	if ceiling, err := unix.SysctlUint32("kern.maxfilesperproc"); err == nil && uint64(ceiling) < target {
		target = uint64(ceiling)
	}
	if limit.Max < target {
		target = limit.Max
	}
	if limit.Cur < target {
		limit.Cur = target
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &limit)
	}
}
