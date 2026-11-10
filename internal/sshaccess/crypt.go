package sshaccess

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"strings"
)

// HashPassword returns the password as a "$6$" (SHA-512 crypt) string, the form
// /etc/shadow holds and glibc's crypt() checks. Go's standard library has no
// crypt(3), and the appliance has no tool that would make one, so this is the
// algorithm of https://www.akkadia.org/drepper/SHA-crypt.txt with the default
// 5000 rounds and a random 16-character salt.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("empty password")
	}
	salt, err := randomSalt(16)
	if err != nil {
		return "", err
	}
	return sha512Crypt([]byte(password), []byte(salt), defaultRounds), nil
}

const (
	defaultRounds = 5000
	cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

func randomSalt(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(cryptAlphabet[int(c)%len(cryptAlphabet)])
	}
	return sb.String(), nil
}

// sha512Crypt computes the crypt string for password and salt (at most 16
// bytes are used). rounds == defaultRounds leaves the "rounds=" part out, as
// the specification asks.
func sha512Crypt(password, salt []byte, rounds int) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	sum := func(parts ...[]byte) []byte {
		h := sha512.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}

	// Digest B, then digest A: password, salt, then B in password-length
	// chunks and the bits of the password length.
	b := sum(password, salt, password)
	ctx := sha512.New()
	ctx.Write(password)
	ctx.Write(salt)
	for n := len(password); n > 0; n -= 64 {
		if n >= 64 {
			ctx.Write(b)
		} else {
			ctx.Write(b[:n])
		}
	}
	for n := len(password); n > 0; n >>= 1 {
		if n&1 != 0 {
			ctx.Write(b)
		} else {
			ctx.Write(password)
		}
	}
	a := ctx.Sum(nil)

	// Byte sequences P (from the password) and S (from the salt).
	dpCtx := sha512.New()
	for i := 0; i < len(password); i++ {
		dpCtx.Write(password)
	}
	p := repeatTo(dpCtx.Sum(nil), len(password))
	dsCtx := sha512.New()
	for i := 0; i < 16+int(a[0]); i++ {
		dsCtx.Write(salt)
	}
	s := repeatTo(dsCtx.Sum(nil), len(salt))

	for i := 0; i < rounds; i++ {
		h := sha512.New()
		if i&1 != 0 {
			h.Write(p)
		} else {
			h.Write(a)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 != 0 {
			h.Write(a)
		} else {
			h.Write(p)
		}
		a = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	if rounds != defaultRounds {
		out.WriteString("rounds=")
		out.WriteString(itoa(rounds))
		out.WriteByte('$')
	}
	out.Write(salt)
	out.WriteByte('$')
	// The digest is written in this byte order, three bytes to four characters,
	// least significant six bits first.
	for _, t := range [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	} {
		v := uint(a[t[0]])<<16 | uint(a[t[1]])<<8 | uint(a[t[2]])
		for k := 0; k < 4; k++ {
			out.WriteByte(cryptAlphabet[v&0x3f])
			v >>= 6
		}
	}
	v := uint(a[63])
	out.WriteByte(cryptAlphabet[v&0x3f])
	out.WriteByte(cryptAlphabet[v>>6])
	return out.String()
}

func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		take := n - len(out)
		if take > len(src) {
			take = len(src)
		}
		out = append(out, src[:take]...)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
