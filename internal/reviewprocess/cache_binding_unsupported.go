//go:build !linux

package reviewprocess

import (
	"context"
	"errors"
)

type CacheMaintenanceBinding struct{ home string }

func NewCacheMaintenanceBinding(string, string, string, string) (*CacheMaintenanceBinding, error) {
	return nil, errors.New("cache generation binding unsupported")
}
func (*CacheMaintenanceBinding) Check(context.Context) error {
	return errors.New("cache generation binding unsupported")
}
