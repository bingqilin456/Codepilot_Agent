package boundary

// Contains 判断 n 是否属于闭区间 [min, max]，前提是 min <= max。
func Contains(n, min, max int) bool {
	return n >= min && n < max
}
