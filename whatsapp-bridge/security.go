package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// chatJIDPattern restricts chat JIDs to the characters WhatsApp actually uses.
var chatJIDPattern = regexp.MustCompile(`^[0-9A-Za-z._:@+-]+$`)

// unsafeNameChars matches anything we do not want in a fallback filename.
var unsafeNameChars = regexp.MustCompile(`[^0-9A-Za-z_-]`)

// errInvalidPath is returned for any path that fails validation. It is deliberately generic.
var errInvalidPath = errors.New("invalid path")

// validChatJID reports whether a client-supplied chat JID is safe to use in queries and paths.
func validChatJID(jid string) bool {
	return jid != "" && len(jid) <= 256 && chatJIDPattern.MatchString(jid) && !strings.Contains(jid, "..")
}

// safeFilename reduces a sender-controlled filename to a single safe path component.
// It returns "" when nothing usable remains; callers must then pick a fallback name.
func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	base := filepath.Base(name)
	if base == "" || base == "." || base == ".." || base == "/" ||
		strings.ContainsAny(base, "/\\\x00") {
		return ""
	}
	return base
}

// fallbackFilename derives a safe filename from a message ID.
func fallbackFilename(messageID string) string {
	return "media_" + unsafeNameChars.ReplaceAllString(messageID, "_")
}

// isWithin reports whether target is base itself or lies below it. Both must be clean absolute
// or both clean relative paths.
func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// storeMediaPath builds the directory and file path for a chat's media under storeDir and
// verifies the result is exactly storeDir/<chat>/<file>, never anything else inside or outside it.
func storeMediaPath(storeDir, chatJID, filename string) (string, string, error) {
	if !validChatJID(chatJID) || safeFilename(filename) != filename {
		return "", "", errInvalidPath
	}
	chatDir := filepath.Join(storeDir, strings.ReplaceAll(chatJID, ":", "_"))
	localPath := filepath.Clean(filepath.Join(chatDir, filename))
	rel, err := filepath.Rel(filepath.Clean(storeDir), localPath)
	if err != nil {
		return "", "", errInvalidPath
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == ".." {
		return "", "", errInvalidPath
	}
	return chatDir, localPath, nil
}

// resolveMediaPath enforces WA_MEDIA_DIR for /api/send. With mediaDir empty the legacy behaviour
// (any path) is kept. Otherwise the fully resolved path must be a regular file inside mediaDir
// and outside storeDir; the resolved path is returned so the caller reads what was checked.
func resolveMediaPath(mediaPath, mediaDir, storeDir string) (string, error) {
	if mediaDir == "" {
		return mediaPath, nil
	}
	realPath, err := realAbs(mediaPath)
	if err != nil {
		return "", errInvalidPath
	}
	realBase, err := realAbs(mediaDir)
	if err != nil || !isWithin(realBase, realPath) || realPath == realBase {
		return "", errInvalidPath
	}
	if realStore, err := realAbs(storeDir); err == nil && isWithin(realStore, realPath) {
		return "", errInvalidPath
	}
	if info, err := os.Stat(realPath); err != nil || !info.Mode().IsRegular() {
		return "", errInvalidPath
	}
	return realPath, nil
}

func realAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
