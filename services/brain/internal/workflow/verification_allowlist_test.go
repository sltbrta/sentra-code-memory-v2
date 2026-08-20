package workflow

import "testing"

// TestVerificationRefusesInterpretersAndTaskRunners closes a fresh-eyes
// finding: removing `/bin/sh -c` removed one syntax for reaching a shell, not
// the capability. `make` runs recipes through a shell, `npm run` runs
// package.json scripts through one, and `node`/`python`/`ruby`/`go run` execute
// a file the change set may have just written into the stage -- which is
// change-set-controlled by construction, since that is what is being verified.
func TestVerificationRefusesInterpretersAndTaskRunners(t *testing.T) {
	for _, command := range []string{
		"make",
		"make -f Makefile all",
		"just build",
		"npm run build",
		"pnpm test",
		"yarn verify",
		"node --require ./pwn.js index.js",
		"node index.js",
		"python setup.py test",
		"python3 -c print(1)",
		"ruby -e puts",
		"rake default",
		"bundle exec rake",
		"go run ./cmd/x",
		"go generate ./...",
		"go install ./...",
		"go test -exec node ./...",
		"go test -exec=node ./...",
	} {
		t.Run(command, func(t *testing.T) {
			if _, err := parseVerificationCommand(command); err == nil {
				t.Fatalf("%q was accepted; it reaches a shell or runs change-set code", command)
			}
		})
	}
}

// TestVerificationStillAdmitsTheToolsAGateNeeds keeps the narrowing from
// making the gate useless.
func TestVerificationStillAdmitsTheToolsAGateNeeds(t *testing.T) {
	for _, command := range []string{
		"go test ./...",
		"go vet ./...",
		"go build ./...",
		"gofmt -l .",
		"grep -q needle file.txt",
		"test -f file.txt",
		"false",
	} {
		t.Run(command, func(t *testing.T) {
			if _, err := parseVerificationCommand(command); err != nil {
				t.Fatalf("%q was refused: %v", command, err)
			}
		})
	}
}
