//go:build !linux

package heavy

func setThreadNice(int) int { return 0 }
