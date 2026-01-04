package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/opencontainers/go-digest"
)

const chainIDPrefix = "sha256:"

func GetVersion() string {
	return "v1.0.0"
}

func GetBuildInfo() string {
	return "Conch build ID"
}

func CalculateSnapshotName(namespace, key, parent string) (string, error) {
	if parent == "" {
		dgst := digest.FromString(fmt.Sprintf("%s/%s", namespace, key))
		return dgst.String(), nil
	}

	diffID := digest.FromString(fmt.Sprintf("%s/%s", namespace, key))
	chainID := calculateChainID(parent, diffID.String())
	return chainID, nil
}

func calculateChainID(parentChainID, diffID string) string {
	data := parentChainID + " " + diffID
	hash := sha256.Sum256([]byte(data))
	return chainIDPrefix + hex.EncodeToString(hash[:])
}
