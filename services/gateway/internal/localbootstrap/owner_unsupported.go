//go:build !unix

package localbootstrap

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
