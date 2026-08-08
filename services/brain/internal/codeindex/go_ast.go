package codeindex

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
)

type tokenRange struct {
	start Position
	end   Position
}

// classifyGoAST uses only bounded top-level AST declarations. Reference
// spellings still come from the exact token stream because this syntax lane
// does not claim object resolution or compiler semantics.
func classifyGoAST(
	ctx context.Context,
	fileSet *token.FileSet,
	file *ast.File,
	tokens []sourceToken,
) ([]classifiedToken, error) {
	kinds := make([]Kind, len(tokens))
	tokenIndexes := make(map[tokenRange]int, len(tokens))
	for index, current := range tokens {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		tokenIndexes[tokenRange{start: current.start, end: current.end}] = index
	}
	remaining := len(tokens)
	for _, declaration := range file.Decls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if err := markGoNode(fileSet, typed.Name, KindDefinition, tokenIndexes, kinds, &remaining); err != nil {
				return nil, err
			}
		case *ast.GenDecl:
			for _, specification := range typed.Specs {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				switch spec := specification.(type) {
				case *ast.ImportSpec:
					if err := markGoNode(fileSet, spec.Path, KindImport, tokenIndexes, kinds, &remaining); err != nil {
						return nil, err
					}
					if spec.Name != nil {
						if err := markGoNode(fileSet, spec.Name, KindImport, tokenIndexes, kinds, &remaining); err != nil {
							return nil, err
						}
					}
				case *ast.TypeSpec:
					if err := markGoNode(fileSet, spec.Name, KindDefinition, tokenIndexes, kinds, &remaining); err != nil {
						return nil, err
					}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						if err := markGoNode(fileSet, name, KindDefinition, tokenIndexes, kinds, &remaining); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	result := make([]classifiedToken, 0, len(tokens))
	for index, current := range tokens {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		kind := kinds[index]
		if kind == "" {
			if current.kind != tokenIdentifier || isKeyword(LanguageGo, current.text) {
				continue
			}
			kind = KindReference
		}
		result = append(result, classifiedToken{kind: kind, token: current})
	}
	return result, nil
}

func markGoNode(
	fileSet *token.FileSet,
	node ast.Node,
	kind Kind,
	tokenIndexes map[tokenRange]int,
	kinds []Kind,
	remaining *int,
) error {
	if *remaining <= 0 {
		return fmt.Errorf("%w: Go AST nodes", ErrLimitExceeded)
	}
	*remaining = *remaining - 1
	start := fileSet.PositionFor(node.Pos(), false)
	end := fileSet.PositionFor(node.End(), false)
	key := tokenRange{
		start: Position{Line: uint32(start.Line), Column: uint32(start.Column)},
		end:   Position{Line: uint32(end.Line), Column: uint32(end.Column)},
	}
	if index, ok := tokenIndexes[key]; ok {
		kinds[index] = kind
	}
	return nil
}
