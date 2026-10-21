package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

// Argon2id parameters match argon2-cffi's PasswordHasher defaults so hashes
// written by the Python service verify unchanged and vice versa.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 65536 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

var (
	dummyOnce sync.Once
	dummyHash string
)

// HashPassword returns a PHC-encoded argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
	return encoded, nil
}

// VerifyPassword reports whether password matches an argon2id PHC string.
// Unsupported or malformed hashes return false rather than panicking.
func VerifyPassword(password, encoded string) bool {
	params, salt, expected, err := parseArgon2(encoded)
	if err != nil {
		return false
	}
	key := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(key, expected) == 1
}

// DummyHash returns a stable hash used to equalize failed-login timing.
func DummyHash() string {
	dummyOnce.Do(func() {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			secret = []byte("music-transcription-dummy-password")
		}
		hash, err := HashPassword(base64.RawStdEncoding.EncodeToString(secret))
		if err != nil {
			hash = "$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcA"
		}
		dummyHash = hash
	})
	return dummyHash
}

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
}

func parseArgon2(encoded string) (argonParams, []byte, []byte, error) {
	var params argonParams
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=65536,t=3,p=4", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return params, nil, nil, errors.New("unsupported password hash")
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return params, nil, nil, errors.New("unsupported argon2 version")
	}
	for _, field := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) != 2 {
			return params, nil, nil, errors.New("invalid argon2 parameters")
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return params, nil, nil, errors.New("invalid argon2 parameter value")
		}
		switch pair[0] {
		case "m":
			params.memory = uint32(value)
		case "t":
			params.time = uint32(value)
		case "p":
			params.threads = uint8(value)
		}
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return params, nil, nil, errors.New("incomplete argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, err
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params, nil, nil, err
	}
	if len(salt) == 0 || len(expected) == 0 {
		return params, nil, nil, errors.New("empty argon2 fields")
	}
	return params, salt, expected, nil
}

// NormalizeUsername applies Unicode NFKC, trims, casefolds and validates,
// matching the Python service (NFKC + strip + str.casefold).
func NormalizeUsername(username string) (string, error) {
	normalized := cases.Fold().String(strings.TrimSpace(norm.NFKC.String(username)))
	normalized = norm.NFKC.String(normalized)
	if !validUsername(normalized) {
		return "", NewError("INVALID_USERNAME", "用户名需为3–64个字母、数字、下划线、点或连字符", 400)
	}
	return normalized, nil
}

func validUsername(value string) bool {
	runes := []rune(value)
	if len(runes) < 3 || len(runes) > 64 {
		return false
	}
	for _, r := range runes {
		if r == '_' || r == '.' || r == '-' {
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsNumber(r) {
			return false
		}
	}
	return true
}

// ValidatePassword enforces the 12–128 character policy.
func ValidatePassword(password string) error {
	length := len([]rune(password))
	if length < 12 || length > 128 {
		return NewError("INVALID_PASSWORD", "密码长度须为12–128个字符", 400)
	}
	return nil
}

// Digest returns the lowercase SHA256 hex digest of a session token.
func Digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CookieName is the session cookie name shared with the legacy service.
const CookieName = "music_session"

// SessionToken returns a cryptographically random URL-safe session token.
func SessionToken() (string, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// InitAdmin creates the one-time administrator account. It never overwrites an
// existing username.
func InitAdmin(store *Store, username, password string) (string, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return "", err
	}
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	var existing int64
	if err := store.DB.Model(&User{}).Where("username = ?", normalized).Count(&existing).Error; err != nil {
		return "", err
	}
	if existing > 0 {
		return "", errors.New("Username already exists; no account was changed")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	user := User{
		ID:           NewID(),
		Username:     normalized,
		PasswordHash: hash,
		Role:         "admin",
		Status:       "active",
		CreatedAt:    Now(),
	}
	err = store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return Audit(tx, &user.ID, "cli.admin.created", &user.ID, nil)
	})
	if err != nil {
		return "", err
	}
	return user.ID, nil
}
