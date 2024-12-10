package focalpoint

import (
	"encoding/base64"
	"strings"

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


func pubKeyToString(ppk ed25519.PublicKey) string{
	if(ppk == nil){
		return padTo44Characters("0")
	}
	return base64.StdEncoding.EncodeToString(ppk[:])
}

// pads the input string to the required Base64 length for ED25519 keys
func padTo44Characters(input string) string {
	// ED25519 keys are 32 bytes, which in Base64 is 44 characters including padding
	const base64Length = 44

	// If the input string is already longer than or equal to the base64Length, return the input
	if len(input) >= base64Length {
		return input
	}

	// Calculate the number of zeros needed
	padLength := base64Length - len(input) - 1

	// Pad the input with rendering zeros
	paddedString :=  input + strings.Repeat("0", padLength) + "="

	return paddedString
}