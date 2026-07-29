package gui

import "strings"

func compareVersionNumbers(left, right string) int {
	leftParts := numericVersionParts(left)
	rightParts := numericVersionParts(right)
	count := len(leftParts)
	if len(rightParts) > count {
		count = len(rightParts)
	}
	for index := 0; index < count; index++ {
		var leftValue, rightValue int
		if index < len(leftParts) {
			leftValue = leftParts[index]
		}
		if index < len(rightParts) {
			rightValue = rightParts[index]
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func numericVersionParts(version string) []int {
	fields := strings.FieldsFunc(strings.TrimSpace(version), func(character rune) bool {
		return character < '0' || character > '9'
	})
	parts := make([]int, 0, len(fields))
	for _, field := range fields {
		value := 0
		for _, character := range field {
			value = value*10 + int(character-'0')
		}
		parts = append(parts, value)
	}
	return parts
}
