package adapters

import (
	"os"

	"github.com/youscentia/ydb-frostdb/vfs"
)

type OSAdapter struct{}

func NewOSAdapter() *OSAdapter {
	return &OSAdapter{}
}

func (a *OSAdapter) OpenFile(name string, flag int, perm os.FileMode) (vfs.File, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &OSFile{f}, nil
}

func (a *OSAdapter) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (a *OSAdapter) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (a *OSAdapter) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (a *OSAdapter) Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func (a *OSAdapter) ReadDir(name string) ([]os.DirEntry, error) {
	return os.ReadDir(name)
}

type OSFile struct {
	*os.File
}

func (f *OSFile) Truncate(size int64) error {
	return f.File.Truncate(size)
}
