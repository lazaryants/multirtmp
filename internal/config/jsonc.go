package config

// stripLineComments убирает `//`-комментарии из JSON, не трогая `//` внутри
// строковых значений (RTMP URL вида rtmp://... иначе были бы искалечены).
// Переносы строк не удаляются — только содержимое комментария заменяется
// пробелами, чтобы номера строк в итоговом тексте совпадали с исходным
// файлом (нужно для сообщений валидации "строка N").
func stripLineComments(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)

	inString := false
	escaped := false

	for i := 0; i < len(out); i++ {
		c := out[i]

		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch {
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		}
	}

	return out
}

// offsetToLine переводит байтовый offset (как в json.SyntaxError.Offset) в
// номер строки (с единицы), считая переводы строк в src[:offset].
func offsetToLine(src []byte, offset int64) int {
	if offset < 0 {
		return 0
	}
	if offset > int64(len(src)) {
		offset = int64(len(src))
	}

	line := 1
	for _, c := range src[:offset] {
		if c == '\n' {
			line++
		}
	}
	return line
}
