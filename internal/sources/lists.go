package sources

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tokens/internal/evm"
)

const (
	listName = "token_list"
	// maxText is the most bytes of a name or a symbol the engine keeps, as for a contract's own
	maxText = 128
	maxLogo = 512
)

// ListToken is one entry of a token list in the Uniswap token list format
type ListToken struct {
	ChainID  uint64
	Address  evm.Address
	Name     string
	Symbol   string
	Decimals uint8
	// LogoURL is an https URL or empty
	LogoURL string
}

// TokenLists reads curated token lists from their public URLs
type TokenLists struct {
	hc *http.Client
}

func NewTokenLists(timeout time.Duration) *TokenLists {
	return &TokenLists{hc: newHTTPClient(timeout)}
}

// WithHTTPClient swaps the client, for tests
func (l *TokenLists) WithHTTPClient(hc *http.Client) *TokenLists {
	l.hc = hc
	return l
}

type rawList struct {
	Tokens []struct {
		ChainID  uint64  `json:"chainId"`
		Address  string  `json:"address"`
		Name     string  `json:"name"`
		Symbol   string  `json:"symbol"`
		Decimals *int    `json:"decimals"`
		LogoURI  *string `json:"logoURI"`
	} `json:"tokens"`
}

// Fetch reads one list. An entry with a bad address, the zero address (the native coin) or no symbol is dropped,
// and a list with no usable entry is an error so that the caller keeps the copy it has
func (l *TokenLists) Fetch(ctx context.Context, rawURL string) ([]ListToken, error) {
	var raw rawList
	if err := getJSON(ctx, l.hc, listName, rawURL, &raw); err != nil {
		return nil, err
	}
	out := make([]ListToken, 0, len(raw.Tokens))
	for _, t := range raw.Tokens {
		addr, err := evm.ParseAddress(t.Address)
		symbol := CleanText(t.Symbol)
		if err != nil || addr == (evm.Address{}) || symbol == "" || t.Decimals == nil || *t.Decimals < 0 || *t.Decimals > 255 {
			continue
		}
		lt := ListToken{ChainID: t.ChainID, Address: addr, Name: CleanText(t.Name), Symbol: symbol, Decimals: uint8(*t.Decimals)}
		if t.LogoURI != nil {
			lt.LogoURL = HTTPSURL(*t.LogoURI)
		}
		out = append(out, lt)
	}
	if len(out) == 0 {
		return nil, &UnavailableError{Source: listName, Err: errors.New("the list holds no usable token")}
	}
	return out, nil
}

// HTTPSURL returns s when it is an https URL without credentials, and an empty string for anything else
func HTTPSURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxLogo {
		return ""
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return s
}

// CleanText drops control and format characters (bidi overrides, zero-width marks) and invalid UTF-8 and keeps at most 128 bytes
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > maxText {
		s = s[:maxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
