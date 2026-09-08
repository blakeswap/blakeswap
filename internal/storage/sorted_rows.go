package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const sortRowsPrefix = ".history-result-"
const sortRunHeaderSize = 48 // random identity and authenticated uint64 count
const sortRunFanIn = 16

// SortedRows is an independent, encrypted, disposable query result. Only ordinal
// offsets are stored in cleartext; bodies and sort keys (including IDs/times)
// are authenticated ciphertext. Its random key never uses a wallet credential.
// Close removes the complete owned directory and clears the scoped key.
type SortedRows struct {
	dir    string
	key    []byte
	run    sortRun
	data   *os.File
	index  *os.File
	Count  uint64
	Digest string
	once   sync.Once
}

type sortEntry struct{ raw, key, value []byte }

func (e sortEntry) clear() { clear(e.raw) }

type sortRun struct {
	path     string
	header   []byte
	count    uint64
	size     int64
	maxFrame uint64
	level    uint32
}

type RowSorter struct {
	ctx    context.Context
	result *SortedRows
	less   func([]byte, []byte) bool
	buffer []sortEntry
	bytes  uint64
	runs   []sortRun
}

func NewRowSorter(ctx context.Context, directory string, less func([]byte, []byte) bool) (*RowSorter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if less == nil {
		return nil, errors.New("query ordering is required")
	}
	dir, err := os.MkdirTemp(directory, sortRowsPrefix)
	if err != nil {
		return nil, err
	}
	result := &SortedRows{dir: dir, key: make([]byte, 32)}
	if _, err = rand.Read(result.key); err != nil {
		result.Close()
		return nil, err
	}
	return &RowSorter{ctx: ctx, result: result, less: less}, nil
}
func (r *SortedRows) Close() error {
	var err error
	r.once.Do(func() {
		if r.data != nil {
			err = errors.Join(err, r.data.Close())
		}
		if r.index != nil {
			err = errors.Join(err, r.index.Close())
		}
		clear(r.key)
		err = errors.Join(err, os.RemoveAll(r.dir))
	})
	return err
}
func (s *RowSorter) Close() error {
	for _, entry := range s.buffer {
		entry.clear()
	}
	s.buffer = nil
	if s.result != nil {
		return s.result.Close()
	}
	return nil
}

// CleanupSortedRows runs only after the caller owns this wallet's exclusive
// vault lock. These reserved scratch directories cannot be reopened after a
// process exit because their independent encryption keys are never persisted.
func CleanupSortedRows(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) > len(sortRowsPrefix) && entry.Name()[:len(sortRowsPrefix)] == sortRowsPrefix {
			if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *RowSorter) Add(key, value []byte) error {
	if s.result == nil {
		return errors.New("query sorter already completed")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if len(key) > math.MaxInt-8-len(value) {
		return errors.New("query row size overflow")
	}
	raw := make([]byte, 8, 8+len(key)+len(value))
	binary.BigEndian.PutUint64(raw, uint64(len(key)))
	raw = append(raw, key...)
	raw = append(raw, value...)
	s.buffer = append(s.buffer, sortEntry{raw, raw[8 : 8+len(key)], raw[8+len(key):]})
	s.bytes += uint64(len(raw))
	if len(s.buffer) >= 64 || s.bytes >= 4<<20 {
		return s.flush()
	}
	return nil
}
func (s *RowSorter) flush() error {
	if len(s.buffer) == 0 {
		return nil
	}
	sort.Slice(s.buffer, func(i, j int) bool { return s.less(s.buffer[i].key, s.buffer[j].key) })
	writer, err := s.newRun(uint64(len(s.buffer)))
	if err != nil {
		return err
	}
	defer writer.file.Close()
	for _, entry := range s.buffer {
		if err = writer.write(s.ctx, entry.raw); err != nil {
			return err
		}
	}
	run, err := writer.finish()
	if err != nil {
		return err
	}
	s.runs = append(s.runs, run)
	// Carry complete groups immediately. At most fan-in minus one runs per
	// level remain, so directory bookkeeping grows logarithmically, not once
	// per lifetime row batch.
	for len(s.runs) >= sortRunFanIn {
		start := len(s.runs) - sortRunFanIn
		group := s.runs[start:]
		same := true
		for _, item := range group {
			if item.level != group[0].level {
				same = false
			}
		}
		if !same {
			break
		}
		merged, err := s.merge(group)
		if err != nil {
			return err
		}
		merged.level = group[0].level + 1
		for _, item := range group {
			if err := os.Remove(item.path); err != nil {
				return err
			}
		}
		s.runs = append(s.runs[:start], merged)
	}
	for _, entry := range s.buffer {
		entry.clear()
	}
	s.buffer, s.bytes = nil, 0
	return nil
}
func runAEAD(key, header []byte) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("blakeswap/private-query-run/v1\x00"))
	mac.Write(header)
	derived := mac.Sum(nil)
	defer clear(derived)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func runNonce(index uint64) []byte { return binary.BigEndian.AppendUint64(make([]byte, 4), index) }
func runAAD(header []byte, index, size uint64) []byte {
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(append([]byte(nil), header...), index), size)
}

type runWriter struct {
	file    *os.File
	run     sortRun
	aead    cipher.AEAD
	written uint64
}

func (s *RowSorter) newRun(count uint64) (*runWriter, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckPathSpace(s.result.dir, sortRunHeaderSize, 1); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(s.result.dir, "run-")
	if err != nil {
		return nil, err
	}
	w := &runWriter{file: file, run: sortRun{path: file.Name(), header: make([]byte, sortRunHeaderSize), count: count}}
	copy(w.run.header, []byte("BSQUERY1"))
	if _, err = rand.Read(w.run.header[8:40]); err == nil {
		binary.BigEndian.PutUint64(w.run.header[40:], count)
		w.aead, err = runAEAD(s.result.key, w.run.header)
	}
	if err == nil {
		_, err = file.Write(w.run.header)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return w, nil
}
func (w *runWriter) write(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.written >= w.run.count {
		return errors.New("query run row count changed")
	}
	size := uint64(len(raw)) + uint64(w.aead.Overhead())
	if size > math.MaxInt64-8 {
		return errors.New("query row frame overflow")
	}
	if err := CheckPathSpace(w.run.path, size+8, 1); err != nil {
		return err
	}
	sealed := w.aead.Seal(nil, runNonce(w.written), raw, runAAD(w.run.header, w.written, size))
	defer clear(sealed)
	if _, err := w.file.Write(binary.BigEndian.AppendUint64(nil, size)); err != nil {
		return err
	}
	if _, err := w.file.Write(sealed); err != nil {
		return err
	}
	w.run.maxFrame = max(w.run.maxFrame, size)
	w.written++
	return nil
}
func (w *runWriter) finish() (sortRun, error) {
	defer w.file.Close()
	if w.written != w.run.count {
		return sortRun{}, errors.New("incomplete query run")
	}
	info, err := w.file.Stat()
	if err != nil {
		return sortRun{}, err
	}
	w.run.size = info.Size()
	if err = w.file.Close(); err != nil {
		return sortRun{}, err
	}
	return w.run, nil
}

type runReader struct {
	file   *os.File
	run    sortRun
	aead   cipher.AEAD
	at     uint64
	offset int64
}

func openRun(key []byte, run sortRun) (*runReader, error) {
	file, err := os.Open(run.path)
	if err != nil {
		return nil, err
	}
	r := &runReader{file: file, run: run, offset: sortRunHeaderSize}
	header := make([]byte, sortRunHeaderSize)
	if _, err = io.ReadFull(file, header); err == nil && !bytes.Equal(header, run.header) {
		err = errors.New("query run identity/count changed")
	}
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && info.Size() != run.size {
			err = errors.New("query run length changed")
		}
	}
	if err == nil {
		r.aead, err = runAEAD(key, header)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return r, nil
}
func readRunEntry(file *os.File, run sortRun, aead cipher.AEAD, index uint64, offset int64) (sortEntry, int64, error) {
	if index >= run.count || offset < sortRunHeaderSize || offset > run.size-8 {
		return sortEntry{}, 0, errors.New("query row outside result")
	}
	var frame [8]byte
	if _, err := file.ReadAt(frame[:], offset); err != nil {
		return sortEntry{}, 0, err
	}
	size := binary.BigEndian.Uint64(frame[:])
	if size < uint64(aead.Overhead())+8 || size > uint64(run.size-offset-8) || size > uint64(math.MaxInt) || size > run.maxFrame {
		return sortEntry{}, 0, errors.New("invalid encrypted query frame")
	}
	sealed := make([]byte, int(size))
	if _, err := file.ReadAt(sealed, offset+8); err != nil {
		clear(sealed)
		return sortEntry{}, 0, err
	}
	raw, err := aead.Open(nil, runNonce(index), sealed, runAAD(run.header, index, size))
	clear(sealed)
	if err != nil {
		return sortEntry{}, 0, err
	}
	keySize := binary.BigEndian.Uint64(raw[:8])
	if keySize > uint64(len(raw)-8) {
		clear(raw)
		return sortEntry{}, 0, errors.New("invalid query sort key")
	}
	return sortEntry{raw, raw[8 : 8+int(keySize)], raw[8+int(keySize):]}, offset + 8 + int64(size), nil
}
func (r *runReader) next(ctx context.Context) (sortEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return sortEntry{}, false, err
	}
	if r.at == r.run.count {
		if r.offset != r.run.size {
			return sortEntry{}, false, errors.New("trailing query run data")
		}
		return sortEntry{}, false, nil
	}
	entry, next, err := readRunEntry(r.file, r.run, r.aead, r.at, r.offset)
	if err != nil {
		return sortEntry{}, false, err
	}
	r.at++
	r.offset = next
	return entry, true, nil
}

func (s *RowSorter) merge(runs []sortRun) (sortRun, error) {
	var count uint64
	readers := make([]*runReader, len(runs))
	heads := make([]sortEntry, len(runs))
	present := make([]bool, len(runs))
	defer func() {
		for i, r := range readers {
			heads[i].clear()
			if r != nil {
				r.file.Close()
			}
		}
	}()
	for i, run := range runs {
		if count > math.MaxUint64-run.count {
			return sortRun{}, errors.New("query result count overflow")
		}
		count += run.count
		r, err := openRun(s.result.key, run)
		if err != nil {
			return sortRun{}, err
		}
		readers[i] = r
		head, ok, err := r.next(s.ctx)
		if err != nil {
			return sortRun{}, err
		}
		heads[i], present[i] = head, ok
	}
	writer, err := s.newRun(count)
	if err != nil {
		return sortRun{}, err
	}
	defer writer.file.Close()
	for {
		best := -1
		for i, ok := range present {
			if ok && (best < 0 || s.less(heads[i].key, heads[best].key)) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		if err = writer.write(s.ctx, heads[best].raw); err != nil {
			return sortRun{}, err
		}
		heads[best].clear()
		heads[best], present[best], err = readers[best].next(s.ctx)
		if err != nil {
			return sortRun{}, err
		}
	}
	return writer.finish()
}
func (s *RowSorter) Finish() (*SortedRows, error) {
	if s.result == nil {
		return nil, errors.New("query sorter already completed")
	}
	if err := s.flush(); err != nil {
		return nil, err
	}
	if len(s.runs) == 0 {
		w, err := s.newRun(0)
		if err != nil {
			return nil, err
		}
		run, err := w.finish()
		if err != nil {
			return nil, err
		}
		s.runs = []sortRun{run}
	}
	for len(s.runs) > 1 {
		var next []sortRun
		for start := 0; start < len(s.runs); start += sortRunFanIn {
			group := s.runs[start:min(start+sortRunFanIn, len(s.runs))]
			run, err := s.merge(group)
			if err != nil {
				return nil, err
			}
			next = append(next, run)
			for _, old := range group {
				if err = os.Remove(old.path); err != nil {
					return nil, err
				}
			}
		}
		s.runs = next
	}
	result := s.result
	result.run = s.runs[0]
	result.Count = result.run.count
	if result.Count > math.MaxInt64/8 {
		return nil, errors.New("query ordinal index overflow")
	}
	reader, err := openRun(result.key, result.run)
	if err != nil {
		return nil, err
	}
	defer reader.file.Close()
	index, err := os.OpenFile(filepath.Join(result.dir, "ordinals"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	result.index = index
	digest := sha256.New()
	digest.Write([]byte("["))
	for n := uint64(0); ; n++ {
		offset := reader.offset
		entry, ok, err := reader.next(s.ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if _, err = index.Write(binary.BigEndian.AppendUint64(nil, uint64(offset))); err != nil {
			entry.clear()
			return nil, err
		}
		if n > 0 {
			digest.Write([]byte(","))
		}
		digest.Write(entry.value)
		entry.clear()
	}
	digest.Write([]byte("]"))
	result.Digest = hex.EncodeToString(digest.Sum(nil))
	if err = index.Sync(); err != nil {
		return nil, err
	}
	result.data, err = os.Open(result.run.path)
	if err != nil {
		return nil, err
	}
	s.result = nil
	return result, nil
}
func (r *SortedRows) Page(ctx context.Context, start, limit uint64) ([][]byte, error) {
	if start > r.Count || limit > 500 {
		return nil, errors.New("query cursor outside result")
	}
	end := r.Count
	if limit < end-start {
		end = start + limit
	}
	aead, err := runAEAD(r.key, r.run.header)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := make([][]byte, 0, end-start)
	fail := func(err error) ([][]byte, error) {
		for _, row := range rows {
			clear(row)
		}
		return nil, err
	}
	for n := start; n < end; n++ {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		var offset [8]byte
		if _, err = r.index.ReadAt(offset[:], int64(n*8)); err != nil {
			return fail(err)
		}
		position := binary.BigEndian.Uint64(offset[:])
		if position > math.MaxInt64 {
			return fail(errors.New("invalid query offset"))
		}
		entry, _, err := readRunEntry(r.data, r.run, aead, n, int64(position))
		if err != nil {
			return fail(err)
		}
		rows = append(rows, append([]byte(nil), entry.value...))
		entry.clear()
	}
	return rows, nil
}
