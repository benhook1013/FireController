//go:build !linux

package capturecustody

import "os"

func owned(os.FileInfo) bool            { return false }
func openLock(string) (*os.File, error) { return nil, ErrUnsupported }
func tryLock(*os.File, bool) error      { return ErrUnsupported }
