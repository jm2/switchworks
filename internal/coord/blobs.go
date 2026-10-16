// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/attach"
)

// blobStore keeps the bytes of image attachments, content-addressed by
// SHA-256 in <coordinator dir>/attachments (0700; files 0600). File names
// are digests the coordinator computed, never names a sender chose.
//
// Message records (messages.jsonl, kept in memory) and events carry only
// each image's metadata. The bytes are kept:
//   - while a message that carries them is pending (the target's host may
//     be offline), so it can still be delivered: never evicted then;
//   - after delivery, for blobKeep (7 days), so the web UI can show the
//     images in the inbox, the traffic list and transcripts; while the
//     store holds more than blobSoftMax, the oldest delivered images go
//     first.
//
// New attachments are refused while the store holds blobHardMax, which
// only pending images can fill (an agent flooding an offline host).
type blobStore struct {
	dir string

	mu    sync.Mutex
	total int64
}

const (
	blobKeep    = 7 * 24 * time.Hour
	blobSoftMax = 512 << 20
	blobHardMax = 1 << 30
)

func openBlobStore(dir string) (*blobStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	b := &blobStore{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && attach.ValidSHA256(e.Name()) {
			b.total += fi.Size()
		}
	}
	return b, nil
}

var errBlobStoreFull = errors.New("the coordinator's attachment store is full (too many undelivered images); try again later")

// put stores data and returns its digest. Storing the same image again
// only refreshes its age.
func (b *blobStore) put(data []byte) (string, error) {
	sum := attach.SHA256(data)
	p := filepath.Join(b.dir, sum)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if old, err := os.ReadFile(p); err == nil && attach.SHA256(old) == sum {
		_ = os.Chtimes(p, now, now)
		return sum, nil
	} else if err == nil {
		// Torn by a crash (renamed before its data reached the disk):
		// write it again.
		b.total -= int64(len(old))
		_ = os.Remove(p)
	}
	if b.total+int64(len(data)) > blobHardMax {
		return "", errBlobStoreFull
	}
	f, err := os.CreateTemp(b.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store attachment: %v", errors.Join(werr, cerr))
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	b.total += int64(len(data))
	return sum, nil
}

// get returns the bytes of a stored image, checked against its digest.
func (b *blobStore) get(sum string) ([]byte, error) {
	if !attach.ValidSHA256(sum) {
		return nil, errors.New("invalid attachment id")
	}
	data, err := os.ReadFile(filepath.Join(b.dir, sum))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("attachment no longer available")
		}
		return nil, err
	}
	if attach.SHA256(data) != sum {
		return nil, errors.New("attachment is corrupt")
	}
	return data, nil
}

// has reports whether an image is stored.
func (b *blobStore) has(sum string) bool {
	if !attach.ValidSHA256(sum) {
		return false
	}
	_, err := os.Stat(filepath.Join(b.dir, sum))
	return err == nil
}

// prune drops images past their retention, and the oldest while the store
// is over its soft limit. pinned (images of pending messages) are kept.
func (b *blobStore) prune(now time.Time, pinned map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	type blob struct {
		name string
		size int64
		mod  time.Time
	}
	var all []blob
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			if now.Sub(fi.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(b.dir, e.Name()))
			}
			continue
		}
		if !fi.Mode().IsRegular() || !attach.ValidSHA256(e.Name()) {
			continue
		}
		if !pinned[e.Name()] && now.Sub(fi.ModTime()) > blobKeep {
			if os.Remove(filepath.Join(b.dir, e.Name())) == nil {
				continue
			}
		}
		all = append(all, blob{e.Name(), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, x := range all {
		if total <= blobSoftMax {
			break
		}
		if pinned[x.name] {
			continue
		}
		if os.Remove(filepath.Join(b.dir, x.name)) == nil {
			total -= x.size
		}
	}
	b.total = total
}
