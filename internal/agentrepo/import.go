package agentrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Importer fetches agent repositories into Lux's data directory and
// validates a pinned commit. It is safe for concurrent use.
type Importer struct {
	// DataDir is luxd's data directory; mirrors live in DataDir/mirrors.
	DataDir string
	Options Options

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lockMirror serializes fetches of the same mirror within this process.
func (im *Importer) lockMirror(path string) func() {
	im.mu.Lock()
	if im.locks == nil {
		im.locks = map[string]*sync.Mutex{}
	}
	l, ok := im.locks[path]
	if !ok {
		l = &sync.Mutex{}
		im.locks[path] = l
	}
	im.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Sync fetches a source into its mirror, one fetch per mirror at a time,
// and resolves ref (or HEAD) to a commit.
func (im *Importer) Sync(ctx context.Context, kind, src, ref string, cred *Credential) (string, error) {
	mirror := im.MirrorPath(kind, src)
	unlock := im.lockMirror(mirror)
	defer unlock()
	return SyncMirrorAuth(ctx, mirror, kind, src, ref, cred)
}

// Inspection is the result of fetching and validating one commit.
type Inspection struct {
	MirrorPath string  `json:"-"`
	CommitSHA  string  `json:"commit_sha"`
	Report     *Report `json:"report"`
}

// MirrorPath returns the mirror location for a source. Mirrors are keyed by
// source so re-importing the same repository reuses the existing fetch.
func (im *Importer) MirrorPath(kind, src string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + src))
	return filepath.Join(im.DataDir, "mirrors", hex.EncodeToString(h[:12])+".git")
}

// Inspect syncs the source, resolves ref to a commit, checks that commit out
// into a temporary directory, validates it, and removes the checkout.
func (im *Importer) Inspect(ctx context.Context, kind, src, ref string) (*Inspection, error) {
	return im.InspectWith(ctx, kind, src, ref, im.Options)
}

// InspectWith is Inspect with per-call validation options, so one Importer
// (and its mirror locks) can serve workspaces with different policies.
func (im *Importer) InspectWith(ctx context.Context, kind, src, ref string, opts Options) (*Inspection, error) {
	if im.DataDir == "" {
		return nil, fmt.Errorf("importer has no data directory")
	}
	mirror := im.MirrorPath(kind, src)
	unlock := im.lockMirror(mirror)
	sha, err := SyncMirrorAuth(ctx, mirror, kind, src, ref, opts.Credential)
	unlock()
	if err != nil {
		return nil, err
	}
	rep, err := im.inspectCommit(ctx, mirror, sha, opts)
	if err != nil {
		return nil, err
	}
	return &Inspection{MirrorPath: mirror, CommitSHA: sha, Report: rep}, nil
}

// InspectCommit validates an already-fetched commit.
func (im *Importer) InspectCommit(ctx context.Context, mirror, sha string) (*Report, error) {
	return im.inspectCommit(ctx, mirror, sha, im.Options)
}

func (im *Importer) inspectCommit(ctx context.Context, mirror, sha string, opts Options) (*Report, error) {
	tmpRoot := filepath.Join(im.DataDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(tmpRoot, "inspect-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	dest := filepath.Join(work, "agent")
	if err := Checkout(ctx, mirror, sha, dest); err != nil {
		return nil, err
	}
	return Validate(ctx, dest, opts), nil
}
