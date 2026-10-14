//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package weixin

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockWeixinConfigFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
