package changeset

import (
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func digestOf(value string) contracts.Digest { return DigestBytes([]byte(value)) }

func validEdits() []Edit {
	return []Edit{
		{
			Path: "src/go/add-00.go", Op: OpAdd, Lang: LanguageGo,
			AfterDigest: digestOf("package add\n"), NewContent: []byte("package add\n"),
		},
		{
			Path: "src/go/modify-00.go", Op: OpModify, Lang: LanguageGo,
			BeforeDigest: digestOf("before\n"), AfterDigest: digestOf("after\n"), NewContent: []byte("after\n"),
		},
		{
			Path: "src/go/delete-00.go", Op: OpDelete, Lang: LanguageGo,
			BeforeDigest: digestOf("gone\n"),
		},
		{
			Path: "src/go/renamed-00.go", OldPath: "src/go/rename-00.go", Op: OpRename, Lang: LanguageGo,
			BeforeDigest: digestOf("old\n"), AfterDigest: digestOf("new\n"), NewContent: []byte("new\n"),
		},
	}
}

func TestValidateAcceptsOneEditOfEachOperation(t *testing.T) {
	if err := Validate(validEdits()); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
}

func TestValidateRejectsEmptyAndOversizedSets(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Fatal("Validate(nil) = nil, want error")
	}
	edits := make([]Edit, 0, MaxEdits+1)
	for index := 0; index <= MaxEdits; index++ {
		edit := validEdits()[0]
		edit.Path = "src/go/add-" + string(rune('a'+index/26)) + string(rune('a'+index%26)) + ".go"
		edits = append(edits, edit)
	}
	if err := Validate(edits); err == nil {
		t.Fatalf("Validate(%d edits) = nil, want error", len(edits))
	}
}

func TestValidateEditRejectsUnsafePaths(t *testing.T) {
	bad := []string{
		"", "../escape.go", "/absolute.go", "a//b.go", "a/./b.go", "a/../b.go",
		"back\\slash.go", "ctrl\nchar.go", "trailing/", ".", "..",
		string(make([]byte, MaxPathBytes+1)),
	}
	for _, path := range bad {
		edit := validEdits()[0]
		edit.Path = path
		if err := ValidateEdit(edit); err == nil {
			t.Fatalf("ValidateEdit(path %q) = nil, want error", path)
		}
	}
	if err := ValidatePath("src/go/ok-00.go"); err != nil {
		t.Fatalf("ValidatePath(valid) = %v, want nil", err)
	}
}

func TestValidateEditEnforcesRenameShape(t *testing.T) {
	modify := validEdits()[1]
	modify.OldPath = "src/go/other.go"
	if err := ValidateEdit(modify); err == nil {
		t.Fatal("modify carrying old path accepted")
	}
	rename := validEdits()[3]
	rename.OldPath = ""
	if err := ValidateEdit(rename); err == nil {
		t.Fatal("rename without old path accepted")
	}
	rename = validEdits()[3]
	rename.OldPath = rename.Path
	if err := ValidateEdit(rename); err == nil {
		t.Fatal("rename with identical old and new path accepted")
	}
}

func TestValidateEditEnforcesOperationDigestShapes(t *testing.T) {
	add := validEdits()[0]
	add.BeforeDigest = digestOf("unexpected\n")
	if err := ValidateEdit(add); err == nil {
		t.Fatal("add carrying a before digest accepted")
	}
	add = validEdits()[0]
	add.AfterDigest = contracts.Digest{}
	if err := ValidateEdit(add); err == nil {
		t.Fatal("add without an after digest accepted")
	}
	deleteEdit := validEdits()[2]
	deleteEdit.AfterDigest = digestOf("unexpected\n")
	if err := ValidateEdit(deleteEdit); err == nil {
		t.Fatal("delete carrying an after digest accepted")
	}
	deleteEdit = validEdits()[2]
	deleteEdit.BeforeDigest = contracts.Digest{}
	if err := ValidateEdit(deleteEdit); err == nil {
		t.Fatal("delete without a before digest accepted")
	}
	modify := validEdits()[1]
	modify.BeforeDigest = contracts.Digest{Algorithm: "sha512", Hex: strings.Repeat("ab", 32)}
	if err := ValidateEdit(modify); err == nil {
		t.Fatal("modify with a non-sha256 before digest accepted")
	}
	modify = validEdits()[1]
	modify.AfterDigest = contracts.Digest{Algorithm: "sha256", Hex: strings.Repeat("zz", 32)}
	if err := ValidateEdit(modify); err == nil {
		t.Fatal("modify with a non-hex after digest accepted")
	}
}

func TestValidateEditRequiresAfterDigestToMatchBytes(t *testing.T) {
	edit := validEdits()[0]
	edit.NewContent = []byte("package other\n")
	if err := ValidateEdit(edit); err == nil {
		t.Fatal("post-image bytes not matching the after digest accepted")
	}
	deleteEdit := validEdits()[2]
	deleteEdit.NewContent = []byte("residue\n")
	if err := ValidateEdit(deleteEdit); err == nil {
		t.Fatal("delete carrying post-image bytes accepted")
	}
}

func TestValidateRejectsDuplicatePostImagePaths(t *testing.T) {
	edits := []Edit{validEdits()[0], validEdits()[0]}
	if err := Validate(edits); err == nil {
		t.Fatal("duplicate post-image paths accepted")
	}
}

func TestValidateRejectsDuplicatePreimagePaths(t *testing.T) {
	cases := map[string][]Edit{
		"rename source consumed by a second rename": {
			validEdits()[3],
			{
				Path: "src/go/renamed-01.go", OldPath: "src/go/rename-00.go", Op: OpRename, Lang: LanguageGo,
				BeforeDigest: digestOf("old\n"), AfterDigest: digestOf("newer\n"), NewContent: []byte("newer\n"),
			},
		},
		"rename source consumed by a modify": {
			validEdits()[3],
			{
				Path: "src/go/rename-00.go", Op: OpModify, Lang: LanguageGo,
				BeforeDigest: digestOf("old\n"), AfterDigest: digestOf("changed\n"), NewContent: []byte("changed\n"),
			},
		},
		"path deleted and modified": {
			validEdits()[2],
			{
				Path: "src/go/delete-00.go", Op: OpModify, Lang: LanguageGo,
				BeforeDigest: digestOf("gone\n"), AfterDigest: digestOf("changed\n"), NewContent: []byte("changed\n"),
			},
		},
	}
	for name, edits := range cases {
		if err := Validate(edits); err == nil {
			t.Fatalf("%s: accepted, want error", name)
		}
	}
}

func TestValidateEditEnforcesBoundedLanguageVocabulary(t *testing.T) {
	for _, language := range []Language{LanguageGo, LanguageTypeScript, LanguagePython, LanguageRust, LanguageJava} {
		edit := validEdits()[0]
		edit.Lang = language
		if err := ValidateEdit(edit); err != nil {
			t.Fatalf("ValidateEdit(language %q) = %v, want nil", language, err)
		}
	}
	edit := validEdits()[0]
	edit.Lang = "cobol"
	if err := ValidateEdit(edit); err == nil {
		t.Fatal("unknown language lane accepted")
	}
}
