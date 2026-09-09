package storage

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

const (
	pageFormatVersion uint16 = 1
	pageTargetBytes          = 64 << 10
	pageMaxRows              = 256
	pageMaxPayload           = 64 << 20
	pageFrameSize            = 20
	pageDataMagic            = "GBLROW01"
	pageHeadMagic            = "GBLHED01"
	pageWALMagic             = "GBLWAL01"
)

var pageCRC = crc32.MakeTable(crc32.Castagnoli)

type rowPageRef struct {
	Hash        [32]byte
	Rows        int
	Bytes       int
	MemoryBytes int64
}

type tablePageRefs struct {
	Pages             []rowPageRef
	Indexes           []DiskIndexDescriptor
	IndexFingerprints [][32]byte
}
type databasePageRefs struct{ Tables []tablePageRefs }
type pageManifest struct {
	Version    uint16
	Generation uint64
	Snapshot   StoreSnapshot
	Databases  []databasePageRefs
}

func pageGob(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(value); err != nil {
		return nil, err
	}
	if buffer.Len() > pageMaxPayload {
		return nil, fmt.Errorf("paged record exceeds %d byte limit", pageMaxPayload)
	}
	return buffer.Bytes(), nil
}

func decodePageGob(data []byte, value any) error {
	decoder := gob.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("unexpected trailing bytes in paged record")
	}
	return nil
}

func pageFrame(magic string, payload []byte) []byte {
	frame := make([]byte, pageFrameSize+len(payload))
	copy(frame, magic)
	binary.LittleEndian.PutUint32(frame[8:12], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[12:16], crc32.Checksum(payload, pageCRC))
	binary.LittleEndian.PutUint32(frame[16:20], crc32.Checksum(frame[:16], pageCRC))
	copy(frame[pageFrameSize:], payload)
	return frame
}

func readPageFrame(reader io.Reader, magic string) ([]byte, int64, error) {
	var header [pageFrameSize]byte
	count, err := io.ReadFull(reader, header[:])
	if err != nil {
		return nil, int64(count), err
	}
	if string(header[:8]) != magic {
		return nil, pageFrameSize, fmt.Errorf("invalid paged record magic")
	}
	if crc32.Checksum(header[:16], pageCRC) != binary.LittleEndian.Uint32(header[16:20]) {
		return nil, pageFrameSize, fmt.Errorf("paged record header checksum mismatch")
	}
	size := binary.LittleEndian.Uint32(header[8:12])
	if size > pageMaxPayload {
		return nil, pageFrameSize, fmt.Errorf("paged record length %d exceeds limit", size)
	}
	payload := make([]byte, int(size))
	count, err = io.ReadFull(reader, payload)
	if err != nil {
		return nil, int64(pageFrameSize + count), err
	}
	if crc32.Checksum(payload, pageCRC) != binary.LittleEndian.Uint32(header[12:16]) {
		return nil, int64(pageFrameSize + count), fmt.Errorf("paged record checksum mismatch")
	}
	return payload, int64(pageFrameSize + count), nil
}

func readPageFile(path, magic string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > pageMaxPayload+pageFrameSize {
		return nil, fmt.Errorf("invalid paged file size or kind: %s", path)
	}
	data, count, err := readPageFrame(file, magic)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if count != info.Size() {
		return nil, fmt.Errorf("trailing bytes in paged file %s", path)
	}
	return data, nil
}

type cachedRowPage struct {
	key  [32]byte
	data []byte
}

type rowPageCache struct {
	mu      sync.Mutex
	budget  int64
	used    int64
	entries map[[32]byte]*list.Element
	order   list.List
	hits    uint64
	misses  uint64
}

func newRowPageCache(budget int64) *rowPageCache {
	if budget < 0 {
		budget = 0
	}
	return &rowPageCache{budget: budget, entries: make(map[[32]byte]*list.Element)}
}

func (cache *rowPageCache) get(key [32]byte) ([]byte, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok {
		cache.misses++
		return nil, false
	}
	cache.hits++
	cache.order.MoveToFront(entry)
	return entry.Value.(cachedRowPage).data, true
}

func (cache *rowPageCache) put(key [32]byte, data []byte) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if int64(len(data)) > cache.budget || cache.budget == 0 {
		return
	}
	if entry, ok := cache.entries[key]; ok {
		cache.order.MoveToFront(entry)
		return
	}
	for cache.used+int64(len(data)) > cache.budget {
		entry := cache.order.Back()
		old := entry.Value.(cachedRowPage)
		cache.used -= int64(len(old.data))
		delete(cache.entries, old.key)
		cache.order.Remove(entry)
	}
	cache.entries[key] = cache.order.PushFront(cachedRowPage{key: key, data: data})
	cache.used += int64(len(data))
}

func (cache *rowPageCache) clear() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	clear(cache.entries)
	cache.order.Init()
	cache.used = 0
}

func decodeRowPage(data []byte, ref rowPageRef) ([]Row, error) {
	if len(data) != ref.Bytes || sha256.Sum256(data) != ref.Hash {
		return nil, fmt.Errorf("row page SHA-256 or length mismatch")
	}
	var rows []Row
	if err := decodePageGob(data, &rows); err != nil {
		return nil, err
	}
	if len(rows) != ref.Rows || len(rows) > pageMaxRows {
		return nil, fmt.Errorf("row page count mismatch")
	}
	return rows, nil
}
