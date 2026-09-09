package userpanelasset

import (
	"fmt"
	"strconv"
	"strings"
)

type semanticVersion struct {
	major int64
	minor int64
	patch int64
	pre   []string
}

func parseSemver(raw string) (*semanticVersion, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "v")
	value = strings.TrimPrefix(value, "V")
	if value == "" {
		return nil, fmt.Errorf("version is empty")
	}
	withoutBuild := strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 2 && len(core) != 3 {
		return nil, fmt.Errorf("%q is not semantic version", raw)
	}
	values := make([]int64, 3)
	for i, component := range core {
		if component == "" || (len(component) > 1 && component[0] == '0') {
			return nil, fmt.Errorf("%q is not semantic version", raw)
		}
		parsed, errParse := strconv.ParseInt(component, 10, 64)
		if errParse != nil || parsed < 0 {
			return nil, fmt.Errorf("%q is not semantic version", raw)
		}
		values[i] = parsed
	}
	version := &semanticVersion{major: values[0], minor: values[1], patch: values[2]}
	if len(parts) == 2 {
		if parts[1] == "" {
			return nil, fmt.Errorf("%q has empty prerelease", raw)
		}
		for _, identifier := range strings.Split(parts[1], ".") {
			if identifier == "" || !validIdentifier(identifier) {
				return nil, fmt.Errorf("%q has invalid prerelease", raw)
			}
			version.pre = append(version.pre, identifier)
		}
	}
	return version, nil
}

func validIdentifier(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
			return false
		}
	}
	return true
}

func compareSemver(left, right *semanticVersion) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	for _, values := range [][2]int64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if values[0] < values[1] {
			return -1
		}
		if values[0] > values[1] {
			return 1
		}
	}
	if len(left.pre) == 0 && len(right.pre) == 0 {
		return 0
	}
	if len(left.pre) == 0 {
		return 1
	}
	if len(right.pre) == 0 {
		return -1
	}
	for i := 0; i < len(left.pre) && i < len(right.pre); i++ {
		leftID, rightID := left.pre[i], right.pre[i]
		leftNumeric := numericIdentifier(leftID)
		rightNumeric := numericIdentifier(rightID)
		switch {
		case leftNumeric && rightNumeric:
			leftValue, _ := strconv.ParseInt(leftID, 10, 64)
			rightValue, _ := strconv.ParseInt(rightID, 10, 64)
			if leftValue < rightValue {
				return -1
			}
			if leftValue > rightValue {
				return 1
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftID < rightID:
			return -1
		case leftID > rightID:
			return 1
		}
	}
	if len(left.pre) < len(right.pre) {
		return -1
	}
	if len(left.pre) > len(right.pre) {
		return 1
	}
	return 0
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
}

func compareSemverString(left, right string) int {
	leftVersion, leftErr := parseSemver(left)
	rightVersion, rightErr := parseSemver(right)
	if leftErr != nil && rightErr != nil {
		return strings.Compare(strings.TrimSpace(left), strings.TrimSpace(right))
	}
	if leftErr != nil {
		return -1
	}
	if rightErr != nil {
		return 1
	}
	return compareSemver(leftVersion, rightVersion)
}

func selectRelease(releases []ReleaseInfo, pinned string) (ReleaseInfo, error) {
	pinned = strings.TrimSpace(pinned)
	var pinnedVersion *semanticVersion
	if pinned != "" {
		var errPinned error
		pinnedVersion, errPinned = parseSemver(pinned)
		if errPinned != nil {
			return ReleaseInfo{}, fmt.Errorf("invalid pinned user panel version: %w", errPinned)
		}
	}
	var selected ReleaseInfo
	var selectedVersion *semanticVersion
	found := false
	for _, release := range releases {
		if release.Draft {
			continue
		}
		version, errVersion := parseSemver(release.TagName)
		if errVersion != nil {
			continue
		}
		if pinnedVersion != nil {
			if compareSemver(version, pinnedVersion) != 0 {
				continue
			}
			return release, nil
		}
		// Stable releases are preferred over prereleases. If no stable release
		// exists, the highest prerelease is still a usable fallback.
		if !found || (!release.Prerelease && selected.Prerelease) ||
			(release.Prerelease == selected.Prerelease && compareSemver(version, selectedVersion) > 0) {
			selected = release
			selectedVersion = version
			found = true
		}
	}
	if !found {
		return ReleaseInfo{}, ErrNoRelease
	}
	return selected, nil
}
