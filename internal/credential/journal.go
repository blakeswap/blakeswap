package credential

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const JournalName = "credential.json"

// Journal contains only local credential references and a verified public
// recovery identity. It is installation metadata, never portable wallet state.
type Journal struct {
	Version  int    `json:"version"`
	Key      Key    `json:"key"`
	Origin   string `json:"origin"`
	Phase    string `json:"phase"`
	Identity string `json:"identity,omitempty"`
}

type Profiles struct {
	Store Store
	// After is an injectable interruption boundary. Production leaves it nil.
	After func(string) error
}

func (p Profiles) after(stage string) error {
	if p.After != nil {
		return p.After(stage)
	}
	return nil
}

func validKey(k Key) bool {
	if k.Installation == "" || len(k.Installation) > 128 || k.Profile == "" || len(k.Profile) > 128 || len(k.Record) != 64 {
		return false
	}
	_, err := hex.DecodeString(k.Record)
	return err == nil
}

func validIdentity(identity string) bool {
	if len(identity) != 64 {
		return false
	}
	_, err := hex.DecodeString(identity)
	return err == nil
}

func ReadJournal(root string) (Journal, error) {
	var j Journal
	path := filepath.Join(root, JournalName)
	info, err := os.Lstat(path)
	if err != nil {
		return j, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return j, errors.New("credential journal must be a private bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return j, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return j, errors.New("credential journal changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return j, err
	}
	if len(raw) > 8192 || json.Unmarshal(raw, &j) != nil || j.Version != 1 || !validKey(j.Key) || (j.Origin != "legacy" && j.Origin != "new") {
		return j, errors.New("invalid credential journal")
	}
	switch j.Phase {
	case "discovered", "stored", "verified", "active", "removed":
	default:
		return j, errors.New("invalid credential migration phase")
	}
	if (j.Identity != "" && !validIdentity(j.Identity)) || ((j.Phase == "verified" || j.Phase == "active" || j.Phase == "removed") && j.Identity == "") {
		return j, errors.New("credential journal lacks verified identity")
	}
	return j, nil
}

func writeJournal(root string, j Journal) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(root, JournalName)); err != nil {
		return err
	}
	return syncDir(root)
}

func syncDir(root string) error {
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (p Profiles) begin(root string, key Key, origin string) (Journal, error) {
	j, err := ReadJournal(root)
	if err == nil {
		if j.Key.Installation != key.Installation || j.Key.Profile != key.Profile || (key.Record != "" && key.Record != j.Key.Record) || j.Origin != origin {
			return j, ErrConflict
		}
		return j, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return j, err
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return j, err
	}
	key.Record = hex.EncodeToString(random[:])
	if !validKey(key) {
		return j, errors.New("invalid credential profile identity")
	}
	j = Journal{Version: 1, Key: key, Origin: origin, Phase: "discovered"}
	if err = writeJournal(root, j); err != nil {
		return j, err
	}
	return j, p.after("discovered")
}

func (p Profiles) record(root string, j *Journal, phase string) error {
	j.Phase = phase
	if err := writeJournal(root, *j); err != nil {
		return err
	}
	return p.after(phase)
}

// establish never replaces an item. A lost Create acknowledgement is resolved
// by reading the exact journal account and comparing against the retained input.
func (p Profiles) establish(ctx context.Context, key Key, password []byte) error {
	if p.Store == nil {
		return ErrUnavailable
	}
	stored, err := p.Store.Get(ctx, key)
	if errors.Is(err, ErrMissing) {
		clear(stored)
		createErr := p.Store.Create(ctx, key, password)
		if err = p.after("created"); err != nil {
			return err
		}
		stored, err = p.Store.Get(ctx, key)
		if err != nil && createErr != nil {
			clear(stored)
			return createErr
		}
	}
	defer clear(stored)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(stored, password) != 1 {
		return ErrConflict
	}
	return nil
}

// Migrate must run while the installation owns its lifecycle lock, before any
// of this profile's vaults open. verify opens every existing vault and returns
// its common recovery identity; missing/corrupt/mismatched vaults are failures.
func (p Profiles) Migrate(ctx context.Context, root string, key Key, verify func([]byte) (string, error)) ([]byte, error) {
	j, err := ReadJournal(root)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(filepath.Join(root, "vault.password")); e != nil {
			return nil, e
		}
		j, err = p.begin(root, key, "legacy")
	} else if err == nil && (j.Key.Installation != key.Installation || j.Key.Profile != key.Profile || (key.Record != "" && key.Record != j.Key.Record)) {
		err = ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if p.Store == nil {
		return nil, ErrUnavailable
	}
	var password []byte
	if j.Phase == "active" || j.Phase == "removed" {
		password, err = p.Store.Get(ctx, j.Key)
	} else {
		if j.Origin != "legacy" {
			return nil, errors.New("new wallet credential installation is incomplete")
		}
		password, err = File(filepath.Join(root, "vault.password")).Acquire(ctx)
		if err == nil {
			err = p.establish(ctx, j.Key, password)
		}
		if err == nil && j.Phase == "discovered" {
			err = p.record(root, &j, "stored")
		}
	}
	if err != nil {
		clear(password)
		return nil, err
	}
	defer clear(password)
	if len(password) < 16 || len(password) > 4096 {
		return nil, errors.New("native credential length is invalid")
	}
	identity, err := verify(password)
	if err != nil {
		return nil, err
	}
	if !validIdentity(identity) || (j.Identity != "" && j.Identity != identity) {
		return nil, ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	j.Identity = identity
	if j.Phase != "active" && j.Phase != "removed" {
		if err = p.record(root, &j, "verified"); err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = p.record(root, &j, "active"); err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if j.Origin == "legacy" && j.Phase != "removed" {
		if err = os.Remove(filepath.Join(root, "vault.password")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err = syncDir(root); err != nil {
			return nil, err
		}
		if err = p.after("file_removed"); err != nil {
			return nil, err
		}
		if err = p.record(root, &j, "removed"); err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), password...), nil
}

// Initialize is for a new private staging directory only. Its unique journal
// account prevents another abandoned installation from donating authority.
// No plaintext file is ever created; the caller atomically publishes this
// directory only after initialization and verification have both completed.
func (p Profiles) Initialize(ctx context.Context, root string, key Key, identity string, initialize func([]byte) error, verify func([]byte) (string, error)) ([]byte, error) {
	if !validIdentity(identity) {
		return nil, errors.New("new credential requires its recovery identity")
	}
	j, err := p.begin(root, key, "new")
	if err != nil {
		return nil, err
	}
	if j.Identity != "" && j.Identity != identity {
		return nil, ErrConflict
	}
	if j.Identity == "" {
		j.Identity = identity
		if err = writeJournal(root, j); err != nil {
			return nil, err
		}
	}
	if p.Store == nil {
		return nil, ErrUnavailable
	}
	password, err := p.Store.Get(ctx, j.Key)
	if errors.Is(err, ErrMissing) {
		clear(password)
		if j.Phase != "discovered" {
			return nil, ErrMissing
		}
		var random [32]byte
		if _, err = rand.Read(random[:]); err != nil {
			return nil, err
		}
		password = []byte(hex.EncodeToString(random[:]))
		clear(random[:])
		err = p.establish(ctx, j.Key, password)
	}
	if err != nil {
		clear(password)
		return nil, err
	}
	defer clear(password)
	if len(password) < 16 || len(password) > 4096 {
		return nil, errors.New("native credential length is invalid")
	}
	if j.Phase == "discovered" {
		if err = p.record(root, &j, "stored"); err != nil {
			return nil, err
		}
	}
	if j.Phase != "active" {
		if err = initialize(password); err != nil {
			return nil, err
		}
		verified, err := verify(password)
		if err != nil {
			return nil, err
		}
		if verified != identity {
			return nil, ErrConflict
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = p.record(root, &j, "verified"); err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if err = p.record(root, &j, "active"); err != nil {
			return nil, err
		}
	} else if verified, err := verify(password); err != nil || verified != identity {
		if err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), password...), nil
}

func (p Profiles) Source(root string, key Key) (Source, error) {
	j, err := ReadJournal(root)
	if err != nil {
		return nil, err
	}
	if j.Key.Installation != key.Installation || j.Key.Profile != key.Profile || (key.Record != "" && key.Record != j.Key.Record) || (j.Phase != "active" && j.Phase != "removed") {
		return nil, errors.New("wallet credential migration is incomplete")
	}
	return SourceFunc(func(ctx context.Context) ([]byte, error) {
		if p.Store == nil {
			return nil, ErrUnavailable
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		password, err := p.Store.Get(ctx, j.Key)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && (len(password) < 16 || len(password) > 4096) {
			err = errors.New("native credential length is invalid")
		}
		if err != nil {
			clear(password)
			return nil, err
		}
		return password, nil
	}), nil
}
