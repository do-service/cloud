package wopi

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
)

// WOPI sends file names in headers as UTF-7 (RFC 2152): ASCII as is, the
// rest as "+<base64 of UTF-16BE>-", and "+-" for a plus sign.

var errUTF7 = errors.New("wopi: malformed UTF-7")

var utf7Base64 = base64.RawStdEncoding

func decodeUTF7(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			return "", errUTF7
		}
		if c != '+' {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(s) && isBase64(s[j]) {
			j++
		}
		if j == i+1 {
			// "+-" is a plus; a "+" before anything else is malformed.
			if j < len(s) && s[j] == '-' {
				b.WriteByte('+')
				i = j
				continue
			}
			return "", errUTF7
		}
		// Unpadded; bits left over past the last whole byte are zero.
		raw, err := utf7Base64.DecodeString(s[i+1 : j])
		if err != nil || len(raw)%2 != 0 {
			return "", errUTF7
		}
		units := make([]uint16, len(raw)/2)
		for k := range units {
			units[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		b.WriteString(string(utf16.Decode(units)))
		// A "-" ending the run is absorbed.
		if j < len(s) && s[j] == '-' {
			j++
		}
		i = j - 1
	}
	return b.String(), nil
}

func isBase64(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/'
}
