package executor

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"gbaselite/storage"
)

const externalSortFanIn = 8
const externalSortMaxRuns = 64

type externalSortRow struct {
	values  []any
	ordinal uint64
}
type externalSortRun struct {
	path  string
	bytes int64
}

// externalRowSorter retains a bounded batch and merges private temporary runs.
// Finish yields sorted rows incrementally. Call Close even after Add fails.
// Payload strings are immutable; Add copies the row header and byte slices.
type externalRowSorter struct {
	control    *queryControl
	compare    func([]any, []any) int
	memory     int64
	batchBytes int64
	next       uint64
	batch      []externalSortRow
	runs       []externalSortRun
	files      map[string]int64
	tempBytes  int64
	closed     bool
}

func newExternalRowSorter(q *queryControl, compare func([]any, []any) int) (*externalRowSorter, error) {
	memory := int64(4 << 20)
	if q != nil && q.options.SortMemoryBytes > 0 {
		memory = q.options.SortMemoryBytes
	}
	if memory < 64<<10 {
		return nil, fmt.Errorf("%w: sort memory must be at least 65536 bytes", ErrQueryResourceLimit)
	}
	return &externalRowSorter{control: q, compare: compare, memory: memory, files: make(map[string]int64)}, nil
}

func (s *externalRowSorter) less(a, b externalSortRow) bool {
	cmp := s.compare(a.values, b.values)
	return cmp < 0 || cmp == 0 && a.ordinal < b.ordinal
}

func (s *externalRowSorter) Add(values []any) error {
	if s.closed {
		return errors.New("external sorter is closed")
	}
	if err := s.control.check(); err != nil {
		return err
	}
	size := queryRowBytes(values) + 32
	// Reserving space for eight merge heads keeps very wide rows from evading
	// the budget. Reject them explicitly instead of allocating an unbounded run.
	if size > s.memory/16 {
		return fmt.Errorf("%w: sort row requires %d bytes, maximum is %d; raise sort_memory_mb or reduce selected columns", ErrQueryResourceLimit, size, s.memory/16)
	}
	if s.batchBytes+size > s.memory/2 && len(s.batch) > 0 {
		if err := s.flush(); err != nil {
			return err
		}
	}
	owned := append([]any(nil), values...)
	for i, value := range owned {
		if data, ok := value.([]byte); ok {
			owned[i] = append([]byte(nil), data...)
		}
	}
	s.batch = append(s.batch, externalSortRow{owned, s.next})
	s.next++
	s.batchBytes += size
	return nil
}

type sortInterrupted struct{ err error }

func (s *externalRowSorter) sortBatch() (err error) {
	defer func() {
		if value := recover(); value != nil {
			if stopped, ok := value.(sortInterrupted); ok {
				err = stopped.err
			} else {
				panic(value)
			}
		}
	}()
	checks := 0
	sort.Slice(s.batch, func(i, j int) bool {
		checks++
		if checks&1023 == 0 {
			if err := s.control.check(); err != nil {
				panic(sortInterrupted{err})
			}
		}
		return s.less(s.batch[i], s.batch[j])
	})
	return s.control.check()
}

func (s *externalRowSorter) createRun() (*os.File, error) {
	directory := ""
	if s.control != nil {
		directory = s.control.options.TempDirectory
	}
	file, err := os.CreateTemp(directory, "gbaselite-sort-*.run")
	if err != nil {
		return nil, fmt.Errorf("create sort run: %w", err)
	}
	s.files[file.Name()] = 0
	return file, nil
}

type sortRunWriter struct {
	sorter *externalRowSorter
	file   *os.File
}

func (w sortRunWriter) Write(p []byte) (int, error) {
	if err := w.sorter.control.check(); err != nil {
		return 0, err
	}
	if err := w.sorter.control.reserveTemporary(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	w.sorter.control.releaseTemporary(int64(len(p) - n))
	w.sorter.tempBytes += int64(n)
	w.sorter.files[w.file.Name()] += int64(n)
	return n, err
}

func (s *externalRowSorter) flush() error {
	if len(s.batch) == 0 {
		return nil
	}
	if err := s.sortBatch(); err != nil {
		return err
	}
	file, err := s.createRun()
	if err != nil {
		return err
	}
	writer := bufio.NewWriterSize(sortRunWriter{s, file}, 4096)
	for _, row := range s.batch {
		if err = writeSortRow(writer, row); err != nil {
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
	s.runs = append(s.runs, externalSortRun{file.Name(), s.files[file.Name()]})
	clear(s.batch)
	s.batch = nil
	s.batchBytes = 0
	if len(s.runs) >= externalSortMaxRuns {
		return s.compact()
	}
	return nil
}

func (s *externalRowSorter) removeRun(run externalSortRun) error {
	if err := os.Remove(run.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.control.releaseTemporary(s.files[run.path])
	s.tempBytes -= s.files[run.path]
	delete(s.files, run.path)
	return nil
}

func (s *externalRowSorter) compact() error {
	previous := s.runs
	next := make([]externalSortRun, 0, (len(previous)+externalSortFanIn-1)/externalSortFanIn)
	for i := 0; i < len(previous); i += externalSortFanIn {
		end := i + externalSortFanIn
		if end > len(previous) {
			end = len(previous)
		}
		if end-i == 1 {
			next = append(next, previous[i])
			continue
		}
		file, err := s.createRun()
		if err != nil {
			return err
		}
		writer := bufio.NewWriterSize(sortRunWriter{s, file}, 4096)
		err = s.merge(previous[i:end], func(row externalSortRow) error { return writeSortRow(writer, row) })
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
		next = append(next, externalSortRun{file.Name(), s.files[file.Name()]})
		for _, run := range previous[i:end] {
			if err = s.removeRun(run); err != nil {
				return err
			}
		}
	}
	s.runs = next
	return nil
}

func (s *externalRowSorter) Finish(yield func([]any) error) (err error) {
	if s.closed {
		return errors.New("external sorter is closed")
	}
	defer func() {
		closeErr := s.Close()
		if err == nil {
			err = closeErr
		}
	}()
	if len(s.runs) == 0 {
		if err = s.sortBatch(); err != nil {
			return err
		}
		for _, row := range s.batch {
			if err = s.control.check(); err != nil {
				return err
			}
			if err = yield(row.values); err != nil {
				return err
			}
		}
		return nil
	}
	if err = s.flush(); err != nil {
		return err
	}
	for len(s.runs) > externalSortFanIn {
		if err = s.compact(); err != nil {
			return err
		}
	}
	return s.merge(s.runs, func(row externalSortRow) error { return yield(row.values) })
}

// Close removes only paths created by this sorter. Interrupted/error paths use
// the same cleanup; no shared temporary directory is ever recursively removed.
func (s *externalRowSorter) Close() error {
	if s.closed {
		return nil
	}
	var first error
	for path := range s.files {
		if err := s.removeRun(externalSortRun{path: path}); err != nil && first == nil {
			first = err
		}
	}
	clear(s.batch)
	s.batch = nil
	s.runs = nil
	s.closed = first == nil
	return first
}

type sortMergeHead struct {
	row    externalSortRow
	source int
}
type sortMergeHeap struct {
	values []sortMergeHead
	less   func(externalSortRow, externalSortRow) bool
}

func (h sortMergeHeap) Len() int           { return len(h.values) }
func (h sortMergeHeap) Less(i, j int) bool { return h.less(h.values[i].row, h.values[j].row) }
func (h sortMergeHeap) Swap(i, j int)      { h.values[i], h.values[j] = h.values[j], h.values[i] }
func (h *sortMergeHeap) Push(v any)        { h.values = append(h.values, v.(sortMergeHead)) }
func (h *sortMergeHeap) Pop() any {
	n := len(h.values) - 1
	v := h.values[n]
	h.values[n] = sortMergeHead{}
	h.values = h.values[:n]
	return v
}
func (s *externalRowSorter) merge(runs []externalSortRun, yield func(externalSortRow) error) error {
	files := make([]*os.File, 0, len(runs))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	readers := make([]*bufio.Reader, 0, len(runs))
	heads := &sortMergeHeap{less: s.less}
	for i, run := range runs {
		file, err := os.Open(run.path)
		if err != nil {
			return err
		}
		files = append(files, file)
		reader := bufio.NewReaderSize(file, 4096)
		readers = append(readers, reader)
		row, err := readSortRow(reader, s.memory/16)
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
		heap.Push(heads, sortMergeHead{row, i})
	}
	for heads.Len() > 0 {
		if err := s.control.check(); err != nil {
			return err
		}
		head := heap.Pop(heads).(sortMergeHead)
		if err := yield(head.row); err != nil {
			return err
		}
		row, err := readSortRow(readers[head.source], s.memory/16)
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
		heap.Push(heads, sortMergeHead{row, head.source})
	}
	return nil
}

func writeSortUint(w io.Writer, n uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	_, err := w.Write(b[:])
	return err
}
func readSortUint(r io.Reader) (uint64, error) {
	var b [8]byte
	_, err := io.ReadFull(r, b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}
func writeSortText(w io.Writer, text string) error {
	if err := writeSortUint(w, uint64(len(text))); err != nil {
		return err
	}
	_, err := io.WriteString(w, text)
	return err
}
func writeSortRow(w io.Writer, row externalSortRow) error {
	if err := writeSortUint(w, row.ordinal); err != nil {
		return err
	}
	if err := writeSortUint(w, uint64(len(row.values))); err != nil {
		return err
	}
	for _, value := range row.values {
		var tag byte
		var number uint64
		var text string
		switch v := value.(type) {
		case nil:
			tag = 0
		case string:
			tag = 1
			text = v
		case jsonDocument:
			tag = 2
			text = string(v)
		case collatedText:
			if _, err := w.Write([]byte{11}); err != nil {
				return err
			}
			if err := writeSortText(w, v.Text); err != nil {
				return err
			}
			if err := writeSortText(w, v.Collation); err != nil {
				return err
			}
			continue
		case storage.Decimal:
			tag = 10
			text = string(v)
		case []byte:
			tag = 3
			text = string(v)
		case int:
			tag = 4
			number = uint64(v)
		case int64:
			tag = 5
			number = uint64(v)
		case uint64:
			tag = 6
			number = v
		case float64:
			tag = 7
			number = math.Float64bits(v)
		case bool:
			tag = 8
			if v {
				number = 1
			}
		case time.Time:
			tag = 9
			data, err := v.MarshalBinary()
			if err != nil {
				return err
			}
			text = string(data)
		case int8:
			tag = 5
			number = uint64(v)
		case int16:
			tag = 5
			number = uint64(v)
		case int32:
			tag = 5
			number = uint64(v)
		case uint:
			tag = 6
			number = uint64(v)
		case uint8:
			tag = 6
			number = uint64(v)
		case uint16:
			tag = 6
			number = uint64(v)
		case uint32:
			tag = 6
			number = uint64(v)
		case float32:
			tag = 7
			number = math.Float64bits(float64(v))
		case interface{ String() string }:
			tag = 1
			text = v.String()
		default:
			return fmt.Errorf("unsupported external sort value %T", value)
		}
		if _, err := w.Write([]byte{tag}); err != nil {
			return err
		}
		if tag == 0 {
			continue
		}
		if tag == 1 || tag == 2 || tag == 3 || tag == 9 || tag == 10 {
			if err := writeSortText(w, text); err != nil {
				return err
			}
		} else {
			if err := writeSortUint(w, number); err != nil {
				return err
			}
		}
	}
	return nil
}

func readSortRow(r io.Reader, maximum int64) (externalSortRow, error) {
	ordinal, err := readSortUint(r)
	if err != nil {
		return externalSortRow{}, err
	}
	count, err := readSortUint(r)
	if err != nil {
		return externalSortRow{}, err
	}
	if maximum < 48 || count > uint64((maximum-48)/24) {
		return externalSortRow{}, errors.New("invalid sort run row width")
	}
	values := make([]any, int(count))
	used := int64(48) + int64(count)*24
	for i := range values {
		var tag [1]byte
		if _, err := io.ReadFull(r, tag[:]); err != nil {
			return externalSortRow{}, err
		}
		if tag[0] == 0 {
			continue
		}
		if tag[0] == 11 {
			parts := [2]string{}
			for part := range parts {
				size, err := readSortUint(r)
				if err != nil {
					return externalSortRow{}, err
				}
				if size > uint64(maximum-used) {
					return externalSortRow{}, errors.New("invalid collated sort value length")
				}
				used += int64(size)
				data := make([]byte, int(size))
				if _, err = io.ReadFull(r, data); err != nil {
					return externalSortRow{}, err
				}
				parts[part] = string(data)
			}
			values[i] = collatedText{Text: parts[0], Collation: parts[1]}
			continue
		}
		if tag[0] == 1 || tag[0] == 2 || tag[0] == 3 || tag[0] == 9 || tag[0] == 10 {
			size, err := readSortUint(r)
			if err != nil {
				return externalSortRow{}, err
			}
			if size > uint64(maximum-used) {
				return externalSortRow{}, errors.New("invalid sort run value length")
			}
			used += int64(size)
			data := make([]byte, int(size))
			if _, err = io.ReadFull(r, data); err != nil {
				return externalSortRow{}, err
			}
			switch tag[0] {
			case 1:
				values[i] = string(data)
			case 2:
				values[i] = jsonDocument(data)
			case 3:
				values[i] = data
			case 10:
				values[i] = storage.Decimal(data)
			case 9:
				var date time.Time
				if err = date.UnmarshalBinary(data); err != nil {
					return externalSortRow{}, err
				}
				values[i] = date
			}
			continue
		}
		number, err := readSortUint(r)
		if err != nil {
			return externalSortRow{}, err
		}
		switch tag[0] {
		case 4:
			values[i] = int(number)
		case 5:
			values[i] = int64(number)
		case 6:
			values[i] = number
		case 7:
			values[i] = math.Float64frombits(number)
		case 8:
			values[i] = number != 0
		default:
			return externalSortRow{}, errors.New("invalid sort run value tag")
		}
	}
	return externalSortRow{values, ordinal}, nil
}
