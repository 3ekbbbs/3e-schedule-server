package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------- ID / token ----------

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- password (stdlib PBKDF2-HMAC-SHA256, no external dep) ----------

const pbkdfIter = 120000

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	T := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		U = prf.Sum(U[:0])
		copy(T, U)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = prf.Sum(U[:0])
			for x := range T {
				T[x] ^= U[x]
			}
		}
		dk = append(dk, T...)
	}
	return dk[:keyLen]
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk := pbkdf2SHA256([]byte(pw), salt, pbkdfIter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdfIter, base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
}

func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return hmac.Equal(got, want)
}

// ---------- json helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// ---------- misc ----------

func atoi64(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// jsonMap is a loose map used for partial patches / field_seqs.
type jsonMap map[string]any

// int64Of reads an integral value out of a loose map.
//
// It deliberately accepts every numeric shape that can reach a map[string]any:
// json.Unmarshal produces float64, but middleware that rewrites the body in
// flight inserts Go ints, and json.Number shows up if a decoder ever switches
// to UseNumber. Accepting only float64 silently returned false for the int64
// case and dropped values — so the whole family is handled here once.
func (m jsonMap) int64Of(k string) (int64, bool) {
	f, ok := m[k]
	if !ok || f == nil {
		return 0, false
	}
	switch t := f.(type) {
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if v, err := t.Int64(); err == nil {
			return v, true
		}
		return 0, false
	case string:
		return atoi64(t)
	}
	return 0, false
}

func (m jsonMap) stringOf(k string) string {
	if f, ok := m[k]; ok && f != nil {
		if s, ok := f.(string); ok {
			return s
		}
	}
	return ""
}

func (m jsonMap) boolOf(k string) bool {
	if f, ok := m[k]; ok && f != nil {
		if b, ok := f.(bool); ok {
			return b
		}
	}
	return false
}
