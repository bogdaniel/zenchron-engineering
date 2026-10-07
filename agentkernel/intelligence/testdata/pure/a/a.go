package a

func leaf() int { return 1 }

// Mid calls leaf statically.
func Mid() int { return leaf() }
