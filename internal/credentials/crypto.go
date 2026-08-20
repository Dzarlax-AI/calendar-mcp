package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const envelopeVersion byte = 1

type Cipher struct {
	aead cipher.AEAD
}

func NewCipher(encodedKey string) (*Cipher, error) {
	if encodedKey == "" {
		return nil, errors.New("CALENDAR_ENCRYPTION_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("decode CALENDAR_ENCRYPTION_KEY: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("CALENDAR_ENCRYPTION_KEY must decode to exactly 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

func AssociatedData(connectionID, provider string) []byte {
	return []byte("calendar-connection\x00" + connectionID + "\x00" + provider)
}

func (c *Cipher) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	envelope := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	envelope[0] = envelopeVersion
	envelope = append(envelope, nonce...)
	envelope = c.aead.Seal(envelope, nonce, plaintext, associatedData)
	return envelope, nil
}

func (c *Cipher) Decrypt(envelope, associatedData []byte) ([]byte, error) {
	if len(envelope) == 0 || envelope[0] != envelopeVersion {
		return nil, errors.New("unsupported credential envelope version")
	}
	nonceSize := c.aead.NonceSize()
	if len(envelope) < 1+nonceSize+c.aead.Overhead() {
		return nil, errors.New("invalid credential envelope")
	}
	nonce := envelope[1 : 1+nonceSize]
	plaintext, err := c.aead.Open(nil, nonce, envelope[1+nonceSize:], associatedData)
	if err != nil {
		return nil, errors.New("decrypt credentials: authentication failed")
	}
	return plaintext, nil
}
