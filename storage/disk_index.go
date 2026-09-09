package storage

import (
	"bufio"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const diskIndexMagic = "GBLIDX01"
const diskIndexHeaderSize = 32
const diskIndexMaxKeyBytes = 64 << 10
const diskIndexMaxPageBytes = 2 << 20
const diskIndexTargetPageBytes = 64 << 10
const diskIndexPageEntries = 128

// DiskIndexDescriptor is the immutable manifest reference for one sorted disk
// index. File names are hashes, never user-provided SQL names.
type DiskIndexDescriptor struct {
	File       [32]byte
	Root       int64
	Height     uint16
	Entries    int
	Bytes      int64
	Definition Index
}
type diskIndexEntry struct {
	Key      Row
	Position int
}
type diskIndexChild struct {
	First, Last Row
	Offset      int64
	Count       int
}
type diskIndexPage struct {
	Entries  []diskIndexEntry
	Children []diskIndexChild
	Leaf     bool
}
type DiskIndex struct {
	path       string
	descriptor DiskIndexDescriptor
	cache      *rowPageCache
}

func DiskIndexFileName(descriptor DiskIndexDescriptor) string {
	return fmt.Sprintf("%x.idx", descriptor.File)
}

func indexKeyCompare(a, b Row, definition Index) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if cmp := compareIndexValue(a[i], b[i], indexCollation(definition, i)); cmp != 0 {
			return cmp
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}
func indexEntryCompare(a, b diskIndexEntry, definition Index) int {
	if cmp := indexKeyCompare(a.Key, b.Key, definition); cmp != 0 {
		return cmp
	}
	if a.Position < b.Position {
		return -1
	}
	if a.Position > b.Position {
		return 1
	}
	return 0
}
func diskKeyBytes(key Row) int {
	n := len(key)*80 + 24
	for _, v := range key {
		n += len(v.Text)
	}
	return n
}
func appendDiskUint(dst []byte, n uint64) []byte { return binary.LittleEndian.AppendUint64(dst, n) }
func appendDiskKey(dst []byte, key Row) ([]byte, error) {
	if len(key) > 256 || diskKeyBytes(key) > diskIndexMaxKeyBytes {
		return nil, fmt.Errorf("disk index key exceeds %d bytes or 256 columns", diskIndexMaxKeyBytes)
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(key)))
	for _, v := range key {
		var tag byte
		switch v.Type {
		case TypeInt:
			tag = 1
		case TypeBigInt:
			tag = 2
		case TypeFloat:
			tag = 3
		case TypeDouble:
			tag = 4
		case TypeDecimal:
			tag = 5
		case TypeVarchar:
			tag = 6
		case TypeText:
			tag = 7
		case TypeBoolean:
			tag = 8
		case TypeDate:
			tag = 9
		case TypeDateTime:
			tag = 10
		default:
			return nil, fmt.Errorf("unsupported disk index type %s", v.Type)
		}
		if v.Null {
			dst = append(dst, tag|128)
			continue
		}
		dst = append(dst, tag)
		switch v.Type {
		case TypeInt, TypeBigInt:
			dst = appendDiskUint(dst, uint64(v.Int64))
		case TypeFloat, TypeDouble:
			dst = appendDiskUint(dst, math.Float64bits(v.Float))
		case TypeDecimal, TypeVarchar, TypeText:
			dst = binary.LittleEndian.AppendUint32(dst, uint32(len(v.Text)))
			dst = append(dst, v.Text...)
		case TypeBoolean:
			if v.Bool {
				dst = append(dst, 1)
			} else {
				dst = append(dst, 0)
			}
		case TypeDate, TypeDateTime:
			dst = appendDiskUint(dst, uint64(v.Date.Unix()))
			dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Date.Nanosecond()))
		}
	}
	return dst, nil
}

type diskDecoder struct {
	data []byte
	pos  int
	err  error
}

func (d *diskDecoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.data)-d.pos {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	b := d.data[d.pos : d.pos+n]
	d.pos += n
	return b
}
func (d *diskDecoder) u64() uint64 {
	b := d.take(8)
	if len(b) != 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}
func (d *diskDecoder) integer() int {
	n := d.u64()
	if n > uint64(^uint(0)>>1) {
		d.err = fmt.Errorf("disk index integer overflow")
		return 0
	}
	return int(n)
}
func (d *diskDecoder) key() Row {
	b := d.take(2)
	if len(b) != 2 {
		return nil
	}
	count := int(binary.LittleEndian.Uint16(b))
	if count == 0 || count > 256 {
		d.err = fmt.Errorf("invalid disk index key width")
		return nil
	}
	key := make(Row, count)
	start := d.pos
	for i := range key {
		raw := d.take(1)
		if len(raw) != 1 {
			return nil
		}
		tag := raw[0] & 127
		types := [...]DataType{"", TypeInt, TypeBigInt, TypeFloat, TypeDouble, TypeDecimal, TypeVarchar, TypeText, TypeBoolean, TypeDate, TypeDateTime}
		if tag == 0 || int(tag) >= len(types) {
			d.err = fmt.Errorf("invalid disk index value type")
			return nil
		}
		v := Value{Type: types[tag], Null: raw[0]&128 != 0}
		if v.Null {
			key[i] = v
			continue
		}
		switch v.Type {
		case TypeInt, TypeBigInt:
			v.Int64 = int64(d.u64())
		case TypeFloat, TypeDouble:
			v.Float = math.Float64frombits(d.u64())
			if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
				d.err = fmt.Errorf("invalid indexed float")
			}
		case TypeDecimal, TypeVarchar, TypeText:
			b = d.take(4)
			if len(b) != 4 {
				return nil
			}
			n := binary.LittleEndian.Uint32(b)
			if n > diskIndexMaxKeyBytes {
				d.err = fmt.Errorf("disk index string exceeds limit")
				return nil
			}
			v.Text = string(d.take(int(n)))
			if v.Type == TypeDecimal {
				if _, err := ParseDecimal(v.Text); err != nil {
					d.err = err
				}
			}
		case TypeBoolean:
			b = d.take(1)
			if len(b) != 1 {
				return nil
			}
			if b[0] > 1 {
				d.err = fmt.Errorf("invalid indexed boolean")
			}
			v.Bool = b[0] == 1
		case TypeDate, TypeDateTime:
			seconds := int64(d.u64())
			b = d.take(4)
			if len(b) != 4 {
				return nil
			}
			nanos := binary.LittleEndian.Uint32(b)
			if nanos >= 1e9 {
				d.err = fmt.Errorf("invalid indexed timestamp")
			}
			v.Date = time.Unix(seconds, int64(nanos)).UTC()
		}
		key[i] = v
	}
	if d.pos-start > diskIndexMaxKeyBytes {
		d.err = fmt.Errorf("disk index key exceeds limit")
	}
	return key
}
func encodeDiskPage(page diskIndexPage) ([]byte, error) {
	leaf := byte(0)
	count := len(page.Children)
	if page.Leaf {
		leaf = 1
		count = len(page.Entries)
	}
	if count > diskIndexPageEntries {
		return nil, fmt.Errorf("too many disk index page records")
	}
	data := []byte{leaf}
	data = binary.LittleEndian.AppendUint16(data, uint16(count))
	var err error
	if page.Leaf {
		for _, entry := range page.Entries {
			data, err = appendDiskKey(data, entry.Key)
			if err != nil {
				return nil, err
			}
			data = appendDiskUint(data, uint64(entry.Position))
		}
	} else {
		for _, child := range page.Children {
			data, err = appendDiskKey(data, child.First)
			if err != nil {
				return nil, err
			}
			data, err = appendDiskKey(data, child.Last)
			if err != nil {
				return nil, err
			}
			data = appendDiskUint(data, uint64(child.Offset))
			data = appendDiskUint(data, uint64(child.Count))
		}
	}
	if len(data) > diskIndexMaxPageBytes {
		return nil, fmt.Errorf("disk index page exceeds limit")
	}
	return data, nil
}
func decodeDiskPage(data []byte) (diskIndexPage, error) {
	d := diskDecoder{data: data}
	h := d.take(3)
	if len(h) != 3 {
		return diskIndexPage{}, d.err
	}
	if h[0] > 1 {
		return diskIndexPage{}, fmt.Errorf("invalid disk index page kind")
	}
	page := diskIndexPage{Leaf: h[0] == 1}
	count := int(binary.LittleEndian.Uint16(h[1:]))
	if count > diskIndexPageEntries {
		return page, fmt.Errorf("invalid disk index page count")
	}
	if page.Leaf {
		page.Entries = make([]diskIndexEntry, count)
		for i := range page.Entries {
			page.Entries[i] = diskIndexEntry{Key: d.key(), Position: d.integer()}
		}
	} else {
		if count == 0 {
			return page, fmt.Errorf("empty disk index internal page")
		}
		page.Children = make([]diskIndexChild, count)
		for i := range page.Children {
			page.Children[i] = diskIndexChild{First: d.key(), Last: d.key(), Offset: int64(d.u64()), Count: d.integer()}
		}
	}
	if d.err != nil {
		return page, d.err
	}
	if d.pos != len(data) {
		return page, fmt.Errorf("trailing disk index page data")
	}
	return page, nil
}
func writeDiskRecord(writer io.Writer, data []byte) error {
	if len(data) > diskIndexMaxPageBytes {
		return fmt.Errorf("disk index record too large")
	}
	var header [8]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(len(data)))
	binary.LittleEndian.PutUint32(header[4:], crc32.Checksum(data, pageCRC))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}
func readDiskRecord(reader io.Reader) ([]byte, error) {
	var h [8]byte
	if _, err := io.ReadFull(reader, h[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(h[:4])
	if n > diskIndexMaxPageBytes {
		return nil, fmt.Errorf("invalid disk index record size")
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	if crc32.Checksum(data, pageCRC) != binary.LittleEndian.Uint32(h[4:]) {
		return nil, fmt.Errorf("disk index checksum mismatch")
	}
	return data, nil
}
func writeDiskEntry(writer io.Writer, entry diskIndexEntry) error {
	data, err := appendDiskKey(nil, entry.Key)
	if err != nil {
		return err
	}
	data = appendDiskUint(data, uint64(entry.Position))
	return writeDiskRecord(writer, data)
}
func readDiskEntry(reader io.Reader) (diskIndexEntry, error) {
	data, err := readDiskRecord(reader)
	if err != nil {
		return diskIndexEntry{}, err
	}
	d := diskDecoder{data: data}
	entry := diskIndexEntry{Key: d.key(), Position: d.integer()}
	if d.err != nil {
		return entry, d.err
	}
	if d.pos != len(data) {
		return entry, fmt.Errorf("trailing disk index run bytes")
	}
	return entry, nil
}
func writeDiskChild(writer io.Writer, child diskIndexChild) error {
	data, err := encodeDiskPage(diskIndexPage{Children: []diskIndexChild{child}})
	if err != nil {
		return err
	}
	return writeDiskRecord(writer, data)
}
func readDiskChild(reader io.Reader) (diskIndexChild, error) {
	data, err := readDiskRecord(reader)
	if err != nil {
		return diskIndexChild{}, err
	}
	page, err := decodeDiskPage(data)
	if err != nil {
		return diskIndexChild{}, err
	}
	if page.Leaf || len(page.Children) != 1 {
		return diskIndexChild{}, fmt.Errorf("invalid disk index level record")
	}
	return page.Children[0], nil
}

// BuildDiskIndex performs bounded external merge sorting, then builds immutable
// leaf/fence pages bottom-up using level streams. budgetBytes bounds each run;
// merge fan-in and buffers are bounded separately (at most sixteen readers).
func BuildDiskIndex(directory string, definition Index, columns []Column, visit func(func(int, Row) error) error, budgetBytes int64) (DiskIndexDescriptor, error) {
	descriptor := DiskIndexDescriptor{Definition: definition}
	if len(definition.Columns) == 0 || len(definition.Columns) > 256 {
		return descriptor, fmt.Errorf("invalid disk index column count")
	}
	positions := make([]int, len(definition.Columns))
	for i, name := range definition.Columns {
		positions[i] = -1
		for j, column := range columns {
			if strings.EqualFold(name, column.Name) {
				positions[i] = j
				break
			}
		}
		if positions[i] < 0 {
			return descriptor, fmt.Errorf("%w: %s", ErrColumnNotFound, name)
		}
	}
	if budgetBytes <= 0 {
		budgetBytes = 4 << 20
	}
	budgetBytes = max(256<<10, min(64<<20, budgetBytes))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return descriptor, err
	}
	var temporaries []string
	temporary := func(prefix string) (*os.File, error) {
		f, err := os.CreateTemp(directory, prefix)
		if err == nil {
			temporaries = append(temporaries, f.Name())
		}
		return f, err
	}
	defer func() {
		for _, path := range temporaries {
			_ = os.Remove(path)
		}
	}()
	var runs []string
	var entries []diskIndexEntry
	var runBytes int64
	flush := func() error {
		if len(entries) == 0 {
			return nil
		}
		slices.SortFunc(entries, func(a, b diskIndexEntry) int { return indexEntryCompare(a, b, definition) })
		file, err := temporary(".run-*")
		if err != nil {
			return err
		}
		writer := bufio.NewWriterSize(file, 16<<10)
		for _, entry := range entries {
			if err = writeDiskEntry(writer, entry); err != nil {
				break
			}
		}
		if err == nil {
			err = writer.Flush()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		runs = append(runs, file.Name())
		entries = nil
		runBytes = 0
		return nil
	}
	err := visit(func(position int, row Row) error {
		if position < 0 {
			return fmt.Errorf("negative disk index row position")
		}
		key := make(Row, len(positions))
		for i, j := range positions {
			if j >= len(row) {
				return ErrColumnCount
			}
			key[i] = row[j]
		}
		size := diskKeyBytes(key)
		if size > diskIndexMaxKeyBytes {
			return fmt.Errorf("disk index key exceeds %d bytes", diskIndexMaxKeyBytes)
		}
		if runBytes+int64(size) > budgetBytes && len(entries) > 0 {
			if err := flush(); err != nil {
				return err
			}
		}
		entries = append(entries, diskIndexEntry{Key: key, Position: position})
		runBytes += int64(size)
		descriptor.Entries++
		return nil
	})
	if err != nil {
		return descriptor, err
	}
	if err = flush(); err != nil {
		return descriptor, err
	}
	fanIn := max(2, min(16, int(budgetBytes/(128<<10))))
	for len(runs) > 1 {
		next := make([]string, 0, (len(runs)+fanIn-1)/fanIn)
		for i := 0; i < len(runs); i += fanIn {
			end := min(len(runs), i+fanIn)
			file, err := temporary(".merge-*")
			if err != nil {
				return descriptor, err
			}
			writer := bufio.NewWriterSize(file, 16<<10)
			err = mergeDiskRuns(runs[i:end], definition, func(entry diskIndexEntry) error { return writeDiskEntry(writer, entry) })
			if err == nil {
				err = writer.Flush()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return descriptor, err
			}
			next = append(next, file.Name())
			for _, path := range runs[i:end] {
				_ = os.Remove(path)
			}
		}
		runs = next
	}
	file, err := temporary(".build-*")
	if err != nil {
		return descriptor, err
	}
	defer file.Close()
	if _, err = file.Write(make([]byte, diskIndexHeaderSize)); err != nil {
		return descriptor, err
	}
	level, err := temporary(".level-*")
	if err != nil {
		return descriptor, err
	}
	defer level.Close()
	levelWriter := bufio.NewWriterSize(level, 16<<10)
	writePage := func(page diskIndexPage) (diskIndexChild, error) {
		data, err := encodeDiskPage(page)
		if err != nil {
			return diskIndexChild{}, err
		}
		offset, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return diskIndexChild{}, err
		}
		if err = writeDiskRecord(file, data); err != nil {
			return diskIndexChild{}, err
		}
		child := diskIndexChild{Offset: offset}
		if page.Leaf {
			child.Count = len(page.Entries)
			if child.Count > 0 {
				child.First = page.Entries[0].Key
				child.Last = page.Entries[len(page.Entries)-1].Key
			}
		} else {
			child.First = page.Children[0].First
			child.Last = page.Children[len(page.Children)-1].Last
			for _, c := range page.Children {
				child.Count += c.Count
			}
		}
		return child, nil
	}
	var leaf []diskIndexEntry
	leafBytes := 0
	leafCount := 0
	var previous *diskIndexEntry
	flushLeaf := func() error {
		if len(leaf) == 0 {
			return nil
		}
		child, err := writePage(diskIndexPage{Leaf: true, Entries: leaf})
		if err != nil {
			return err
		}
		if err = writeDiskChild(levelWriter, child); err != nil {
			return err
		}
		leaf = nil
		leafBytes = 0
		leafCount++
		return nil
	}
	if len(runs) > 0 {
		err = mergeDiskRuns(runs, definition, func(entry diskIndexEntry) error {
			if definition.Unique && previous != nil && indexKeyCompare(previous.Key, entry.Key, definition) == 0 {
				nullable := false
				for _, value := range entry.Key {
					nullable = nullable || value.Null
				}
				if !nullable {
					return fmt.Errorf("%w for disk index %q", ErrDuplicateKey, definition.Name)
				}
			}
			copyEntry := entry
			previous = &copyEntry
			size := diskKeyBytes(entry.Key)
			if len(leaf) > 0 && (len(leaf) >= diskIndexPageEntries || leafBytes+size > diskIndexTargetPageBytes) {
				if err := flushLeaf(); err != nil {
					return err
				}
			}
			leaf = append(leaf, entry)
			leafBytes += size
			return nil
		})
		if err != nil {
			return descriptor, err
		}
	}
	if err = flushLeaf(); err != nil {
		return descriptor, err
	}
	if err = levelWriter.Flush(); err != nil {
		return descriptor, err
	}
	if err = level.Close(); err != nil {
		return descriptor, err
	}
	if leafCount == 0 {
		child, err := writePage(diskIndexPage{Leaf: true})
		if err != nil {
			return descriptor, err
		}
		descriptor.Root = child.Offset
	} else {
		count := leafCount
		levelPath := level.Name()
		for {
			readerFile, err := os.Open(levelPath)
			if err != nil {
				return descriptor, err
			}
			reader := bufio.NewReaderSize(readerFile, 16<<10)
			if count == 1 {
				child, err := readDiskChild(reader)
				readerFile.Close()
				if err != nil {
					return descriptor, err
				}
				descriptor.Root = child.Offset
				break
			}
			next, err := temporary(".level-*")
			if err != nil {
				readerFile.Close()
				return descriptor, err
			}
			writer := bufio.NewWriterSize(next, 16<<10)
			var children []diskIndexChild
			size, nextCount := 0, 0
			flushChildren := func() error {
				if len(children) == 0 {
					return nil
				}
				child, err := writePage(diskIndexPage{Children: children})
				if err != nil {
					return err
				}
				if err = writeDiskChild(writer, child); err != nil {
					return err
				}
				children = nil
				size = 0
				nextCount++
				return nil
			}
			for {
				child, readErr := readDiskChild(reader)
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					err = readErr
					break
				}
				childSize := diskKeyBytes(child.First) + diskKeyBytes(child.Last) + 24
				if len(children) >= 2 && (len(children) >= diskIndexPageEntries || size+childSize > diskIndexTargetPageBytes) {
					if err = flushChildren(); err != nil {
						break
					}
				}
				children = append(children, child)
				size += childSize
			}
			if err == nil {
				err = flushChildren()
			}
			if err == nil {
				err = writer.Flush()
			}
			readerFile.Close()
			closeErr := next.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return descriptor, err
			}
			_ = os.Remove(levelPath)
			levelPath = next.Name()
			count = nextCount
			descriptor.Height++
			if descriptor.Height > 32 {
				return descriptor, fmt.Errorf("disk index height exceeded")
			}
		}
	}
	descriptor.Bytes, err = file.Seek(0, io.SeekCurrent)
	if err != nil {
		return descriptor, err
	}
	header := makeDiskIndexHeader(descriptor)
	if _, err = file.WriteAt(header, 0); err != nil {
		return descriptor, err
	}
	if err = file.Sync(); err != nil {
		return descriptor, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return descriptor, err
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return descriptor, err
	}
	copy(descriptor.File[:], hash.Sum(nil))
	if err = file.Close(); err != nil {
		return descriptor, err
	}
	target := filepath.Join(directory, DiskIndexFileName(descriptor))
	if _, err = os.Stat(target); err == nil {
		if err = verifyDiskIndexHash(target, descriptor.File); err != nil {
			return descriptor, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err = os.Rename(file.Name(), target); err != nil {
			return descriptor, err
		}
	} else {
		return descriptor, err
	}
	if err = syncPageDirectory(directory); err != nil {
		return descriptor, err
	}
	return descriptor, nil
}
func makeDiskIndexHeader(descriptor DiskIndexDescriptor) []byte {
	h := make([]byte, diskIndexHeaderSize)
	copy(h, diskIndexMagic)
	binary.LittleEndian.PutUint64(h[8:16], uint64(descriptor.Root))
	binary.LittleEndian.PutUint16(h[16:18], descriptor.Height)
	binary.LittleEndian.PutUint64(h[20:28], uint64(descriptor.Entries))
	binary.LittleEndian.PutUint32(h[28:], crc32.Checksum(h[:28], pageCRC))
	return h
}
func verifyDiskIndexHash(path string, want [32]byte) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], hash.Sum(nil))
	if got != want {
		return fmt.Errorf("disk index file hash mismatch")
	}
	return nil
}

type diskRunHead struct {
	entry  diskIndexEntry
	reader int
}
type diskRunHeap struct {
	entries    []diskRunHead
	definition Index
}

func (h diskRunHeap) Len() int { return len(h.entries) }
func (h diskRunHeap) Less(i, j int) bool {
	return indexEntryCompare(h.entries[i].entry, h.entries[j].entry, h.definition) < 0
}
func (h diskRunHeap) Swap(i, j int) { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *diskRunHeap) Push(v any)   { h.entries = append(h.entries, v.(diskRunHead)) }
func (h *diskRunHeap) Pop() any {
	n := len(h.entries)
	v := h.entries[n-1]
	h.entries[n-1] = diskRunHead{}
	h.entries = h.entries[:n-1]
	return v
}
func mergeDiskRuns(paths []string, definition Index, yield func(diskIndexEntry) error) error {
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	readers := make([]*bufio.Reader, len(paths))
	h := diskRunHeap{definition: definition}
	for i, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		files = append(files, file)
		readers[i] = bufio.NewReaderSize(file, 16<<10)
		entry, err := readDiskEntry(readers[i])
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
		heap.Push(&h, diskRunHead{entry: entry, reader: i})
	}
	for h.Len() > 0 {
		head := heap.Pop(&h).(diskRunHead)
		if err := yield(head.entry); err != nil {
			return err
		}
		entry, err := readDiskEntry(readers[head.reader])
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
		heap.Push(&h, diskRunHead{entry: entry, reader: head.reader})
	}
	return nil
}

func OpenDiskIndex(directory string, descriptor DiskIndexDescriptor, cacheBytes int64) (*DiskIndex, error) {
	if descriptor.Root < diskIndexHeaderSize || descriptor.Height > 32 || descriptor.Entries < 0 || descriptor.Bytes < diskIndexHeaderSize+8 || len(descriptor.Definition.Columns) == 0 {
		return nil, fmt.Errorf("invalid disk index descriptor")
	}
	path := filepath.Join(directory, DiskIndexFileName(descriptor))
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != descriptor.Bytes {
		return nil, fmt.Errorf("disk index file size mismatch")
	}
	header := make([]byte, diskIndexHeaderSize)
	if _, err = io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header) != string(makeDiskIndexHeader(descriptor)) {
		return nil, fmt.Errorf("disk index header mismatch")
	}
	index := &DiskIndex{path: path, descriptor: descriptor, cache: newRowPageCache(cacheBytes)}
	if _, err = index.readPage(file, descriptor.Root); err != nil {
		return nil, err
	}
	return index, nil
}
func (index *DiskIndex) readPage(file *os.File, offset int64) (diskIndexPage, error) {
	if offset < diskIndexHeaderSize || offset > index.descriptor.Bytes-8 {
		return diskIndexPage{}, fmt.Errorf("invalid disk index page offset")
	}
	key := index.descriptor.File
	binary.LittleEndian.PutUint64(key[24:], uint64(offset))
	data, ok := index.cache.get(key)
	if !ok {
		var err error
		data, err = readDiskRecord(io.NewSectionReader(file, offset, index.descriptor.Bytes-offset))
		if err != nil {
			return diskIndexPage{}, err
		}
		index.cache.put(key, data)
	}
	page, err := decodeDiskPage(data)
	if err != nil {
		return page, err
	}
	for _, entry := range page.Entries {
		if len(entry.Key) != len(index.descriptor.Definition.Columns) {
			return page, fmt.Errorf("disk index key width mismatch")
		}
	}
	for _, child := range page.Children {
		if child.Offset >= offset || child.Offset < diskIndexHeaderSize || child.Count < 0 || len(child.First) != len(index.descriptor.Definition.Columns) || len(child.Last) != len(child.First) {
			return page, fmt.Errorf("invalid disk index fence")
		}
	}
	return page, nil
}
func (index *DiskIndex) validScan(scan IndexScan) error {
	if !strings.EqualFold(scan.Name, index.descriptor.Definition.Name) {
		return ErrIndexNotFound
	}
	n := len(index.descriptor.Definition.Columns)
	if len(scan.EqualPrefix) > n || len(scan.EqualPrefix) == n && (scan.Lower != nil || scan.Upper != nil) {
		return fmt.Errorf("invalid disk index bounds")
	}
	return nil
}
func (index *DiskIndex) comparePrefix(key Row, prefix []Value) int {
	for i, v := range prefix {
		if cmp := compareIndexValue(key[i], v, indexCollation(index.descriptor.Definition, i)); cmp != 0 {
			return cmp
		}
	}
	return 0
}
func (index *DiskIndex) matches(key Row, scan IndexScan) bool {
	if index.comparePrefix(key, scan.EqualPrefix) != 0 {
		return false
	}
	i := len(scan.EqualPrefix)
	if scan.Lower != nil {
		cmp := compareIndexValue(key[i], scan.Lower.Value, indexCollation(index.descriptor.Definition, i))
		if cmp < 0 || cmp == 0 && !scan.Lower.Inclusive {
			return false
		}
	}
	if scan.Upper != nil {
		cmp := compareIndexValue(key[i], scan.Upper.Value, indexCollation(index.descriptor.Definition, i))
		if cmp > 0 || cmp == 0 && !scan.Upper.Inclusive {
			return false
		}
	}
	return true
}
func (index *DiskIndex) intersects(child diskIndexChild, scan IndexScan) bool {
	first, last := index.comparePrefix(child.First, scan.EqualPrefix), index.comparePrefix(child.Last, scan.EqualPrefix)
	if first > 0 || last < 0 {
		return false
	}
	i := len(scan.EqualPrefix)
	if scan.Lower != nil && last == 0 {
		cmp := compareIndexValue(child.Last[i], scan.Lower.Value, indexCollation(index.descriptor.Definition, i))
		if cmp < 0 || cmp == 0 && !scan.Lower.Inclusive {
			return false
		}
	}
	if scan.Upper != nil && first == 0 {
		cmp := compareIndexValue(child.First[i], scan.Upper.Value, indexCollation(index.descriptor.Definition, i))
		if cmp > 0 || cmp == 0 && !scan.Upper.Inclusive {
			return false
		}
	}
	return true
}
func (index *DiskIndex) Scan(scan IndexScan, yield func(int) error) error {
	_, err := index.walk(scan, yield, false)
	return err
}
func (index *DiskIndex) Count(scan IndexScan) (int, error) { return index.walk(scan, nil, true) }
func (index *DiskIndex) walk(scan IndexScan, yield func(int) error, countOnly bool) (int, error) {
	if err := index.validScan(scan); err != nil {
		return 0, err
	}
	file, err := os.Open(index.path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	count := 0
	var walk func(int64, uint16) error
	walk = func(offset int64, height uint16) error {
		page, err := index.readPage(file, offset)
		if err != nil {
			return err
		}
		if page.Leaf {
			if height != 0 {
				return fmt.Errorf("disk index height mismatch")
			}
			for ordinal := range page.Entries {
				i := ordinal
				if scan.Descending {
					i = len(page.Entries) - ordinal - 1
				}
				entry := page.Entries[i]
				if !index.matches(entry.Key, scan) {
					continue
				}
				if !countOnly {
					if err := yield(entry.Position); err != nil {
						return err
					}
				}
				count++
			}
			return nil
		}
		if height == 0 {
			return fmt.Errorf("disk index height mismatch")
		}
		for ordinal := range page.Children {
			i := ordinal
			if scan.Descending {
				i = len(page.Children) - ordinal - 1
			}
			child := page.Children[i]
			if !index.intersects(child, scan) {
				continue
			}
			if countOnly && index.matches(child.First, scan) && index.matches(child.Last, scan) {
				count += child.Count
				continue
			}
			if err := walk(child.Offset, height-1); err != nil {
				return err
			}
		}
		return nil
	}
	err = walk(index.descriptor.Root, index.descriptor.Height)
	return count, err
}

// RemoveUnusedDiskIndexes only removes canonical hash-named index files. The
// caller must ensure no active snapshot/reader still references excluded roots.
func RemoveUnusedDiskIndexes(directory string, live []DiskIndexDescriptor) (int, error) {
	keep := make(map[string]struct{}, len(live))
	for _, descriptor := range live {
		keep[DiskIndexFileName(descriptor)] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) != 68 || !strings.HasSuffix(name, ".idx") {
			continue
		}
		valid := true
		for _, c := range name[:64] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if err = os.Remove(filepath.Join(directory, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
