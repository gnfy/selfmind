package tools

import "strings"

// envSplitEscapes are the -S escapes that stand for a control character.
var envSplitEscapes = map[byte]byte{'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v'}

// splitEnvString splits an env -S string into the arguments env reads in its
// place, as FreeBSD and macOS env(1) and GNU env do. It is not a shell
// script: blanks separate words; single and double quotes group them; an
// argument that would begin with # ends the string. Outside single quotes a
// backslash escapes — \_ separates words outside quotes and is a blank inside
// double quotes, \c ends the string, and \f \n \r \t \v \# \$ \" \' \\ stand for
// their character — while inside single quotes only \' and \\ do. ${NAME} is
// replaced with the variable's value when env runs; it is kept as written and
// reported in substituted. ok is false where env refuses the string: an
// unterminated quote, an unknown escape, \c inside double quotes, or any other
// $.
func splitEnvString(s string) (words, substituted []string, ok bool) {
	var word strings.Builder
	inWord := false
	var quote byte
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			switch {
			case c == '\'':
				quote = 0
			case c == '\\' && i+1 < len(s) && (s[i+1] == '\'' || s[i+1] == '\\'):
				i++
				word.WriteByte(s[i])
			default:
				word.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			if quote == 0 {
				flush()
				continue
			}
			word.WriteByte(c)
		case '#':
			if quote == 0 && !inWord {
				return words, substituted, true
			}
			word.WriteByte(c)
		case '\'':
			if quote == 0 {
				quote, inWord = '\'', true
				continue
			}
			word.WriteByte(c)
		case '"':
			if quote == '"' {
				quote = 0
				continue
			}
			quote, inWord = '"', true
		case '\\':
			if i++; i == len(s) {
				return nil, nil, false
			}
			switch escape := s[i]; {
			case escape == 'c' && quote == 0:
				flush()
				return words, substituted, true
			case escape == '_' && quote == 0:
				flush()
			case escape == '_':
				word.WriteByte(' ')
			case envSplitEscapes[escape] != 0:
				word.WriteByte(envSplitEscapes[escape])
				inWord = true
			case strings.IndexByte(`#$"'\`, escape) >= 0:
				word.WriteByte(escape)
				inWord = true
			default:
				return nil, nil, false
			}
		case '$':
			reference, ok := envVariableReference(s[i:])
			if !ok {
				return nil, nil, false
			}
			word.WriteString(reference)
			substituted = append(substituted, reference)
			inWord = true
			i += len(reference) - 1
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, nil, false
	}
	flush()
	return words, substituted, true
}

// envVariableReference returns the ${NAME} reference s begins with.
func envVariableReference(s string) (string, bool) {
	end := strings.IndexByte(s, '}')
	if !strings.HasPrefix(s, "${") || end < 3 {
		return "", false
	}
	for i, r := range s[2:end] {
		letter := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return "", false
		}
	}
	return s[:end+1], true
}
