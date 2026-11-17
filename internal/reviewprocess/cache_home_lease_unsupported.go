//go:build !linux

package reviewprocess

import (
	"errors"
	"os"
)

type CacheHomeLease struct{ home string }

func NewCacheHomeLease(string) (*CacheHomeLease, error) {
	return nil, errors.New("cache HOME lease unsupported")
}
func (*CacheHomeLease) Close() error { return nil }
func (*CacheHomeLease) acquire(bool) (*os.File, error) {
	return nil, errors.New("cache HOME lease unsupported")
}
