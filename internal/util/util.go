// Package util holds the tiny cross-domain helpers every package leans on:
// timestamps, pointer constructors, random ids, and process-scoped paths.
package util

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"
)

// NowISO matches JS Date.toISOString(): UTC, millisecond precision.
func NowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func IntPtr(i int) *int {
	return &i
}

// RandomID returns n hex characters from crypto/rand (TS: randomUUID().slice(0, n)).
func RandomID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)[:n]
}

func MustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return h
}

func MustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}

func PathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// FirstRunes truncates s to at most n runes.
func FirstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
