package interbase

import (
	"fmt"
	"strings"
)

func normalizeCatalogTextCharset(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "", "WIN1250", "WIN1252", "ISO8859_1", "ASCII":
		return value, nil
	default:
		return "", fmt.Errorf("interbase: unsupported catalog text charset %q", value)
	}
}

func catalogTextCharsetID(value string) (int16, error) {
	charset, err := normalizeCatalogTextCharset(value)
	if err != nil {
		return 0, err
	}
	switch charset {
	case "":
		return 0, nil
	case "ASCII":
		return 2, nil
	case "ISO8859_1":
		return 21, nil
	case "WIN1250":
		return 51, nil
	case "WIN1252":
		return 53, nil
	default:
		return 0, fmt.Errorf("interbase: unsupported catalog text charset %q", charset)
	}
}
