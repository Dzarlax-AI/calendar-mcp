package credentials

import (
	"encoding/base64"
	"testing"
)

func testKey(fill byte) string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestCipherRoundTrip(t *testing.T) {
	cipher, err := NewCipher(testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"refresh_token":"secret"}`)
	aad := AssociatedData("connection-1", "google")
	envelope, err := cipher.Encrypt(want, aad)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cipher.Decrypt(envelope, aad)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("Decrypt() = %q, want %q", got, want)
	}
}

func TestCipherRejectsTamperingWrongContextAndWrongKey(t *testing.T) {
	cipher, _ := NewCipher(testKey(1))
	envelope, err := cipher.Encrypt([]byte("secret"), AssociatedData("one", "google"))
	if err != nil {
		t.Fatal(err)
	}

	tampered := append([]byte(nil), envelope...)
	tampered[len(tampered)-1] ^= 1
	if _, err := cipher.Decrypt(tampered, AssociatedData("one", "google")); err == nil {
		t.Fatal("tampered envelope decrypted successfully")
	}
	if _, err := cipher.Decrypt(envelope, AssociatedData("two", "google")); err == nil {
		t.Fatal("envelope decrypted with wrong associated data")
	}
	other, _ := NewCipher(testKey(2))
	if _, err := other.Decrypt(envelope, AssociatedData("one", "google")); err == nil {
		t.Fatal("envelope decrypted with wrong key")
	}
}

func TestCipherRejectsInvalidKeysAndVersions(t *testing.T) {
	for _, key := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := NewCipher(key); err == nil {
			t.Fatalf("NewCipher(%q) succeeded", key)
		}
	}

	cipher, _ := NewCipher(testKey(1))
	envelope, _ := cipher.Encrypt([]byte("secret"), nil)
	envelope[0]++
	if _, err := cipher.Decrypt(envelope, nil); err == nil {
		t.Fatal("unsupported envelope version decrypted successfully")
	}
}
