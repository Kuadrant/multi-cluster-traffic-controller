package slice

// RemoveString returns a newly created []string that contains all items from slice that
// are not equal to s.
func RemoveString(slice []string, s string) []string {
	newSlice := make([]string, 0)
	for _, item := range slice {
		if item == s {
			continue
		}
		newSlice = append(newSlice, item)
	}
	if len(newSlice) == 0 {
		// Sanitize for unit tests so we don't need to distinguish empty array
		// and nil.
		newSlice = nil
	}
	return newSlice
}

// ContainsString checks if a given slice of strings contains the provided string.
func ContainsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// Find checks if an element in slice satisfies the given predicate, and returns
// it. If no element is found returns false
func Find[T any](slice []T, predicate func(T) bool) (element T, ok bool) {
	for _, elem := range slice {
		if predicate(elem) {
			element = elem
			ok = true
			return
		}
	}

	return
}

func Filter[T any](slice []T, predicate func(T) bool) []T {
	result := []T{}
	for _, elem := range slice {
		if predicate(elem) {
			result = append(result, elem)
		}
	}

	return result
}
