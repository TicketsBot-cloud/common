package utils

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTicketIds(t *testing.T) {
	tests := []struct {
		input   string
		ids     []int
		invalid []string
	}{
		{input: "1, 2,3", ids: []int{1, 2, 3}},
		{input: "1;2\n3\t4 5\r\n6", ids: []int{1, 2, 3, 4, 5, 6}},
		{input: "3 3 1", ids: []int{3, 1}},
		{input: "007, 2147483647", ids: []int{7, 2147483647}},
		{input: "12, abc, 1-5, 0, -3, +4, 2147483648", ids: []int{12}, invalid: []string{"abc", "1-5", "0", "-3", "+4", "2147483648"}},
		{input: " ,; \n"},
		{input: ""},
	}

	for _, tc := range tests {
		ids, invalid := ParseTicketIds(tc.input)
		require.Equal(t, tc.ids, ids, tc.input)
		require.Equal(t, tc.invalid, invalid, tc.input)
	}
}
