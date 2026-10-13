// Package evm holds the address, hash and ABI pieces that the engine needs to read ERC-20 tokens
package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type Address [20]byte

type Hash [32]byte

// ParseAddress takes 0x and 40 hex digits in any case
func ParseAddress(s string) (Address, error) {
	var a Address
	if err := parseHex(a[:], s); err != nil {
		return Address{}, fmt.Errorf("address: %w", err)
	}
	return a, nil
}

func ParseHash(s string) (Hash, error) {
	var h Hash
	if err := parseHex(h[:], s); err != nil {
		return Hash{}, fmt.Errorf("hash: %w", err)
	}
	return h, nil
}

func MustAddress(s string) Address {
	a, err := ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

func MustHash(s string) Hash {
	h, err := ParseHash(s)
	if err != nil {
		panic(err)
	}
	return h
}

func (a Address) String() string { return "0x" + hex.EncodeToString(a[:]) }

func (h Hash) String() string { return "0x" + hex.EncodeToString(h[:]) }

func (a Address) IsZero() bool { return a == Address{} }

func (a Address) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (a *Address) UnmarshalText(text []byte) error {
	parsed, err := ParseAddress(string(text))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := ParseHash(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// TopicAddress reads an indexed address topic, which must have 12 zero bytes before the address
func TopicAddress(t Hash) (Address, bool) {
	for _, b := range t[:12] {
		if b != 0 {
			return Address{}, false
		}
	}
	return Address(t[12:]), true
}

func parseHex(dst []byte, s string) error {
	body, ok := strings.CutPrefix(s, "0x")
	if !ok {
		body, ok = strings.CutPrefix(s, "0X")
	}
	if !ok {
		return fmt.Errorf("%q has no 0x prefix", s)
	}
	if len(body) != 2*len(dst) {
		return fmt.Errorf("%q is not %d hex digits", s, 2*len(dst))
	}
	if _, err := hex.Decode(dst, []byte(body)); err != nil {
		return fmt.Errorf("%q: %w", s, err)
	}
	return nil
}

// DecodeHex reads 0x-prefixed hex data of any even length
func DecodeHex(s string) ([]byte, error) {
	body, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return nil, errors.New("hex data has no 0x prefix")
	}
	out, err := hex.DecodeString(body)
	if err != nil {
		return nil, errors.New("hex data is malformed")
	}
	return out, nil
}
