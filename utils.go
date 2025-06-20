package focalpoint

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"time"

	"golang.org/x/crypto/ed25519"
)

// OrderedHashSet is a deduplicated collection of strings with preserved insertion order
type OrderedHashSet struct {
    set    map[string]struct{}
    values []string
}

// NewOrderedHashSet creates and returns a new OrderedHashSet
func NewOrderedHashSet() *OrderedHashSet {
    return &OrderedHashSet{
        set:    make(map[string]struct{}),
        values: []string{},
    }
}

// Add inserts a string into the OrderedHashSet (if not already present)
func (ohs *OrderedHashSet) Add(value string) {
    if _, exists := ohs.set[value]; !exists {
        ohs.set[value] = struct{}{}
        ohs.values = append(ohs.values, value)
    }
}

// Remove deletes a string from the OrderedHashSet
func (ohs *OrderedHashSet) Remove(value string) {
    if _, exists := ohs.set[value]; exists {
        delete(ohs.set, value)
        // Rebuild the values slice to maintain order
        for i, v := range ohs.values {
            if v == value {
                ohs.values = append(ohs.values[:i], ohs.values[i+1:]...)
                break
            }
        }
    }
}

// Contains checks if a string is in the OrderedHashSet
func (ohs *OrderedHashSet) Contains(value string) bool {
    _, exists := ohs.set[value]
    return exists
}

// Size returns the number of elements in the OrderedHashSet
func (ohs *OrderedHashSet) Size() int {
    return len(ohs.set)
}

// Values returns a slice of all elements in insertion order
func (ohs *OrderedHashSet) Values() []string {
    return ohs.values
}

func DiminishingOrders(n int64) []int64 {
	// Special-case zero.
	if n == 0 {
		return []int64{0}
	}
	// Determine the number of digits.
	digits := int(math.Log10(float64(n))) + 1

	results := []int64{n}
	// For each power of 10 from 10^1 up to 10^(digits)
	for i := 0; i < digits; i++ {
		power := int64(math.Pow(10, float64(i+1)))
		rounded := n - (n % power)
		// Append only if it's a new value
		if rounded != results[len(results)-1] {
			results = append(results, rounded)
		}
	}
	return results
}

func reverse(s []string) []string {
	result := make([]string, len(s))
	for i, v := range s {
		result[len(s)-1-i] = v
	}
	return result
}

func pluralize(value int, unit string) string {
	if value == 1 {
		return fmt.Sprintf("%d %s ago", value, unit)
	}
	return fmt.Sprintf("%d %ss ago", value, unit)
}

func timeAgo(unixTimestamp int64) string {
	t := time.Unix(unixTimestamp, 0)
	now := time.Now()
	duration := now.Sub(t)

	switch {
	case duration < time.Minute:
		return "just now"
	case duration < time.Hour:
		minutes := int(duration.Minutes())
		return pluralize(minutes, "minute")
	case duration < 24*time.Hour:
		hours := int(duration.Hours())
		return pluralize(hours, "hour")
	case duration < 48*time.Hour:
		return "yesterday"
	case duration < 30*24*time.Hour:
		days := int(duration.Hours() / 24)
		return pluralize(days, "day")
	case duration < 12*30*24*time.Hour:
		months := int(duration.Hours() / (24 * 30))
		return pluralize(months, "month")
	default:
		years := int(duration.Hours() / (24 * 365))
		return pluralize(years, "year")
	}
}

// Returns a slice of strings where each element is the
// original string shortened by 2 characters from the end recursively.
func inflateLocale(s string) []string {
	// Base case: if the string is 2 characters, return a slice with just that string.
	if len(s) == 2 {
		return []string{s}
	}
	// Recursive case: append the current string to the result of the recursive call.
	return append([]string{s}, inflateLocale(s[:len(s)-2])...)
}

func pubKeyToString(ppk ed25519.PublicKey) string{
	if(ppk == nil){
		return pad44("0")
	}
	return base64.StdEncoding.EncodeToString(ppk[:])
}

// pads the input string to the required Base64 length for ED25519 keys
func pad44(input string) string {
	// ED25519 keys are 32 bytes, which in Base64 is 44 characters including padding
	const base64Length = 44

	// If the input string is already longer than or equal to the base64Length, return the input
	if len(input) >= base64Length {
		return input
	}

    reInput := ""
    if input != "0" {
		reInput = input + "/"
	}

	// Calculate the number of zeros needed
	padLength := base64Length - len(reInput) - 1

	// Pad the input with rendering zeros
	paddedString :=  reInput + strings.Repeat("0", padLength) + "="

	return paddedString
}