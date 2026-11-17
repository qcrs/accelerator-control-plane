package controller

import (
	"crypto/sha256"
	"encoding/hex"
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

func leaseName(workloadName, uid string, attempt int32) string {
	return childName(workloadName, fmt.Sprintf("%s-lease-%d", identitySuffix(workloadName, uid), attempt))
}

func podName(workloadName, uid string) string {
	return childName(workloadName, identitySuffix(workloadName, uid))
}

func identitySuffix(workloadName, uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	sum := sha256.Sum256([]byte(workloadName + "\x00" + uid))
	return hex.EncodeToString(sum[:4])
}
