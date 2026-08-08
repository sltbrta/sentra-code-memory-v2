//go:build darwin || linux

// This file owns the process-lifetime advisory lock for one local authority.
// The lock sidecar is derived only from the canonical absolute SQLite path so
// independent processes cannot accidentally become concurrent writers.
package localstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type authorityOwner struct {
	file *os.File
}

func acquireAuthorityOwner(databasePath string) (*authorityOwner, string, error) {
	canonicalPath, err := canonicalAuthorityPath(databasePath)
	if err != nil {
		return nil, "", err
	}
	file, err := os.OpenFile(
		canonicalPath+".owner.lock",
		os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, "", fmt.Errorf("localstate: open authority owner lock: %w", err)
	}
	closeOnError := func(lockErr error) (*authorityOwner, string, error) {
		return nil, "", errors.Join(lockErr, file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("localstate: inspect authority owner lock: %w", err))
	}
	if !info.Mode().IsRegular() {
		return closeOnError(ErrInvalidInput)
	}
	if err := file.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("localstate: secure authority owner lock: %w", err))
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeOnError(ErrAuthorityOwned)
		}
		return closeOnError(fmt.Errorf("localstate: acquire authority owner lock: %w", err))
	}
	return &authorityOwner{file: file}, canonicalPath, nil
}

func canonicalAuthorityPath(databasePath string) (string, error) {
	cleanPath := filepath.Clean(databasePath)
	if !filepath.IsAbs(cleanPath) {
		return "", ErrInvalidInput
	}
	resolvedPath, err := filepath.EvalSymlinks(cleanPath)
	if err == nil {
		info, statErr := os.Stat(resolvedPath)
		if statErr != nil {
			return "", fmt.Errorf("localstate: inspect authority path: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return "", ErrInvalidInput
		}
		return resolvedPath, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("localstate: resolve authority path: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(cleanPath))
	if err != nil {
		return "", fmt.Errorf("localstate: resolve authority parent: %w", err)
	}
	return filepath.Join(resolvedParent, filepath.Base(cleanPath)), nil
}

func (owner *authorityOwner) close() error {
	if owner == nil || owner.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(owner.file.Fd()), syscall.LOCK_UN)
	closeErr := owner.file.Close()
	owner.file = nil
	return errors.Join(unlockErr, closeErr)
}
