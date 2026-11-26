package personal

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type Config struct {
	Server    string `json:"server"`
	Owner     string `json:"owner"`
	RuntimeID string `json:"runtimeId"`
	UID       int    `json:"uid"`
	Name      string `json:"name"`
	CA        string `json:"ca,omitempty"`
}
type Vault struct {
	Dir  string
	UID  int
	lock *os.File
}

func privateDirectory(dir string) (string, int, error) {
	uid := os.Geteuid()
	if uid == 0 || uid != os.Getuid() {
		return "", uid, errors.New("personal agents must run as your normal signed-in OS user, without sudo or setuid")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", uid, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", uid, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(stat.Uid) != uid || info.Mode().Perm() != 0700 {
		return "", uid, errors.New("personal runtime directory must be owned by the signed-in user and have mode 0700")
	}
	real, err := filepath.EvalSymlinks(dir)
	return real, uid, err
}
func OpenVault(dir string) (*Vault, error) {
	real, uid, err := privateDirectory(dir)
	if err != nil {
		return nil, err
	}
	// Reject pre-existing links and foreign-owned state before opening SQLite or credentials.
	err = filepath.WalkDir(real, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != uid || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return errors.New("personal runtime contains unsafe ownership, permissions or a symlink")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(real, "runtime.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "runtime.lock")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("personal agent is already running; use its local controls")
	}
	return &Vault{real, uid, f}, nil
}
func (v *Vault) Close() {
	if v.lock != nil {
		_ = unix.Flock(int(v.lock.Fd()), unix.LOCK_UN)
		v.lock.Close()
	}
}
func (v *Vault) write(name string, data []byte) error {
	f, err := os.CreateTemp(v.Dir, ".private-")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(path, filepath.Join(v.Dir, name))
}
func (v *Vault) Key() (ed25519.PrivateKey, error) {
	raw, err := v.read("agent.key", 4096)
	if errors.Is(err, os.ErrNotExist) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if err = v.write("agent.key", []byte(base64.RawURLEncoding.EncodeToString(key))); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.RawURLEncoding.DecodeString(string(raw))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid personal agent key")
	}
	return ed25519.PrivateKey(key), nil
}
func (v *Vault) Config() (Config, error) {
	var c Config
	b, err := v.read("runtime.json", 1<<20)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(b, &c) != nil || c.UID != v.UID || c.Owner == "" {
		return c, errors.New("runtime belongs to a different signed-in user")
	}
	origin, err := Origin(c.Server)
	if err != nil || origin != c.Server {
		return c, ErrInvalid
	}
	key, err := v.Key()
	if err != nil || RuntimeID(key.Public().(ed25519.PublicKey)) != c.RuntimeID {
		return c, errors.New("runtime identity mismatch")
	}
	return c, nil
}
func (v *Vault) SaveConfig(c Config) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	return v.write("runtime.json", data)
}
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var inner error
	err = raw.Control(func(fd uintptr) { uid, inner = socketUID(int(fd)) })
	if err != nil {
		return 0, err
	}
	return uid, inner
}
func verifyPeer(c *net.UnixConn, uid int) error {
	peer, err := peerUID(c)
	if err != nil || peer != uid {
		return fmt.Errorf("%w: local OS user does not own this runtime", ErrDenied)
	}
	return nil
}

func (v *Vault) read(name string, max int64) ([]byte, error) {
	path := filepath.Join(v.Dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != v.UID || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > max {
		return nil, errors.New("unsafe private runtime file")
	}
	return os.ReadFile(path)
}
