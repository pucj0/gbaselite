package storage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	walPrepare             uint8 = 1
	walCommit              uint8 = 2
	pageCheckpointCommits        = 64
	pageCheckpointWALBytes       = 16 << 20
)

type pageWALRecord struct {
	Version    uint16
	Kind       uint8
	Generation uint64
	Digest     [32]byte
	Manifest   []byte
}

type pageWALState struct {
	committed      *pageManifest
	committedBytes []byte
	validBytes     int64
	hasRecords     bool
}

// An incomplete final frame is an interrupted append. A complete frame with a
// bad checksum is corruption, including at EOF, and must never be discarded.
func readPageWAL(path string) (pageWALState, error) {
	var state pageWALState
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer file.Close()
	var pending *pageManifest
	var pendingBytes []byte
	for {
		payload, size, err := readPageFrame(file, pageWALMagic)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return state, nil
		}
		if err != nil {
			return state, fmt.Errorf("WAL corruption at byte %d: %w", state.validBytes, err)
		}
		var record pageWALRecord
		if err := decodePageGob(payload, &record); err != nil {
			return state, fmt.Errorf("decode WAL record: %w", err)
		}
		if record.Version != pageFormatVersion {
			return state, fmt.Errorf("unsupported WAL version %d", record.Version)
		}
		if record.Generation == 0 {
			return state, fmt.Errorf("invalid zero WAL generation")
		}
		switch record.Kind {
		case walPrepare:
			if len(record.Manifest) == 0 || sha256.Sum256(record.Manifest) != record.Digest {
				return state, fmt.Errorf("WAL prepare manifest checksum mismatch")
			}
			pending, err = decodePageManifest(record.Manifest)
			if err != nil {
				return state, err
			}
			if pending.Generation != record.Generation {
				return state, fmt.Errorf("WAL prepare generation mismatch")
			}
			if state.committed != nil && pending.Generation <= state.committed.Generation {
				return state, fmt.Errorf("WAL generation is not increasing")
			}
			pendingBytes = record.Manifest
		case walCommit:
			if len(record.Manifest) != 0 || pending == nil || record.Generation != pending.Generation || sha256.Sum256(pendingBytes) != record.Digest {
				return state, fmt.Errorf("WAL commit has no matching prepare")
			}
			state.committed = pending
			state.committedBytes = pendingBytes
			pending, pendingBytes = nil, nil
		default:
			return state, fmt.Errorf("unknown WAL record kind %d", record.Kind)
		}
		state.validBytes += size
		state.hasRecords = true
	}
}

func (p *Persistence) appendPageWAL(kind uint8, manifest *pageManifest, data []byte) error {
	record := pageWALRecord{Version: pageFormatVersion, Kind: kind, Generation: manifest.Generation, Digest: sha256.Sum256(data)}
	if kind == walPrepare {
		record.Manifest = data
	}
	payload, err := pageGob(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(p.pages.walPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open WAL: %w", err)
	}
	defer file.Close()
	// Initialization verified the valid prefix. Remove only an incomplete tail;
	// corruption never reaches this code. This also removes a failed local append.
	if err := file.Truncate(p.pages.walBytes); err != nil {
		return fmt.Errorf("truncate interrupted WAL append: %w", err)
	}
	if _, err := file.Seek(p.pages.walBytes, io.SeekStart); err != nil {
		return err
	}
	frame := pageFrame(pageWALMagic, payload)
	if _, err := file.Write(frame); err != nil {
		return fmt.Errorf("append WAL: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync WAL: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close WAL: %w", err)
	}
	if err := syncPageDirectory(p.pages.directory); err != nil {
		return err
	}
	p.pages.walBytes += int64(len(frame))
	p.pages.stats.WALBytesWritten += uint64(len(frame))
	return nil
}
