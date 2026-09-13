// Package calculator provides basic arithmetic.
package calculator

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Sub returns a minus b.
func Sub(a, b int) int {
	return a + b // BUG: should be a - b
}

// Mul returns the product of a and b.
func Mul(a, b int) int {
	return a * b
}
