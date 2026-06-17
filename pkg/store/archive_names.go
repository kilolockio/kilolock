package store

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

var archiveAdjectives = []string{
	"faulty",
	"silent",
	"brave",
	"gentle",
	"bright",
	"steady",
	"curious",
	"frozen",
	"lucky",
	"swift",
}

var archiveNouns = []string{
	"dragon",
	"river",
	"forest",
	"comet",
	"otter",
	"falcon",
	"lantern",
	"harbor",
	"cloud",
	"meadow",
}

func archivedLabelName(original string) string {
	original = strings.TrimSpace(original)
	if original == "" {
		original = "archived"
	}
	return original + "--archived-" + randomArchiveSuffix()
}

func archivedStateName(original string) string {
	original = strings.TrimSpace(original)
	if original == "" {
		return archivedLabelName(original)
	}
	lastSlash := strings.LastIndex(original, "/")
	if lastSlash < 0 {
		return archivedLabelName(original)
	}
	prefix := original[:lastSlash+1]
	label := original[lastSlash+1:]
	return prefix + archivedLabelName(label)
}

func randomArchiveSuffix() string {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "hidden-" + hex.EncodeToString([]byte("ig"))[:4]
	}
	adj := archiveAdjectives[int(raw[0])%len(archiveAdjectives)]
	noun := archiveNouns[int(raw[1])%len(archiveNouns)]
	return adj + "-" + noun + "-" + hex.EncodeToString(raw[2:])[:2]
}
