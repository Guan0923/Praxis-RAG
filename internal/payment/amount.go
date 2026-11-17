package payment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidAmount reports a malformed or unsafe money value.
var ErrInvalidAmount = errors.New("金额格式不正确")

// ParseAmountToCents converts one decimal yuan amount such as "19.90" into the
// integer number of cents (1990). An integer value such as "20" is accepted and
// means 2000 cents. Negative values, more than two fractional digits, an empty
// value and any non-digit input are rejected. The result is exact: no floating
// point arithmetic is used anywhere.
func ParseAmountToCents(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%w：金额不能为空", ErrInvalidAmount)
	}
	integerPart, fractionPart, hasFraction := strings.Cut(trimmed, ".")
	if integerPart == "" || !isDigits(integerPart) {
		return 0, fmt.Errorf("%w：金额必须是非负数字", ErrInvalidAmount)
	}
	if hasFraction {
		if fractionPart == "" || len(fractionPart) > 2 || !isDigits(fractionPart) {
			return 0, fmt.Errorf("%w：金额最多保留两位小数", ErrInvalidAmount)
		}
	}
	// An integer amount such as "20" means 20 yuan, so the fractional digits
	// are zero-filled to exactly two places before the cents are parsed.
	switch len(fractionPart) {
	case 0:
		fractionPart = "00"
	case 1:
		fractionPart += "0"
	}
	cents, err := strconv.ParseInt(integerPart+fractionPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w：金额超出范围", ErrInvalidAmount)
	}
	return cents, nil
}

// FormatCents renders an integer number of cents as a two-decimal yuan amount:
// 1990 becomes "19.90". Negative values keep their sign; callers normally never
// store negative amounts.
func FormatCents(cents int64) string {
	negative := cents < 0
	var magnitude uint64
	if negative {
		magnitude = uint64(-(cents + 1)) + 1
	} else {
		magnitude = uint64(cents)
	}
	formatted := fmt.Sprintf("%d.%02d", magnitude/100, magnitude%100)
	if negative {
		return "-" + formatted
	}
	return formatted
}

// isDigits reports whether value consists of ASCII digits only.
func isDigits(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
