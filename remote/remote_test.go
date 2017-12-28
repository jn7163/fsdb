package remote_test

import (
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fishy/fsdb/bucket"
	"github.com/fishy/fsdb/interface"
	"github.com/fishy/fsdb/local"
	"github.com/fishy/fsdb/remote"
)

type dbCollection struct {
	DB     fsdb.FSDB
	Local  fsdb.Local
	Remote *bucket.Mock
	Opts   remote.OptionsBuilder
}

func TestLocal(t *testing.T) {
	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)

	key := fsdb.Key("foo")
	content := "bar"

	if _, err := db.DB.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"read from empty remote db should return NoSuchKeyError, got %v",
			err,
		)
	}

	if err := db.DB.Write(key, strings.NewReader(content)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	compareContent(t, db.DB, key, content)

	if err := db.DB.Delete(key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := db.DB.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"read from empty remote db should return NoSuchKeyError, got %v",
			err,
		)
	}
}

func TestRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	delay := time.Millisecond * 100
	longer := time.Millisecond * 150

	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)
	db.Opts.SetUploadDelay(delay).SetSkipFunc(remote.UploadAll)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)

	key := fsdb.Key("foo")
	content := "bar"

	if _, err := db.DB.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"read from empty remote db should return NoSuchKeyError, got %v",
			err,
		)
	}

	if err := db.DB.Write(key, strings.NewReader(content)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	time.Sleep(longer)

	if _, err := db.Local.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"key should be uploaded to remote and deleted locally, got %v",
			err,
		)
	}

	compareContent(t, db.DB, key, content)
	// Now it should be available locally
	compareContent(t, db.Local, key, content)

	time.Sleep(longer)

	if _, err := db.Local.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"key should be uploaded to remote and deleted locally again, got %v",
			err,
		)
	}

	compareContent(t, db.DB, key, content)
	// Now it should be available locally
	compareContent(t, db.Local, key, content)

	if err := db.DB.Delete(key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := db.DB.Read(key); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"read from empty remote db should return NoSuchKeyError, got %v",
			err,
		)
	}
}

func TestSkip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	delay := time.Millisecond * 100
	longer := delay * 2

	key1 := fsdb.Key("foo")
	key2 := fsdb.Key("bar")
	content := "foobar"

	skipFunc := func(key fsdb.Key) bool {
		return key.Equals(key2)
	}

	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)
	db.Opts.SetUploadDelay(delay).SetSkipFunc(skipFunc)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)

	if err := db.DB.Write(key1, strings.NewReader(content)); err != nil {
		t.Fatalf("Write %v failed: %v", key1, err)
	}
	if err := db.DB.Write(key2, strings.NewReader(content)); err != nil {
		t.Fatalf("Write %v failed: %v", key2, err)
	}

	time.Sleep(longer)

	if _, err := db.Local.Read(key1); !fsdb.IsNoSuchKeyError(err) {
		t.Errorf(
			"%v should be uploaded to remote and deleted locally, got %v",
			key1,
			err,
		)
	}
	compareContent(t, db.Local, key2, content)

	compareContent(t, db.DB, key1, content)
	compareContent(t, db.DB, key2, content)
}

func TestSlowUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Write 6 keys, provide 4 threads to upload. After one upload cycle there
	// should be 2 keys left locally.

	delay := time.Millisecond * 100
	// longer should be slightly larger than 2 * delay,
	// as we need one delay before uploading and another delay for uploading.
	longer := time.Millisecond * 250

	keys := []fsdb.Key{
		fsdb.Key("key0"),
		fsdb.Key("key1"),
		fsdb.Key("key2"),
		fsdb.Key("key3"),
		fsdb.Key("key4"),
		fsdb.Key("key5"),
	}
	content := "foobar"
	left := 2

	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)
	db.Remote.WriteDelay = bucket.MockOperationDelay{
		Before: delay,
		After:  0,
	}
	db.Opts.SetUploadDelay(delay)
	db.Opts.SetUploadThreadNum(len(keys) - left)
	db.Opts.SetSkipFunc(remote.UploadAll)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)

	for _, key := range keys {
		if err := db.DB.Write(key, strings.NewReader(content)); err != nil {
			t.Fatalf("Write %v failed: %v", key, err)
		}
	}

	time.Sleep(longer)
	localKeys := scanKeys(t, db.Local)
	if len(localKeys) != left {
		t.Errorf("Expected %d local keys left, got %v", left, localKeys)
	}
}

func TestUploadRaceCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Write content1, overwrite with content2 during upload.
	// Check read result after upload finishes.

	delay := time.Millisecond * 100
	// secondWrite should be between delay and 2 * delay
	secondWrite := time.Millisecond * 150
	// readTime should be slightly larger than 2 * delay to make sure the upload
	// finished.
	readTime := time.Millisecond * 250

	key := fsdb.Key("key")
	content1 := "foo"
	content2 := "bar"

	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)
	db.Remote.WriteDelay = bucket.MockOperationDelay{
		Before: delay,
		After:  0,
	}
	db.Opts.SetUploadDelay(delay).SetSkipFunc(remote.UploadAll)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)

	if err := db.DB.Write(key, strings.NewReader(content1)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	go func() {
		time.Sleep(secondWrite)
		if err := db.DB.Write(key, strings.NewReader(content2)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		compareContent(t, db.DB, key, content2)
	}()

	time.Sleep(readTime)
	compareContent(t, db.Local, key, content2)
	compareContent(t, db.DB, key, content2)
}

func TestRemoteReadRaceCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Write content1, wait for upload.
	// Overwrite with content2 during slow read. Check read result.

	delay := time.Millisecond * 100
	longer := time.Millisecond * 150
	secondWrite := 2 * delay

	key := fsdb.Key("key")
	content1 := "foo"
	content2 := "bar"

	root, db := createRemoteDB(t)
	defer os.RemoveAll(root)
	db.Remote.ReadDelay = bucket.MockOperationDelay{
		Before: delay,
		After:  0,
	}
	db.Opts.SetUploadDelay(delay).SetSkipFunc(remote.UploadAll)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)

	if err := db.DB.Write(key, strings.NewReader(content1)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	go func() {
		time.Sleep(secondWrite)
		if err := db.DB.Write(key, strings.NewReader(content2)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}()

	time.Sleep(longer)
	// When this read finishes, second write already happened
	compareContent(t, db.DB, key, content2)
}

func createRemoteDB(t *testing.T) (root string, db dbCollection) {
	root, err := ioutil.TempDir("", "fsdb_remote_")
	if err != nil {
		t.Fatalf("failed to get tmp dir: %v", err)
	}
	if !strings.HasSuffix(root, local.PathSeparator) {
		root += local.PathSeparator
	}
	localRoot := root + "local"
	remoteRoot := root + "remote"
	db.Local = local.Open(local.NewDefaultOptions(localRoot))
	db.Remote = bucket.MockBucket(remoteRoot)
	db.Opts = remote.NewDefaultOptions()
	db.Opts.SetSkipFunc(remote.SkipAll)
	db.DB = remote.Open(db.Local, db.Remote, db.Opts)
	return
}

func compareContent(t *testing.T, db fsdb.FSDB, key fsdb.Key, content string) {
	t.Helper()

	reader, err := db.Read(key)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	defer reader.Close()
	buf, err := ioutil.ReadAll(reader)
	if err != nil {
		t.Fatalf("read content failed: %v", err)
	}
	if content != string(buf) {
		t.Errorf("read content failed, expected %q, got %q", content, buf)
	}
}

func scanKeys(t *testing.T, db fsdb.Local) []fsdb.Key {
	t.Helper()

	keys := make([]fsdb.Key, 0)
	if err := db.ScanKeys(
		func(key fsdb.Key) bool {
			keys = append(keys, key)
			return true
		},
		fsdb.IgnoreAllErrFunc,
	); err != nil {
		t.Fatalf("ScanKeys returned error: %v", err)
	}
	return keys
}
