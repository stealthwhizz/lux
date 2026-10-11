// Package connectors links people's outside accounts (Google...) to Lux and
// performs the actions their agents ask for, so tokens stay inside luxd.
package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Box encrypts tokens at rest with AES-256-GCM.
type Box struct{ aead cipher.AEAD }

// LoadKey reads the key from LUX_SECRET_KEY (base64, 32 bytes) or, if that
// is unset, from <dataDir>/secret.key, creating it on first use.
func LoadKey(dataDir string) (*Box, error) {
	raw := strings.TrimSpace(os.Getenv("LUX_SECRET_KEY"))
	if raw == "" {
		path := filepath.Join(dataDir, "secret.key")
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			raw = strings.TrimSpace(string(b))
		case errors.Is(err, os.ErrNotExist):
			k := make([]byte, 32)
			if _, err := rand.Read(k); err != nil {
				return nil, err
			}
			raw = base64.StdEncoding.EncodeToString(k)
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, fmt.Errorf("create %s: %w", path, err)
			}
			_, werr := f.WriteString(raw + "\n")
			if cerr := f.Close(); werr != nil || cerr != nil {
				return nil, fmt.Errorf("write %s", path)
			}
		default:
			return nil, err
		}
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("LUX_SECRET_KEY must be 32 bytes, base64-encoded (openssl rand -base64 32)")
	}
	return NewBox(key)
}

func NewBox(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead}, nil
}

// Seal encrypts; the associated data binds a token to its owner and provider.
func (b *Box) Seal(plain, ad []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // the system's randomness source failed
	}
	return b.aead.Seal(nonce, nonce, plain, ad)
}

func (b *Box) Open(sealed, ad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed data is too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], ad)
}
