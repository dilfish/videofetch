package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

// cbEncrypt is the encryption counterpart of aesDecrypt: AES-128-CBC with
// PKCS7 padding, what AES-128 HLS segments use.
func cbEncrypt(t *testing.T, plaintext, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

var aesKey = []byte("0123456789abcdef") // 16 bytes: AES-128
var aesIV = []byte("fedcba9876543210")

func TestAesDecryptRoundTrip(t *testing.T) {
	for _, pt := range []string{"", "hi", "0123456789abcde", "exactly 16 bytes", "multi block payload > 16"} {
		ct := cbEncrypt(t, []byte(pt), aesKey, aesIV)
		got, err := aesDecrypt(ct, aesKey, aesIV)
		if err != nil {
			t.Fatalf("aesDecrypt(%q): %v", pt, err)
		}
		if string(got) != pt {
			t.Errorf("aesDecrypt round trip: got %q, want %q", got, pt)
		}
	}
}

func TestAesDecryptNoPadding(t *testing.T) {
	// ciphertext whose plaintext does not look like PKCS7 padding is
	// returned as-is, full length
	plain := []byte("0123456789abcdef") // 16 bytes
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, 16)
	cipher.NewCBCEncrypter(block, aesIV).CryptBlocks(ct, plain)
	got, err := aesDecrypt(ct, aesKey, aesIV)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("got %q, want unpadded %q", got, plain)
	}
}

func TestAesDecryptErrors(t *testing.T) {
	if _, err := aesDecrypt([]byte("short"), aesKey, aesIV); err == nil {
		t.Error("length not multiple of 16: want error, got nil")
	}
	if _, err := aesDecrypt(nil, aesKey, aesIV); err == nil {
		t.Error("empty data: want error, got nil")
	}
	if _, err := aesDecrypt(make([]byte, 16), []byte("too-short"), aesIV); err == nil {
		t.Error("bad key length: want error, got nil")
	}
}
