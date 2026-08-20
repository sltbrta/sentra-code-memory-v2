package authorityprocess

import "testing"

// The DOCS gate asked whether a file contained the two characters "//"
// anywhere. Any file with a single inline comment satisfied that, so a change
// set reached FACTORY_GATE_STATUS_PASSED with "documentation" proven by a
// stray `// TODO`. Callers read that status as an assurance.

func TestExportedDeclarationsAreDocumented(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{
			name:   "documented function",
			source: "package p\n\n// Alpha does a thing.\nfunc Alpha() {}\n",
			want:   true,
		},
		{
			name:   "undocumented exported function",
			source: "package p\n\nfunc Alpha() {}\n",
			want:   false,
		},
		{
			name: "inline comment is not documentation",
			// This is exactly what the old gate accepted.
			source: "package p\n\nfunc Alpha() {\n\t// TODO: later\n}\n",
			want:   false,
		},
		{
			name:   "unexported needs nothing",
			source: "package p\n\nfunc alpha() {}\n",
			want:   true,
		},
		{
			name:   "documented type",
			source: "package p\n\n// Thing is a thing.\ntype Thing struct{}\n",
			want:   true,
		},
		{
			name:   "undocumented exported type",
			source: "package p\n\ntype Thing struct{}\n",
			want:   false,
		},
		{
			name:   "grouped declaration documented once",
			source: "package p\n\n// Colours are the colours.\nconst (\n\tRed = 1\n\tBlue = 2\n)\n",
			want:   true,
		},
		{
			name:   "grouped declaration undocumented",
			source: "package p\n\nconst (\n\tRed = 1\n\tBlue = 2\n)\n",
			want:   false,
		},
		{
			name:   "unexported group needs nothing",
			source: "package p\n\nconst (\n\tred = 1\n)\n",
			want:   true,
		},
		{
			name:   "no exported declarations passes vacuously",
			source: "package p\n",
			want:   true,
		},
		{
			name:   "unparseable is left to the TEST gate",
			source: "package p\n\nfunc Alpha( {\n",
			want:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := exportedDeclarationsAreDocumented("x.go", []byte(test.source))
			if got != test.want {
				t.Fatalf("exportedDeclarationsAreDocumented(%q) = %v, want %v", test.source, got, test.want)
			}
		})
	}
}
