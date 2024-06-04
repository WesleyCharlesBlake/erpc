package common

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/IGLOU-EU/go-wildcard/v2"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// HexToUint64 converts a hexadecimal string to its decimal representation as a string.
func HexToUint64(hexValue string) (uint64, error) {
	// Create a new big.Int
	n := new(big.Int)

	// Check if the string has the "0x" prefix and remove it
	if len(hexValue) >= 2 && hexValue[:2] == "0x" {
		hexValue = hexValue[2:]
	}

	// Set n to the value of the hex string
	_, ok := n.SetString(hexValue, 16)
	if !ok {
		return 0, fmt.Errorf("invalid hexadecimal string")
	}

	// Return the decimal representation
	return n.Uint64(), nil
}

type ContextKey string

func NormalizeHex(value interface{}) (string, error) {
	if bn, ok := value.(string); ok {
		// Check if blockNumber is already in hex format
		if strings.HasPrefix(bn, "0x") {
			// Convert hex to integer to remove 0 padding
			value, err := hexutil.DecodeUint64(bn)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("0x%x", value), nil // Convert back to hex without leading 0s
		}

		// If blockNumber is base 10 digits, convert to hex without 0 padding
		value, err := strconv.ParseUint(bn, 10, 64)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("0x%x", value), nil
	}

	// If blockNumber is a number, convert to hex
	switch v := value.(type) {
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("0x%x", v), nil
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("0x%x", v), nil
	default:
		return "", fmt.Errorf("value is not a string or number: %+v", v)
	}
}

func WildcardMatch(pattern, value string) bool {
	if pattern == "*" {
		return true
	}

	return wildcard.Match(value, pattern)
}
