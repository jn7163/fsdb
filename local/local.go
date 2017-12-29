package local

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/fishy/fsdb/interface"
	"github.com/fishy/fsdb/wrapreader"
)

// Make sure the classes satisify interfaces.
var _ fsdb.Local = new(impl)
var _ error = new(KeyCollisionError)

const tempDirPrefix = "fsdb_"
const tempDirMode os.FileMode = 0700

var errCanceled = errors.New("canceled by keyFunc")

// Filenames used under the entry directory.
const (
	KeyFilename = "key"

	DataFilename     = "data"
	GzipDataFilename = "data.gz"
)

// Permissions for files and directories.
var (
	FileModeForFiles os.FileMode = 0600
	FileModeForDirs  os.FileMode = 0700
)

// KeyCollisionError is an error returned when two keys have the same hash.
type KeyCollisionError struct {
	NewKey fsdb.Key
	OldKey fsdb.Key
}

func (err *KeyCollisionError) Error() string {
	return fmt.Sprintf(
		"key collision detected: new key is %q, old key was %q",
		err.NewKey,
		err.OldKey,
	)
}

type impl struct {
	opts Options
}

// Open opens an fsdb with the given options.
//
// There's no need to close it.
func Open(opts Options) fsdb.Local {
	return &impl{
		opts: opts,
	}
}

func (db *impl) Read(ctx context.Context, key fsdb.Key) (io.ReadCloser, error) {
	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir := db.opts.GetDirForKey(key)
	keyFile := dir + KeyFilename
	if _, err := os.Lstat(keyFile); os.IsNotExist(err) {
		return nil, &fsdb.NoSuchKeyError{Key: key}
	}
	if err := checkKeyCollision(key, keyFile); err != nil {
		return nil, err
	}

	dataFile := dir + DataFilename
	if _, err := os.Lstat(dataFile); err == nil {
		return os.Open(dataFile)
	}

	dataFile = dir + GzipDataFilename
	if _, err := os.Lstat(dataFile); err == nil {
		file, err := os.Open(dataFile)
		if err != nil {
			return nil, err
		}
		reader, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		return wrapreader.Wrap(reader, file), nil
	}

	// Key file exists but there's no data file,
	return nil, &fsdb.NoSuchKeyError{Key: key}
}

func (db *impl) Write(ctx context.Context, key fsdb.Key, data io.Reader) (err error) {
	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	dir := db.opts.GetDirForKey(key)
	keyFile := dir + KeyFilename
	if _, err = os.Lstat(keyFile); err == nil {
		if err = checkKeyCollision(key, keyFile); err != nil {
			return err
		}
	}
	tmpdir, err := db.getTempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Write temp key file
	tmpKeyFile := tmpdir + KeyFilename
	if err = func() error {
		f, err := createFile(tmpKeyFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = io.Copy(f, bytes.NewReader(key)); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Write temp data file
	var tmpDataFile string
	var dataFile string
	if db.opts.GetUseGzip() {
		tmpDataFile = tmpdir + GzipDataFilename
		dataFile = dir + GzipDataFilename
		if err = func() error {
			f, err := createFile(tmpDataFile)
			if err != nil {
				return err
			}
			defer f.Close()
			writer, err := gzip.NewWriterLevel(f, db.opts.GetGzipLevel())
			if err != nil {
				return err
			}
			defer writer.Close()
			if _, err = io.Copy(writer, data); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return err
		}
	} else {
		tmpDataFile = tmpdir + DataFilename
		dataFile = dir + DataFilename
		if err = func() error {
			f, err := createFile(tmpDataFile)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err = io.Copy(f, data); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Move data file
	if err = os.MkdirAll(dir, FileModeForDirs); err != nil && !os.IsExist(err) {
		return err
	}
	for _, file := range []string{DataFilename, GzipDataFilename} {
		if err = os.Remove(dir + file); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = os.Rename(tmpDataFile, dataFile); err != nil {
		return err
	}

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Move key file
	if err = os.Rename(tmpKeyFile, keyFile); err != nil {
		return err
	}
	return nil
}

func (db *impl) Delete(ctx context.Context, key fsdb.Key) error {
	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	dir := db.opts.GetDirForKey(key)
	keyFile := dir + KeyFilename
	if _, err := os.Lstat(keyFile); os.IsNotExist(err) {
		return &fsdb.NoSuchKeyError{Key: key}
	}
	if err := checkKeyCollision(key, keyFile); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (db *impl) ScanKeys(
	ctx context.Context,
	keyFunc fsdb.KeyFunc,
	errFunc fsdb.ErrFunc,
) error {
	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := filepath.Walk(
		db.opts.GetRootDataDir(),
		func(path string, info os.FileInfo, err error) error {
			select {
			default:
			case <-ctx.Done():
				return ctx.Err()
			}

			if err != nil {
				if errFunc(path, err) {
					return filepath.SkipDir
				}
				return err
			}
			if info.IsDir() {
				// Try remove empty directories.
				//
				// It's safe because calling os.Remove on a directory will only work
				// if it's empty, which is exactly what we want.
				//
				// It's possible that after this empty directory is removed,
				// a previously walked directory becomes empty.
				// That could get removed on next scan.
				os.Remove(path)
				return nil
			}
			if filepath.Base(path) == KeyFilename {
				key, err := readKey(path)
				if err != nil {
					if errFunc(path, err) {
						return filepath.SkipDir
					}
					return err
				}
				ret := keyFunc(key)
				if !ret {
					return errCanceled
				}
			}
			return nil
		},
	); err != errCanceled {
		return err
	}
	return nil
}

// getTempDir returns a temp directory ready to use.
func (db *impl) getTempDir() (dir string, err error) {
	root := db.opts.GetRootTempDir()
	if err = os.MkdirAll(root, tempDirMode); err != nil && !os.IsExist(err) {
		return
	}
	dir, err = ioutil.TempDir(root, tempDirPrefix)
	if !strings.HasSuffix(dir, PathSeparator) {
		dir += PathSeparator
	}
	return
}

// checkKeyCollision checks for key collision.
//
// It returns a KeyCollisionError if detected.
func checkKeyCollision(key fsdb.Key, path string) error {
	old, err := readKey(path)
	if err != nil {
		return err
	}
	if key.Equals(old) {
		return nil
	}
	return &KeyCollisionError{
		NewKey: key,
		OldKey: old,
	}
}

// readKey reads a key from the giving path.
func readKey(path string) (fsdb.Key, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	key, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return fsdb.Key(key), nil
}

func createFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, FileModeForFiles)
}
