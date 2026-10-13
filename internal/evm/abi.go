package evm

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"
)

const wordSize = 32

// MaxText is the longest name or symbol kept, in bytes, so a hostile contract cannot fill a record
const MaxText = 128

// Selectors and topics of the ERC-20 calls and events the engine uses, the keccak hashes of their signatures
var (
	SelName        = [4]byte{0x06, 0xfd, 0xde, 0x03}
	SelSymbol      = [4]byte{0x95, 0xd8, 0x9b, 0x41}
	SelDecimals    = [4]byte{0x31, 0x3c, 0xe5, 0x67}
	SelTotalSupply = [4]byte{0x18, 0x16, 0x0d, 0xdd}
	SelBalanceOf   = [4]byte{0x70, 0xa0, 0x82, 0x31}
	selAggregate3  = [4]byte{0x82, 0xad, 0x56, 0xcb}

	// TopicTransfer is Transfer(address,address,uint256)
	TopicTransfer = MustHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	// TopicDeposit is the WETH9 Deposit(address,uint256), a mint of wrapped native coin
	TopicDeposit = MustHash("0xe1fffcc4923d04b559f4d29a8bfc6cda04eb5b0d3c460751c2402c5c5cc9109c")
	// TopicWithdrawal is the WETH9 Withdrawal(address,uint256), a burn of wrapped native coin
	TopicWithdrawal = MustHash("0x7fcf532c15f0a6db0bd6d0e038bea71d30d808c7d98cb3bf7268a95bf5081b65")
)

var ErrShort = errors.New("return data is shorter than one word")

// BalanceOfCall is the calldata of balanceOf(holder)
func BalanceOfCall(holder Address) []byte {
	out := make([]byte, 4+wordSize)
	copy(out, SelBalanceOf[:])
	copy(out[4+12:], holder[:])
	return out
}

// Uint256 reads the first word of return data
func Uint256(data []byte) (*big.Int, error) {
	if len(data) < wordSize {
		return nil, ErrShort
	}
	return new(big.Int).SetBytes(data[:wordSize]), nil
}

// Decimals reads a uint8 answer, which must fit one byte
func Decimals(data []byte) (uint8, error) {
	n, err := Uint256(data)
	if err != nil {
		return 0, err
	}
	if !n.IsUint64() || n.Uint64() > 255 {
		return 0, fmt.Errorf("decimals %s does not fit uint8", n)
	}
	return uint8(n.Uint64()), nil
}

// Text reads a name or symbol, which is an ABI string in most tokens and a bytes32 in some old ones such as MKR
func Text(data []byte) (string, error) {
	var raw []byte
	switch {
	case len(data) == wordSize:
		raw = data
	case len(data) >= 2*wordSize:
		r := reader{data}
		off, err := r.uint64(0)
		if err != nil || off%wordSize != 0 {
			return "", errors.New("string offset is malformed")
		}
		lenAt := int(off / wordSize)
		n, err := r.uint64(lenAt)
		if err != nil {
			return "", fmt.Errorf("string length: %w", err)
		}
		start := (lenAt + 1) * wordSize
		if n > uint64(len(data)-start) {
			return "", errors.New("string runs past the data")
		}
		raw = data[start : start+int(n)]
	default:
		return "", ErrShort
	}
	return cleanText(raw), nil
}

// cleanText drops NUL padding, invalid UTF-8 and control characters and caps the length
func cleanText(raw []byte) string {
	s := strings.ToValidUTF8(string(raw), "")
	s = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	for len(s) > MaxText {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

/* Words */

// reader walks the 32-byte words of one ABI blob and range checks every value it narrows
type reader struct {
	data []byte
}

func (r reader) word(index int) ([]byte, error) {
	start := index * wordSize
	if index < 0 || start+wordSize > len(r.data) {
		return nil, fmt.Errorf("word %d is outside %d bytes of data", index, len(r.data))
	}
	return r.data[start : start+wordSize], nil
}

func (r reader) uint64(index int) (uint64, error) {
	w, err := r.word(index)
	if err != nil {
		return 0, err
	}
	for _, b := range w[:wordSize-8] {
		if b != 0 {
			return 0, fmt.Errorf("word %d does not fit uint64", index)
		}
	}
	var v uint64
	for _, b := range w[wordSize-8:] {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

func (r reader) bool(index int) (bool, error) {
	v, err := r.uint64(index)
	if err != nil {
		return false, err
	}
	if v > 1 {
		return false, fmt.Errorf("word %d is not a bool", index)
	}
	return v == 1, nil
}

// offset reads a byte offset that must land on a word
func (r reader) offset(index int) (int, error) {
	v, err := r.uint64(index)
	if err != nil {
		return 0, err
	}
	if v%wordSize != 0 || v > uint64(len(r.data)) {
		return 0, fmt.Errorf("word %d is not a word offset", index)
	}
	return int(v / wordSize), nil
}

func putUint64(buf []byte, v uint64) []byte {
	var w [wordSize]byte
	for i := range 8 {
		w[wordSize-1-i] = byte(v >> (8 * i))
	}
	return append(buf, w[:]...)
}

func paddedLen(n int) int { return (n + wordSize - 1) / wordSize * wordSize }
