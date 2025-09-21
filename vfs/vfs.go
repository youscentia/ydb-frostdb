package vfs

import (
	"io"
	"os"
)

type FileSystem interface {
	OpenFile(name string, flag int, perm os.FileMode) (File, error)
	Stat(name string) (os.FileInfo, error)
	MkdirAll(path string, perm os.FileMode) error
	RemoveAll(path string) error
	Rename(oldpath, newpath string) error
	ReadDir(name string) ([]os.DirEntry, error)
}

type File interface {
	io.ReadWriteCloser
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	Name() string
}

// type OSFileSystem struct{}

// func (fs *OSFileSystem) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
// 	f, err := os.OpenFile(name, flag, perm)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return &OSFile{f}, nil
// }

// func (fs *OSFileSystem) Stat(name string) (os.FileInfo, error) {
// 	return os.Stat(name)
// }

// func (fs *OSFileSystem) MkdirAll(path string, perm os.FileMode) error {
// 	return os.MkdirAll(path, perm)
// }

// func (fs *OSFileSystem) RemoveAll(path string) error {
// 	return os.RemoveAll(path)
// }

// func (fs *OSFileSystem) Rename(oldpath, newpath string) error {
// 	return os.Rename(oldpath, newpath)
// }

// func (fs *OSFileSystem) ReadDir(name string) ([]os.DirEntry, error) {
// 	return os.ReadDir(name)
// }

// type OSFile struct {
// 	*os.File
// }

// func (f *OSFile) Truncate(size int64) error {
// 	return f.File.Truncate(size)
// }
