package chunkindex

import (
	"crypto/sha256"
	"encoding/hex"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Prefixes of the two ID kinds.
const (
	DocumentPrefix = "lv_doc_"
	ChunkPrefix    = "lv_chk_"
)

// digestLength is the hex characters kept from each SHA-256 digest.
const digestLength = 32

// DocumentID is ChunkIDs.documentID: stable across reindexing and content
// changes, distinct across tenants. It hashes Foundation's uuidString, which is
// UPPERCASE, so hashing Go's lowercase form would give every document a second
// identity while both servers run.
func DocumentID(tenant uuid.UUID, path string) string {
	return DocumentPrefix + digest(swiftUUIDString(tenant)+"\x00"+NormalizePath(path))
}

// ChunkID is ChunkIDs.chunkID: changes whenever the chunk's content or position
// changes.
func ChunkID(documentID string, ordinal int, contentSHA256 string) string {
	return ChunkPrefix + digest(documentID+"\x00"+strconv.Itoa(ordinal)+"\x00"+contentSHA256)
}

// NormalizePath is ChunkIDs.normalize: forward slashes and no leading "./" or
// "/", so the same file hashes the same however the caller spelled it. The
// prefix tests compare Characters, as Swift's hasPrefix does: "./́x" keeps
// its dot.
func NormalizePath(path string) string {
	normalized := swiftstr.ReplaceBackslashes(path)
	for swiftstr.HasASCIIPrefix(normalized, "./") {
		normalized = normalized[2:]
	}
	for swiftstr.HasASCIIPrefix(normalized, "/") {
		normalized = normalized[1:]
	}
	return normalized
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// SHA256Hex is MarkdownChunker.sha256Hex: the lowercase hex SHA-256 of text.
func SHA256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// swiftUUIDString is Foundation's UUID.uuidString.
func swiftUUIDString(id uuid.UUID) string {
	return strings.ToUpper(id.String())
}
