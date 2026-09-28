package propshaft

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// This scanner recognizes CSS URL functions and quoted @import operands. It is
// intentionally not a stylesheet validator or a general-purpose CSS parser.
func rewriteCSS(data []byte, rewrite func(string) (string, error)) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("stylesheets must be UTF-8")
	}
	s := string(data)
	var output strings.Builder
	copied := 0
	replace := func(start, end int, value string, function bool) error {
		rewritten, err := rewrite(value)
		if err != nil {
			return err
		}
		if rewritten == value {
			return nil
		}
		output.WriteString(s[copied:start])
		if function {
			output.WriteString("url(")
		}
		output.WriteByte('"')
		output.WriteString(quoteCSS(rewritten))
		output.WriteByte('"')
		if function {
			output.WriteByte(')')
		}
		copied = end
		return nil
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at byte %d", i)
			}
			i += end + 4
		case s[i] == '\'' || s[i] == '"':
			_, end, err := cssString(s, i)
			if err != nil {
				return nil, err
			}
			i = end
		case s[i] == '@':
			name, end, err := cssName(s, i+1)
			if err != nil {
				return nil, err
			}
			i = end
			if strings.EqualFold(name, "import") {
				i, err = cssTrivia(s, i)
				if err != nil {
					return nil, err
				}
				if i < len(s) && (s[i] == '\'' || s[i] == '"') {
					value, end, err := cssString(s, i)
					if err != nil {
						return nil, err
					}
					if err := replace(i, end, value, false); err != nil {
						return nil, err
					}
					i = end
				}
			}
		case cssNameByte(s[i]) || s[i] == '\\':
			start := i
			name, end, err := cssName(s, i)
			if err != nil {
				return nil, err
			}
			i = end
			if strings.EqualFold(name, "url") && i < len(s) && s[i] == '(' {
				value, end, err := cssURL(s, i+1)
				if err != nil {
					return nil, err
				}
				if err := replace(start, end, value, true); err != nil {
					return nil, err
				}
				i = end
			}
		default:
			i++
		}
	}
	if copied == 0 {
		return data, nil
	}
	output.WriteString(s[copied:])
	return []byte(output.String()), nil
}

func cssNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c >= 0x80
}

func cssSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func cssName(s string, i int) (string, int, error) {
	var name strings.Builder
	for i < len(s) {
		if s[i] == '\\' {
			r, end, err := cssEscape(s, i, false)
			if err != nil {
				return "", 0, err
			}
			name.WriteRune(r)
			i = end
		} else if cssNameByte(s[i]) {
			name.WriteByte(s[i])
			i++
		} else {
			break
		}
	}
	return name.String(), i, nil
}

func cssTrivia(s string, i int) (int, error) {
	for i < len(s) {
		if cssSpace(s[i]) {
			i++
		} else if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return 0, fmt.Errorf("unterminated comment at byte %d", i)
			}
			i += end + 4
		} else {
			break
		}
	}
	return i, nil
}

func cssString(s string, start int) (string, int, error) {
	quote := s[start]
	var value strings.Builder
	for i := start + 1; i < len(s); {
		switch s[i] {
		case quote:
			return value.String(), i + 1, nil
		case '\\':
			r, end, err := cssEscape(s, i, true)
			if err != nil {
				return "", 0, err
			}
			if r != -1 {
				value.WriteRune(r)
			}
			i = end
		case '\n', '\r', '\f':
			return "", 0, fmt.Errorf("unescaped newline in CSS string at byte %d", i)
		case 0:
			value.WriteRune(utf8.RuneError)
			i++
		default:
			value.WriteByte(s[i])
			i++
		}
	}
	return "", 0, fmt.Errorf("unterminated CSS string at byte %d", start)
}

func cssURL(s string, i int) (string, int, error) {
	for i < len(s) && cssSpace(s[i]) {
		i++
	}
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		value, end, err := cssString(s, i)
		if err != nil {
			return "", 0, err
		}
		end, err = cssTrivia(s, end)
		if err != nil {
			return "", 0, err
		}
		if end >= len(s) || s[end] != ')' {
			return "", 0, fmt.Errorf("expected closing parenthesis after CSS URL at byte %d", end)
		}
		return value, end + 1, nil
	}
	var value strings.Builder
	for i < len(s) {
		switch {
		case s[i] == ')':
			return value.String(), i + 1, nil
		case s[i] == '\\':
			r, end, err := cssEscape(s, i, false)
			if err != nil {
				return "", 0, err
			}
			value.WriteRune(r)
			i = end
		case cssSpace(s[i]):
			for i < len(s) && cssSpace(s[i]) {
				i++
			}
			if i < len(s) && s[i] == ')' {
				return value.String(), i + 1, nil
			}
			return "", 0, fmt.Errorf("whitespace inside unquoted CSS URL at byte %d", i)
		case s[i] == '(' || s[i] == '\'' || s[i] == '"' || s[i] < 0x20 || s[i] == 0x7f:
			return "", 0, fmt.Errorf("invalid unquoted CSS URL at byte %d", i)
		default:
			value.WriteByte(s[i])
			i++
		}
	}
	return "", 0, fmt.Errorf("unterminated CSS URL")
}

// cssEscape consumes a CSS escape, including the optional whitespace after a
// hexadecimal escape. -1 denotes a string line continuation.
func cssEscape(s string, start int, continuation bool) (rune, int, error) {
	i := start + 1
	if i >= len(s) {
		return 0, 0, fmt.Errorf("incomplete CSS escape at byte %d", start)
	}
	if s[i] == '\n' || s[i] == '\r' || s[i] == '\f' {
		if !continuation {
			return 0, 0, fmt.Errorf("invalid CSS escape at byte %d", start)
		}
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
		}
		return -1, i + 1, nil
	}
	var value rune
	count := 0
	for i < len(s) && count < 6 {
		var digit byte
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digit = c - '0'
		case c >= 'a' && c <= 'f':
			digit = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			digit = c - 'A' + 10
		default:
			goto decoded
		}
		value = value*16 + rune(digit)
		i++
		count++
	}
decoded:
	if count == 0 {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0 {
			r = utf8.RuneError
		}
		return r, i + size, nil
	}
	if i < len(s) && cssSpace(s[i]) {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
		}
		i++
	}
	if value == 0 || !utf8.ValidRune(value) {
		value = utf8.RuneError
	}
	return value, i, nil
}

func quoteCSS(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '"':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&out, "\\%x ", r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
