package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/parquet-go/parquet-go"

	"github.com/youscentia/ydb-frostdb/dynparquet"
	"github.com/youscentia/ydb-frostdb/parts"
)

const (
	IndexFileExtension     = ".idx"
	ParquetCompactionTXKey = "compaction_tx"
	dirPerms               = os.FileMode(0o755)
	filePerms              = os.FileMode(0o640)
)

type Compaction func(w io.Writer, compact []parts.Part, options ...parquet.WriterOption) (int64, error)

type FileCompaction struct {
	// settings
	dir     string
	compact Compaction
	maxSize int64

	// internal data
	indexFiles []*os.File
	offset     int64          // Writing offsets into the file
	parts      sync.WaitGroup // Wait group for parts that are currently reference in this level.

	// Options
	logger log.Logger
}

func NewFileCompaction(dir string, maxSize int64, compact Compaction, logger log.Logger) (*FileCompaction, error) {
	f := &FileCompaction{
		dir:     dir,
		compact: compact,
		maxSize: maxSize,
		logger:  logger,
	}

	if err := os.MkdirAll(dir, dirPerms); err != nil {
		return nil, err
	}

	return f, nil
}

func (f *FileCompaction) MaxSize() int64 { return f.maxSize }

// Snapshot takes a snapshot of the current level. It ignores the parts and just hard links the files into the snapshot directory.
// It will rotate the active file if it has data in it rendering all snapshotted files as immutable.
func (f *FileCompaction) Snapshot(_ []parts.Part, _ func(parts.Part) error, dir string) error {
	if err := os.MkdirAll(dir, dirPerms); err != nil {
		return err
	}

	for i, file := range f.indexFiles {
		if i == len(f.indexFiles)-1 {
			// Sync the last file if it has data in it.
			if f.offset > 0 {
				if err := f.Sync(); err != nil {
					return err
				}
			} else {
				return nil // Skip empty file.
			}
		}

		// Hard link the file into the snapshot directory.
		if err := os.Link(file.Name(), filepath.Join(dir, filepath.Base(file.Name()))); err != nil {
			return err
		}
	}

	// Rotate the active file if it has data in it.
	_, err := f.createIndexFile(len(f.indexFiles))
	return err
}

func (f *FileCompaction) createIndexFile(id int) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(f.dir, fmt.Sprintf("%020d%s", id, IndexFileExtension)), os.O_CREATE|os.O_RDWR, filePerms)
	if err != nil {
		return nil, err
	}

	f.offset = 0
	f.indexFiles = append(f.indexFiles, file)
	return file, nil
}

func (f *FileCompaction) openIndexFile(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	f.indexFiles = append(f.indexFiles, file)
	return file, nil
}

// file returns the currently active index file.
func (f *FileCompaction) file() *os.File {
	return f.indexFiles[len(f.indexFiles)-1]
}

// accountingWriter is a writer that accounts for the number of bytes written.
type accountingWriter struct {
	w io.Writer
	n int64
}

func (a *accountingWriter) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	a.n += int64(n)
	return n, err
}

// Compact will compact the given parts into a Parquet file written to the next level file.
func (f *FileCompaction) Compact(compact []parts.Part, options ...parts.Option) ([]parts.Part, int64, int64, error) {
	if len(compact) == 0 {
		return nil, 0, 0, fmt.Errorf("no parts to compact")
	}

	accountant := &accountingWriter{w: f.file()}
	preCompactionSize, err := f.compact(accountant, compact,
		parquet.KeyValueMetadata(
			ParquetCompactionTXKey, // Compacting up through this transaction.
			fmt.Sprintf("%v", compact[0].TX()),
		),
	) // compact into the next level
	if err != nil {
		return nil, 0, 0, err
	}

	// Record the writing offset into the file.
	prevOffset := f.offset

	// Record the file size for recovery.
	size := make([]byte, 8)
	binary.LittleEndian.PutUint64(size, uint64(accountant.n))
	if n, err := f.file().Write(size); n != 8 {
		return nil, 0, 0, fmt.Errorf("failed to write size to file: %v", err)
	}
	f.offset += accountant.n + 8

	// Sync file after writing.
	if err := f.Sync(); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to sync file: %v", err)
	}

	pf, err := parquet.OpenFile(io.NewSectionReader(f.file(), prevOffset, accountant.n), accountant.n)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to open file after compaction: %w", err)
	}

	buf, err := dynparquet.NewSerializedBuffer(pf)
	if err != nil {
		return nil, 0, 0, err
	}

	f.parts.Add(1)
	return []parts.Part{parts.NewParquetPart(compact[0].TX(), buf, append(options, parts.WithRelease(f.parts.Done))...)}, preCompactionSize, accountant.n, nil
}

// Reset is called when the level no longer has active parts in it at the end of a compaction.
func (f *FileCompaction) Reset() {
	f.parts.Wait() // Wait for all parts to be released.
	for _, file := range f.indexFiles {
		if err := file.Close(); err != nil {
			level.Error(f.logger).Log("msg", "failed to close level file", "err", err)
		}
	}

	// Delete all the files in the directory level. And open a new file.
	if err := os.RemoveAll(f.dir); err != nil {
		level.Error(f.logger).Log("msg", "failed to remove level directory", "err", err)
	}

	if err := os.MkdirAll(f.dir, dirPerms); err != nil {
		level.Error(f.logger).Log("msg", "failed to create level directory", "err", err)
	}

	f.indexFiles = nil
	_, err := f.createIndexFile(len(f.indexFiles))
	if err != nil {
		level.Error(f.logger).Log("msg", "failed to create new level file", "err", err)
	}
}

// recovery the level from the given directory.
func (f *FileCompaction) recover(options ...parts.Option) ([]parts.Part, error) {
	defer func() {
		_, err := f.createIndexFile(len(f.indexFiles))
		if err != nil {
			level.Error(f.logger).Log("msg", "failed to create new level file", "err", err)
		}
	}()
	recovered := []parts.Part{}
	err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if filepath.Ext(path) != IndexFileExtension {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info: %v", err)
		}

		if info.Size() == 0 { // file empty, nothing to recover.
			return nil
		}

		file, err := f.openIndexFile(path)
		if err != nil {
			return fmt.Errorf("failed to open file: %v", err)
		}

		// Recover all parts from file.
		fileParts := []parts.Part{}
		if err := func() error {
			for offset := info.Size(); offset > 0; {
				offset -= 8
				size := make([]byte, 8)
				if n, err := file.ReadAt(size, offset); n != 8 {
					return fmt.Errorf("failed to read size from file: %v", err)
				}
				parquetSize := int64(binary.LittleEndian.Uint64(size))
				offset -= parquetSize

				pf, err := parquet.OpenFile(io.NewSectionReader(file, offset, parquetSize), parquetSize)
				if err != nil {
					return err
				}

				buf, err := dynparquet.NewSerializedBuffer(pf)
				if err != nil {
					return err
				}

				var tx int
				txstr, ok := buf.ParquetFile().Lookup(ParquetCompactionTXKey)
				if !ok {
					level.Warn(f.logger).Log("msg", "failed to find compaction_tx metadata", "file", file.Name())
					tx = 0 // Downgrade the compaction tx so that all future reads will be able to read this part.
				} else {
					tx, err = strconv.Atoi(txstr)
					if err != nil {
						level.Warn(f.logger).Log("msg", "failed to parse compaction_tx metadata", "file", file.Name(), "err", err)
						tx = 0 // Downgrade the compaction tx so that all future reads will be able to read this part.
					}
				}

				f.parts.Add(1)
				fileParts = append(fileParts, parts.NewParquetPart(uint64(tx), buf, append(options, parts.WithRelease(f.parts.Done))...))
			}

			return nil
		}(); err != nil {
			for _, part := range fileParts {
				part.Release()
			}

			// If we failed to recover the file, remove it.
			if err := f.file().Close(); err != nil {
				level.Error(f.logger).Log("msg", "failed to close level file after failed recovery", "err", err)
			}
			f.indexFiles = f.indexFiles[:len(f.indexFiles)-1] // Remove the file from the list of files.
			return err
		}

		recovered = append(recovered, fileParts...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return recovered, nil
}

// Sync calls Sync on the underlying file.
func (f *FileCompaction) Sync() error { return f.file().Sync() }

type inMemoryLevel struct {
	compact Compaction
	maxSize int64
}

func (l *inMemoryLevel) MaxSize() int64 { return l.maxSize }
func (l *inMemoryLevel) Snapshot(snapshot []parts.Part, writer func(parts.Part) error, _ string) error {
	for _, part := range snapshot {
		if err := writer(part); err != nil {
			return err
		}
	}
	return nil
}
func (l *inMemoryLevel) Reset() {}
func (l *inMemoryLevel) Compact(toCompact []parts.Part, options ...parts.Option) ([]parts.Part, int64, int64, error) {
	if len(toCompact) == 0 {
		return nil, 0, 0, fmt.Errorf("no parts to compact")
	}

	var b bytes.Buffer
	preCompactionSize, err := l.compact(&b, toCompact)
	if err != nil {
		return nil, 0, 0, err
	}

	buf, err := dynparquet.ReaderFromBytes(b.Bytes())
	if err != nil {
		return nil, 0, 0, err
	}

	postCompactionSize := int64(b.Len())
	return []parts.Part{parts.NewParquetPart(toCompact[0].TX(), buf, options...)}, preCompactionSize, postCompactionSize, nil
}
