// Package auth manages cluster users and revocable, opaque browser sessions.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"
	"sluicework/internal/state"
)

var (
	ErrLogin   = errors.New("invalid username or password")
	ErrSession = errors.New("sign in to continue")
	ErrExists  = errors.New("cluster already initialized")
	validName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,79}$`)
)

const Lifetime = 12 * time.Hour

type User struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}
type Service struct {
	db    *sql.DB
	dummy []byte
}

func New(db *sql.DB) (*Service, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS auth_users(username TEXT PRIMARY KEY, role TEXT NOT NULL CHECK(role IN ('operator','admin')), passhash BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS auth_sessions(digest BLOB PRIMARY KEY,username TEXT NOT NULL REFERENCES auth_users(username) ON DELETE CASCADE, expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS auth_sessions_expiry ON auth_sessions(expires);`)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("unused timing comparison"), bcrypt.DefaultCost)
	return &Service{db: db, dummy: hash}, err
}

// SetUser revokes existing sessions when replacing a local user's role or password.
// firstOnly makes bootstrap safe to retry against an already initialized cluster.
func (s *Service) SetUser(name, role, password string, firstOnly bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = SetUserTx(tx, name, role, password, firstOnly); err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserTx allows cluster bootstrap to commit the administrator and initial
// approved template together.
func SetUserTx(tx *sql.Tx, name, role, password string, firstOnly bool) error {
	if !validName.MatchString(name) || (role != "admin" && role != "operator") {
		return errors.New("invalid username or role")
	}
	if len(password) < 12 || len(password) > 72 {
		return errors.New("password must be 12 to 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if firstOnly {
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM auth_users").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrExists
		}
	}
	_, err = tx.Exec(`INSERT INTO auth_users VALUES(?,?,?) ON CONFLICT(username) DO UPDATE SET role=excluded.role,passhash=excluded.passhash`, name, role, hash)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM auth_sessions WHERE username=?", name); err != nil {
		return err
	}
	if err = state.AuditTx(tx, "local administrator", "user.set", fmt.Sprintf("%s (%s)", name, role)); err != nil {
		return err
	}
	return nil
}
func (s *Service) Login(name, password string) (User, string, error) {
	var user User
	var hash []byte
	err := s.db.QueryRow("SELECT username,role,passhash FROM auth_users WHERE username=?", name).Scan(&user.Username, &user.Role, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return User{}, "", err
	}
	if hash == nil {
		hash = s.dummy
	}
	if check := bcrypt.CompareHashAndPassword(hash, []byte(password)); check != nil || err != nil {
		return User{}, "", ErrLogin
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return User{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	// Guard against a password/role change between the comparison and insertion.
	if _, err = s.db.Exec("DELETE FROM auth_sessions WHERE expires<=?", time.Now().Unix()); err != nil {
		return User{}, "", err
	}
	res, err := s.db.Exec(`INSERT INTO auth_sessions(digest,username,expires) SELECT ?,username,? FROM auth_users WHERE username=? AND passhash=?`, digest[:], time.Now().Add(Lifetime).Unix(), name, hash)
	if err != nil {
		return User{}, "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return User{}, "", ErrLogin
	}
	return user, token, nil
}
func (s *Service) Lookup(token string) (User, error) {
	var u User
	d := sha256.Sum256([]byte(token))
	err := s.db.QueryRow(`SELECT u.username,u.role FROM auth_users u JOIN auth_sessions s USING(username) WHERE s.digest=? AND s.expires>?`, d[:], time.Now().Unix()).Scan(&u.Username, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrSession
	}
	return u, err
}
func (s *Service) Logout(token string) error {
	d := sha256.Sum256([]byte(token))
	_, err := s.db.Exec("DELETE FROM auth_sessions WHERE digest=?", d[:])
	return err
}
