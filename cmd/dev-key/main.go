// Command dev-key creates local signing or AES keys without platform OpenSSL.
// It is a development helper and is not included in the service image.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	create := generate
	if len(args) == 2 && args[0] == "--aes" {
		create = generateAES
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: dev-key [--aes] PATH")
		os.Exit(2)
	}
	if err := create(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(path string) error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	return writeExclusive(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func generateAES(path string) error {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	return writeExclusive(path, key[:])
}

func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Local operator-supplied output path; exclusive creation never overwrites a key.
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
