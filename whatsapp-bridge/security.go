package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// maxRequestBody caps JSON request bodies of the write endpoints.
const maxRequestBody = 64 << 10

// maxMediaBytes caps the size of an attachment read for /api/send (a var so tests can lower it).
var maxMediaBytes int64 = 100 << 20

var errMediaTooLarge = errors.New("media file exceeds size limit")

// chatJIDPattern restricts chat JIDs to the characters WhatsApp actually uses.
var chatJIDPattern = regexp.MustCompile(`^[0-9A-Za-z._:@+-]+$`)

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

// messageDirName maps a sender-chosen message ID to a fixed-alphabet directory name. Distinct IDs
// never share a directory, unlike any character-replacement scheme.
func messageDirName(messageID string) string {
	sum := sha256.Sum256([]byte(messageID))
	return hex.EncodeToString(sum[:])
}

// fileMatchesSHA256 reports whether the file at path exists and hashes to want.
func fileMatchesSHA256(path string, want []byte) bool {
	if len(want) == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), want)
}

// validateMediaDir rejects a WA_MEDIA_DIR that is, contains or lies inside storeDir.
func validateMediaDir(mediaDir, storeDir string) error {
	if mediaDir == "" {
		return nil
	}
	media, err := realOrAbs(mediaDir)
	if err != nil {
		return err
	}
	store, err := realOrAbs(storeDir)
	if err != nil {
		return err
	}
	if isWithin(media, store) || isWithin(store, media) {
		return fmt.Errorf("%s overlaps the session store %s", media, store)
	}
	return nil
}

func realOrAbs(p string) (string, error) {
	if r, err := realAbs(p); err == nil {
		return r, nil
	}
	return filepath.Abs(p)
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

// storeMediaPath builds the directory and file path for a message's media under storeDir and
// verifies the result is exactly storeDir/<chat>/<sha256(id)>/<file>, never anything else.
func storeMediaPath(storeDir, chatJID, messageID, filename string) (string, string, error) {
	if !validChatJID(chatJID) || messageID == "" || filename == "" || safeFilename(filename) != filename {
		return "", "", errInvalidPath
	}
	msgDir := filepath.Join(storeDir, strings.ReplaceAll(chatJID, ":", "_"), messageDirName(messageID))
	localPath := filepath.Clean(filepath.Join(msgDir, filename))
	rel, err := filepath.Rel(filepath.Clean(storeDir), localPath)
	if err != nil {
		return "", "", errInvalidPath
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] == "." || parts[0] == ".." || parts[2] == ".." {
		return "", "", errInvalidPath
	}
	return msgDir, localPath, nil
}

// resolveMediaPath enforces WA_MEDIA_DIR for /api/send. With mediaDir empty it fails closed.
// The fully resolved path must be a regular file inside mediaDir and never inside storeDir.
func resolveMediaPath(mediaPath, mediaDir, storeDir string) (string, error) {
	if mediaDir == "" || mediaPath == "" || strings.ContainsRune(mediaPath, 0) {
		return "", errInvalidPath
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

// readMediaFile validates mediaPath with resolveMediaPath and then reads it through an os.Root
// anchored at mediaDir with O_NOFOLLOW, re-checking the opened descriptor, so a path swapped
// after validation cannot make it read anything outside mediaDir.
func readMediaFile(mediaPath, mediaDir, storeDir string) ([]byte, string, error) {
	realPath, err := resolveMediaPath(mediaPath, mediaDir, storeDir)
	if err != nil {
		return nil, "", err
	}
	realBase, err := realAbs(mediaDir)
	if err != nil {
		return nil, "", errInvalidPath
	}
	rel, err := filepath.Rel(realBase, realPath)
	if err != nil {
		return nil, "", errInvalidPath
	}
	root, err := os.OpenRoot(realBase)
	if err != nil {
		return nil, "", errInvalidPath
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", errInvalidPath
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, "", errInvalidPath
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err != nil {
		return nil, "", errInvalidPath
	}
	if int64(len(data)) > maxMediaBytes {
		return nil, "", errMediaTooLarge
	}
	return data, realPath, nil
}
