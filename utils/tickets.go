package utils

import (
	"strconv"
	"strings"
)

func ParseTicketIds(input string) (ids []int, invalid []string) {
	tokens := strings.FieldsFunc(input, func(r rune) bool {
		return strings.ContainsRune(",; \n\r\t", r)
	})

	seen := make(map[int]bool)
	for _, token := range tokens {
		// Bit size 31 matches the int4 tickets.id column.
		id, err := strconv.ParseUint(token, 10, 31)
		if err != nil || id == 0 {
			invalid = append(invalid, token)
			continue
		}

		if !seen[int(id)] {
			ids = append(ids, int(id))
			seen[int(id)] = true
		}
	}

	return ids, invalid
}
