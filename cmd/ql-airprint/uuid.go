package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const seedFile = "printer-seed"

// stableUUID derives the printer UUID from a random seed kept in dir, so clients keep
// recognising the same printer across restarts and upgrades.
func stableUUID(dir string) (string, error) {
	path := filepath.Join(dir, seedFile)
	data, err := os.ReadFile(path)
	var seed []byte
	switch {
	case err == nil:
		seed, err = hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != 32 {
			return "", fmt.Errorf("%s: corrupt seed; delete it to make a new printer identity", path)
		}
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	sum := sha256.Sum256(append([]byte("ql-airprint printer-uuid\x00"), seed...))
	u := sum[:16]
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}
