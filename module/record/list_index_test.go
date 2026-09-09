package record

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestLocalStorageListCachesIndependentSortedSnapshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storage, err := NewLocalStorage(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer storage.Close()
		for _, id := range []string{"old.flv", "b.flv", "a.flv"} {
			stamp := time.Unix(200, 0).UTC()
			if id == "old.flv" {
				stamp = time.Unix(100, 0).UTC()
			}
			completeIndexedRecording(t, storage, id, stamp)
		}
		items := listIndexedRecordings(t, storage)
		if got := []string{items[0].ID, items[1].ID, items[2].ID}; !reflect.DeepEqual(got, []string{"a.flv", "b.flv", "old.flv"}) {
			t.Fatalf("recording order = %v", got)
		}
		items[0].ID = "caller mutation"
		if err := os.Remove(filepath.Join(storage.Root(), "b.flv")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storage.Root(), "a.flv"), []byte("externally replaced"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storage.Root(), "a.flv"+metadataSuffix), []byte(`{"stream_key":"external"}`), 0600); err != nil {
			t.Fatal(err)
		}
		cached := listIndexedRecordings(t, storage)
		if len(cached) != 3 || cached[0].ID != "a.flv" || cached[0].Size != 5 || cached[0].StreamKey != "live/index" {
			t.Fatalf("warm list did not reuse its immutable snapshot: %+v", cached)
		}
		time.Sleep(5 * time.Second)
		refreshed := listIndexedRecordings(t, storage)
		if len(refreshed) != 2 || refreshed[0].ID != "a.flv" || refreshed[0].Size != 19 || refreshed[0].StreamKey != "external" {
			t.Fatalf("expired list did not refresh file and metadata changes: %+v", refreshed)
		}
	})
}

func TestLocalStorageListInvalidatesFinalizedAndDeletedRecordings(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if items := listIndexedRecordings(t, storage); len(items) != 0 {
		t.Fatalf("initial list = %+v", items)
	}
	object, err := storage.Create(context.Background(), "active.flv", RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, writeErr := object.Write([]byte("media")); writeErr != nil {
			t.Fatal(writeErr)
		}
		if items := listIndexedRecordings(t, storage); len(items) != 0 {
			t.Fatalf("active partial leaked into list: %+v", items)
		}
	}
	if _, completeErr := object.Complete(context.Background(), RecordingInfo{DurationSec: 12}); completeErr != nil {
		t.Fatal(completeErr)
	}
	items := listIndexedRecordings(t, storage)
	if len(items) != 1 || items[0].State != RecordingCompleted || items[0].Size != 10 || items[0].DurationSec != 12 {
		t.Fatalf("finalization left stale list: %+v", items)
	}
	object, err = storage.Create(context.Background(), "failed.flv", RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}
	listIndexedRecordings(t, storage)
	failed, err := object.Fail(context.Background(), errors.New("write failure"))
	if err != nil {
		t.Fatal(err)
	}
	items = listIndexedRecordings(t, storage)
	if len(items) != 2 || items[0].ID != failed.ID || items[0].State != RecordingFailed || items[0].Error != "write failure" {
		t.Fatalf("failure left stale list: %+v", items)
	}
	if err := storage.Delete(context.Background(), "active.flv"); err != nil {
		t.Fatal(err)
	}
	items = listIndexedRecordings(t, storage)
	if len(items) != 1 || items[0].ID != failed.ID {
		t.Fatalf("deletion left stale list: %+v", items)
	}
}

func TestLocalStorageListCacheHonorsCancellationAndClose(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	completeIndexedRecording(t, storage, "one.flv", time.Unix(100, 0))
	listIndexedRecordings(t, storage)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled warm list error = %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.List(context.Background()); err == nil {
		t.Fatal("closed storage served a cached list")
	}
}

func TestLocalStorageListInvalidatesAfterFailedFinalize(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	object, err := storage.Create(context.Background(), "one.flv", RecordingInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := object.Write([]byte("media")); writeErr != nil {
		t.Fatal(writeErr)
	}
	listIndexedRecordings(t, storage)
	if mkdirErr := os.Mkdir(filepath.Join(storage.Root(), "one.flv"+metadataSuffix), 0700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	failed, err := object.Complete(context.Background(), RecordingInfo{})
	if err == nil {
		t.Fatal("metadata obstruction did not fail completion")
	}
	items := listIndexedRecordings(t, storage)
	if len(items) != 1 || items[0].ID != failed.ID || items[0].State != RecordingFailed || items[0].Error == "" {
		t.Fatalf("failed completion left stale list: %+v", items)
	}
}

func TestLocalStorageListInvalidatesAfterDeleteCleanupError(t *testing.T) {
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	completeIndexedRecording(t, storage, "one.flv", time.Unix(100, 0))
	listIndexedRecordings(t, storage)
	metadata := filepath.Join(storage.Root(), "one.flv"+metadataSuffix)
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(context.Background(), "one.flv"); err == nil {
		t.Fatal("metadata obstruction did not fail deletion")
	}
	items := listIndexedRecordings(t, storage)
	if len(items) != 1 || items[0].StreamKey != "" {
		t.Fatalf("failed deletion retained stale metadata: %+v", items)
	}
}

func TestLocalStorageListCacheDoesNotAuthorizeReplacedMedia(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storage, err := NewLocalStorage(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer storage.Close()
		completeIndexedRecording(t, storage, "one.flv", time.Unix(100, 0))
		listIndexedRecordings(t, storage)
		outside := filepath.Join(t.TempDir(), "outside.flv")
		if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
		media := filepath.Join(storage.Root(), "one.flv")
		if err := os.Remove(media); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, media); err != nil {
			t.Fatal(err)
		}
		if items := listIndexedRecordings(t, storage); len(items) != 1 {
			t.Fatalf("expected warm listing before external rescan: %+v", items)
		}
		if reader, _, err := storage.Open(context.Background(), "one.flv"); err == nil {
			reader.Close()
			t.Fatal("cached entry authorized a symlink media read")
		}
		if _, err := storage.Stat(context.Background(), "one.flv"); err == nil {
			t.Fatal("cached entry authorized a symlink metadata lookup")
		}
		time.Sleep(5 * time.Second)
		if items := listIndexedRecordings(t, storage); len(items) != 0 {
			t.Fatalf("rescan listed symlink media: %+v", items)
		}
	})
}

func TestRecordingListIndexCoalescesConcurrentScans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var index recordingListIndex
		var calls atomic.Int32
		release := make(chan struct{})
		scan := func(context.Context) ([]RecordingInfo, error) { //nolint:unparam // Matches the production scan callback contract.
			calls.Add(1)
			<-release
			return []RecordingInfo{{ID: "one.flv"}}, nil
		}
		const readers = 16
		results := make(chan []RecordingInfo, readers)
		for range readers {
			go func() {
				items, err := index.list(context.Background(), scan)
				if err != nil {
					t.Error(err)
				}
				results <- items
			}()
		}
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("concurrent scans = %d, want 1", got)
		}
		close(release)
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("completed scans = %d, want 1", got)
		}
		for range readers {
			items := <-results
			if len(items) != 1 || items[0].ID != "one.flv" {
				t.Fatalf("reader list = %+v", items)
			}
			items[0].ID = "caller mutation"
		}
	})
}

func TestRecordingListIndexWaiterCancellationDoesNotCancelScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var index recordingListIndex
		var calls atomic.Int32
		release := make(chan struct{})
		scan := func(context.Context) ([]RecordingInfo, error) { //nolint:unparam // Matches the production scan callback contract.
			calls.Add(1)
			<-release
			return []RecordingInfo{{ID: "one.flv"}}, nil
		}
		ownerDone := make(chan error, 1)
		go func() {
			_, err := index.list(context.Background(), scan)
			ownerDone <- err
		}()
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		waiterDone := make(chan error, 1)
		go func() {
			_, err := index.list(ctx, scan)
			waiterDone <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-waiterDone:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter error = %v", err)
			}
		default:
			t.Fatal("canceled waiter did not return while scan was blocked")
		}
		close(release)
		if err := <-ownerDone; err != nil {
			t.Fatalf("owner error = %v", err)
		}
		items, err := index.list(context.Background(), scan)
		if err != nil || len(items) != 1 || calls.Load() != 1 {
			t.Fatalf("owner result was not cached: items=%+v err=%v scans=%d", items, err, calls.Load())
		}
	})
}

func TestRecordingListIndexDoesNotRetainInvalidatedScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var index recordingListIndex
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := index.list(context.Background(), func(context.Context) ([]RecordingInfo, error) {
				<-release
				return []RecordingInfo{{ID: "removed.flv"}}, nil
			}); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()
		index.invalidate()
		close(release)
		<-done
		items, err := index.list(context.Background(), func(context.Context) ([]RecordingInfo, error) {
			return []RecordingInfo{{ID: "new.flv"}}, nil
		})
		if err != nil || len(items) != 1 || items[0].ID != "new.flv" {
			t.Fatalf("invalidated scan was retained: items=%+v err=%v", items, err)
		}
	})
}

func TestRecordingListIndexDoesNotRetainFailedOrExpiredScans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var index recordingListIndex
		failure := errors.New("storage unavailable")
		if _, err := index.list(context.Background(), func(context.Context) ([]RecordingInfo, error) {
			return nil, failure
		}); !errors.Is(err, failure) {
			t.Fatalf("scan error = %v", err)
		}
		calls := 0
		scan := func(context.Context) ([]RecordingInfo, error) {
			calls++
			time.Sleep(6 * time.Second)
			return []RecordingInfo{{ID: "one.flv"}}, nil
		}
		for range 2 {
			items, err := index.list(context.Background(), scan)
			if err != nil || len(items) != 1 {
				t.Fatalf("scan after failure: items=%+v err=%v", items, err)
			}
		}
		if calls != 2 {
			t.Fatalf("slow scan freshness measured from completion: scans=%d", calls)
		}
	})
}

func TestRecordingListIndexBoundsRetainedEntriesAndMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		items []RecordingInfo
	}{
		{name: "entries", items: make([]RecordingInfo, 100_001)},
		{name: "metadata", items: []RecordingInfo{{ID: "one.flv", Error: strings.Repeat("e", 32<<20)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var index recordingListIndex
			calls := 0
			scan := func(context.Context) ([]RecordingInfo, error) {
				calls++
				return test.items, nil
			}
			for range 2 {
				items, err := index.list(context.Background(), scan)
				if err != nil || !reflect.DeepEqual(items, test.items) {
					t.Fatalf("uncacheable list was truncated: count=%d err=%v", len(items), err)
				}
			}
			if calls != 2 {
				t.Fatalf("oversized list was retained: scans=%d", calls)
			}
		})
	}
}

func BenchmarkLocalStorageList(b *testing.B) {
	storage, err := NewLocalStorage(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()
	for i := range 1_000 {
		id := filepath.Join("live", time.Unix(int64(i), 0).UTC().Format("20060102_150405")+".flv")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(storage.Root(), id)), 0700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storage.Root(), id), []byte("media"), 0600); err != nil {
			b.Fatal(err)
		}
		if err := storage.writeMetadata(id, RecordingInfo{StreamKey: "live/index", Format: "flv", StartedAt: time.Unix(int64(i), 0).UTC()}); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("warm", func(b *testing.B) {
		listIndexedRecordings(b, storage)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if items := listIndexedRecordings(b, storage); len(items) != 1_000 {
				b.Fatalf("list count = %d", len(items))
			}
		}
	})
	b.Run("rescan", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			storage.listIndex.invalidate()
			if items := listIndexedRecordings(b, storage); len(items) != 1_000 {
				b.Fatalf("list count = %d", len(items))
			}
		}
	})
}

func completeIndexedRecording(t testing.TB, storage *LocalStorage, id string, started time.Time) {
	t.Helper()
	object, err := storage.Create(context.Background(), id, RecordingInfo{StreamKey: "live/index", Format: "flv", StartedAt: started})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Write([]byte("media")); err != nil {
		t.Fatal(err)
	}
	if _, err := object.Complete(context.Background(), RecordingInfo{}); err != nil {
		t.Fatal(err)
	}
}

func listIndexedRecordings(t testing.TB, storage *LocalStorage) []RecordingInfo {
	t.Helper()
	items, err := storage.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}
