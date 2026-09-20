package yandex

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateSSHKeyPair(t *testing.T) {
	pub, priv, err := generateSSHKeyPair()
	if err != nil {
		t.Fatalf("generateSSHKeyPair() = %v", err)
	}

	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Fatalf("public key = %q, want ssh-ed25519 prefix", pub)
	}

	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("ssh.ParsePrivateKey() = %v", err)
	}

	derivedPub := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if !bytes.Equal(bytes.TrimSpace(derivedPub), bytes.TrimSpace(pub)) {
		t.Fatalf("private key does not match generated public key")
	}
}

func TestPublicKeyFromPrivate(t *testing.T) {
	wantPub, priv, err := generateSSHKeyPair()
	if err != nil {
		t.Fatalf("generateSSHKeyPair() = %v", err)
	}

	gotPub, err := publicKeyFromPrivate(priv)
	if err != nil {
		t.Fatalf("publicKeyFromPrivate() = %v", err)
	}

	if !bytes.Equal(bytes.TrimSpace(gotPub), bytes.TrimSpace(wantPub)) {
		t.Fatalf("publicKeyFromPrivate() = %q, want %q", gotPub, wantPub)
	}
}

func TestPublicKeyFromPrivateInvalidKey(t *testing.T) {
	if _, err := publicKeyFromPrivate([]byte("not a key")); err == nil {
		t.Fatal("publicKeyFromPrivate() = nil error, want error for invalid key")
	}
}
