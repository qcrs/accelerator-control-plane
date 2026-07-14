package controller

import (
	"fmt"
	"strings"
)

func childName(workloadName, suffix string) string {
	suffix = strings.Trim(suffix, "-")
	maximumBase := 63 - len(suffix) - 1
	if maximumBase < 1 {
		maximumBase = 1
	}
	base := strings.TrimRight(workloadName, "-")
	if len(base) > maximumBase {
		base = strings.TrimRight(base[:maximumBase], "-")
	}
	return base + "-" + suffix
}

func leaseName(workloadName string, attempt int32) string {
	return childName(workloadName, fmt.Sprintf("lease-%d", attempt))
}

func podName(workloadName, uid string) string {
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return childName(workloadName, uid)
}
