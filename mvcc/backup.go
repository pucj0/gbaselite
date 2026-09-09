package mvcc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gbaselite/internal/atomicfile"
	"hash"
	"io"
	"os"
	"path/filepath"
)

type BackupManifest struct {
	Format int    `json:"format"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Head   uint64 `json:"head"`
}
type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

// Backup pins one physical snapshot. Concurrent writes continue, but mapping
// growth may wait until copying finishes. Output contains MVCC data only.
func (s *Store) Backup(ctx context.Context, target string) (BackupManifest, error) {
	var manifest BackupManifest
	if err := s.AvailabilityError(); err != nil {
		return manifest, err
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := os.Mkdir(target, 0700); err != nil {
		return manifest, err
	}
	if err := syncWALDirectory(target); err != nil {
		return manifest, err
	}
	marker := filepath.Join(target, "backup.incomplete")
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return manifest, err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return manifest, err
	}
	if err = syncWALDirectory(marker); err != nil {
		return manifest, err
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		return manifest, err
	}
	build := filepath.Join(target, "mvcc.db.build")
	f, err = os.OpenFile(build, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		snapshot.Rollback()
		return manifest, err
	}
	digest := sha256.New()
	manifest.Head = number(snapshot.tx.Bucket(metaBucket).Get([]byte("head")))
	manifest.Bytes, err = snapshot.WriteTo(contextWriter{ctx, io.MultiWriter(f, digest)})
	releaseErr := snapshot.Rollback()
	if err == nil {
		err = f.Sync()
	}
	closeErr = f.Close()
	if err = errors.Join(err, releaseErr, closeErr); err != nil {
		return manifest, err
	}
	manifest.Format = 1
	manifest.SHA256 = hex.EncodeToString(digest.Sum(nil))
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err = os.WriteFile(marker, payload, 0600); err != nil {
		return manifest, err
	}
	f, err = os.OpenFile(marker, os.O_RDWR, 0600)
	if err != nil {
		return manifest, err
	}
	err = f.Sync()
	closeErr = f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return manifest, err
	}
	if err = ctx.Err(); err != nil {
		return manifest, err
	}
	if err = atomicfile.Replace(build, filepath.Join(target, "mvcc.db")); err != nil {
		return manifest, err
	}
	if err = syncWALDirectory(build); err != nil {
		return manifest, err
	}
	if err = atomicfile.Replace(marker, filepath.Join(target, "backup.json")); err != nil {
		return manifest, err
	}
	return manifest, syncWALDirectory(marker)
}

type verifiedBackupReader struct {
	ctx      context.Context
	r        io.Reader
	digest   hash.Hash
	manifest BackupManifest
	read     int64
}

func (r *verifiedBackupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	r.read += int64(n)
	r.digest.Write(p[:n])
	if r.read > r.manifest.Bytes {
		return n, fmt.Errorf("backup size exceeds manifest")
	}
	if err == io.EOF && (r.read != r.manifest.Bytes || hex.EncodeToString(r.digest.Sum(nil)) != r.manifest.SHA256) {
		return n, fmt.Errorf("backup checksum/size mismatch")
	}
	return n, err
}
func (s *Store) RestoreBackup(ctx context.Context, source string) error {
	if _, err := os.Stat(filepath.Join(source, "backup.incomplete")); err == nil {
		return fmt.Errorf("incomplete backup")
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.Open(filepath.Join(source, "backup.json"))
	if err != nil {
		return err
	}
	var manifest BackupManifest
	err = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&manifest)
	f.Close()
	if err != nil {
		return err
	}
	if manifest.Format != 1 || len(manifest.SHA256) != 64 || manifest.Bytes <= 0 {
		return fmt.Errorf("invalid backup manifest")
	}
	data, err := os.Open(filepath.Join(source, "mvcc.db"))
	if err != nil {
		return err
	}
	defer data.Close()
	return s.Restore(&verifiedBackupReader{ctx: ctx, r: data, digest: sha256.New(), manifest: manifest})
}
